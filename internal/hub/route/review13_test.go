package route

import (
	"database/sql"
	"encoding/json"
	"github.com/gorilla/websocket"
	"strings"
	"termhub/internal/hub/auth"
	"testing"
	"time"

	"termhub/internal/proto"
)

func TestReview13ForeignNodeNotifications(t *testing.T) {
	for _, kind := range []string{proto.MsgTitle, proto.MsgSessionExited} {
		for _, routed := range []bool{false, true} {
			t.Run(kind+map[bool]string{false: "/stored", true: "/routed"}[routed], func(t *testing.T) {
				f := newFixture(t)
				sid := proto.NewSID()
				_, err := f.db.Exec(`INSERT INTO sessions(sid,node_id,owner_id,created_at,title) VALUES (?,2,1,1,'original')`, sid.String())
				if err != nil {
					t.Fatal(err)
				}
				n := &nodeConn{hub: f.hub, node: Node{ID: 1}}
				f.hub.nodes[1] = n
				if routed {
					f.hub.ensureRoute(sid, 2, 80, 24)
				}
				n.handleMsg(&proto.Msg{T: kind, SID: &sid, Title: "forged", Reason: "self"})
				var title string
				var ended sql.NullInt64
				if err = f.db.QueryRow(`SELECT title,ended_at FROM sessions WHERE sid=?`, sid.String()).Scan(&title, &ended); err != nil {
					t.Fatal(err)
				}
				if title != "original" || ended.Valid {
					t.Fatalf("foreign notification changed row: title=%q ended=%v", title, ended)
				}
				if routed && f.hub.route(sid) == nil {
					t.Fatal("foreign notification dropped route")
				}
			})
		}
	}
}

func TestReview13BadFilePages(t *testing.T) {
	f := newFixture(t)
	admin := f.login("root", pw, true)
	for _, page := range []string{"-1", "1000001", "9223372036854776", "nonsense"} {
		status, _ := admin.call("GET", "/api/nodes/1/fs/list?path=C:%5C&page="+page, nil)
		if status != 400 {
			t.Errorf("page %q status %d want 400", page, status)
		}
	}
}

func reviewNode(f *fixture) *nodeConn {
	n := &nodeConn{hub: f.hub, node: Node{ID: 1}, pending: map[uint64]chan *proto.Msg{}, ws: &wsConn{out: make(chan wsMsg, 20), done: make(chan struct{}), limit: 1 << 20}}
	f.hub.nodes[1] = n
	return n
}
func TestReview13ReconcileUnknownAndAdoptKind(t *testing.T) {
	f := newFixture(t)
	n := reviewNode(f)
	sid := proto.NewSID()
	f.db.Exec(`INSERT INTO sessions(sid,node_id,owner_id,profile_name,cwd,kind,created_at,end_reason) VALUES (?,1,1,'original','C:\work','claude',1,'start_unknown')`, sid.String())
	f.hub.reconcile(n, []proto.SessionInfo{{SID: sid, State: "running", Kind: "codex", Owner: "root"}})
	row, e := f.reg.session(sid.String())
	if e != nil || row.EndedAt != 0 || row.EndReason != "" || row.Kind != "claude" || row.ProfileName != "original" {
		t.Fatalf("not recovered: %+v %v", row, e)
	}
	adopted := proto.NewSID()
	f.hub.reconcile(n, []proto.SessionInfo{{SID: sid, State: "running"}, {SID: adopted, State: "running", Kind: "codex", Owner: "root"}})
	row, e = f.reg.session(adopted.String())
	if e != nil || row.Kind != "codex" {
		t.Fatalf("lost adopted kind: %+v %v", row, e)
	}
	f.db.Exec(`UPDATE sessions SET ended_at=2,end_reason='user' WHERE sid=?`, sid.String())
	f.hub.reconcile(n, []proto.SessionInfo{{SID: sid, State: "running"}})
	row, _ = f.reg.session(sid.String())
	if row.EndedAt != 2 {
		t.Fatal("resurrected explicitly ended session")
	}
}
func TestReview13LateCreateFailureAndInterleavedList(t *testing.T) {
	f := newFixture(t)
	n := reviewNode(f)
	sid := proto.NewSID()
	f.db.Exec(`INSERT INTO sessions(sid,node_id,owner_id,created_at,end_reason) VALUES (?,1,1,1,'starting')`, sid.String())
	m := &proto.Msg{T: proto.MsgCreate, SID: &sid}
	done := make(chan error, 1)
	go func() { _, e := n.request(m, 20*time.Millisecond); done <- e }()
	<-n.ws.out
	f.hub.reconcile(n, nil)
	row, _ := f.reg.session(sid.String())
	if row.EndedAt != 0 {
		t.Fatal("in-flight start lost to stale list")
	}
	if e := <-done; e == nil {
		t.Fatal("expected timeout")
	}
	f.db.Exec(`UPDATE sessions SET end_reason='start_unknown' WHERE sid=?`, sid.String())
	n.handleMsg(m.Fail("spawn_failed", "test"))
	row, _ = f.reg.session(sid.String())
	if row.EndedAt == 0 || row.EndReason != "start_failed" {
		t.Fatalf("late error ignored: %+v", row)
	}
}
func TestReview13InputErrorAndBatch(t *testing.T) {
	f := newFixture(t)
	n := reviewNode(f)
	sid := proto.NewSID()
	f.db.Exec(`INSERT INTO sessions(sid,node_id,owner_id,created_at) VALUES (?,1,1,1)`, sid.String())
	rt := f.hub.ensureRoute(sid, 1, 80, 24)
	rt.owner = 1
	a := &attachment{rt: rt, id: &auth.Identity{User: auth.User{ID: 1}}, ws: &wsConn{out: make(chan wsMsg, 20), done: make(chan struct{}), limit: 1 << 20}}
	rt.atts[a] = struct{}{}
	n.handleMsg(&proto.Msg{T: proto.MsgErr, SID: &sid, Code: proto.ErrInputOverflow})
	select {
	case out := <-a.ws.out:
		m, e := proto.DecodeMsg(out.data)
		if e != nil || m.Code != proto.ErrInputOverflow {
			t.Fatalf("bad error %s", out.data)
		}
	default:
		t.Fatal("host input rejection lost")
	}
	rt.taker = 2
	if code := a.queueInput([]byte("x")); code != proto.ErrReadOnly {
		t.Fatalf("accepted foreign input: %s", code)
	}
	if len(n.ws.out) != 0 {
		t.Fatal("rejected input reached node")
	}
	rt.taker = 0
	if code := a.queueInput([]byte("x")); code != "" {
		t.Fatal(code)
	}
	if (<-n.ws.out).kind != websocket.BinaryMessage {
		t.Fatal("missing input")
	}
}
func TestReview13QuotaReservation(t *testing.T) {
	f := newFixture(t)
	admin := f.login("root", pw, true)
	_, pid := f.nodeWithProfile(admin, "pc1")
	n := reviewNode(f)
	f.hub.limits.PerUser = 1
	id := &auth.Identity{User: auth.User{ID: 1, Username: "root", Role: "admin"}, Reverified: true}
	f.hub.starting.Lock()
	result := make(chan error, 2)
	for range 2 {
		go func() { _, e := f.hub.CreateSession(id, NewSession{ProfileID: pid, Cwd: `C:\work`}); result <- e }()
	}
	time.Sleep(40 * time.Millisecond) // both callers reach the held allocation lock
	f.hub.starting.Unlock()
	go func() {
		for out := range n.ws.out {
			if out.kind == websocket.TextMessage {
				m, _ := proto.DecodeMsg(out.data)
				if m != nil && m.T == proto.MsgCreate {
					n.handleMsg(m.Reply())
				}
			}
		}
	}()
	ok := 0
	for range 2 {
		select {
		case e := <-result:
			if e == nil {
				ok++
			}
		case <-time.After(time.Second):
			t.Fatal("creation hung")
		}
	}
	close(n.ws.out)
	if ok != 1 {
		t.Fatalf("quota admitted %d sessions", ok)
	}
}
func TestReview13UploadRevocationAndNodePath(t *testing.T) {
	f := newFixture(t)
	admin := f.login("root", pw, true)
	_, pid := f.nodeWithProfile(admin, "pc1")
	_, out := admin.call("POST", "/api/admin/users", map[string]string{"username": "alice", "role": "user"})
	alice := f.login("alice", out["temp_password"].(string), false)
	admin.call("PUT", "/api/admin/profiles/"+itoa(pid)+"/bindings", map[string]any{"user_ids": []int64{2}})
	// Use a websocket pair solely for cancellation. No node or file writes.
	// A viewer socket is not needed: a closed transfer is sufficient and safe to cancel.
	dial := websocket.Dialer{TLSClientConfig: f.tlsc}
	token, _ := f.nodeWithProfile(admin, "pc2")
	ws, _, e := dial.Dial("wss"+strings.TrimPrefix(f.srv.URL, "https")+"/node/link", map[string][]string{"X-TH-Node-Token": {token}})
	if e != nil {
		t.Fatal(e)
	}
	defer ws.Close()
	u := &upload{ws: ws, userID: 2, nodeID: 1, next: 1, timer: time.NewTimer(time.Hour)}
	defer u.timer.Stop()
	if !f.hub.xfer.acquire(2, 1) {
		t.Fatal("fixture transfer reservation")
	}
	f.hub.xfer.uploads["test"] = u
	code, _ := alice.call("PUT", "/api/nodes/2/fs/uploads/test?offset=0", "x")
	if code != 404 {
		t.Errorf("node path bypass: %d", code)
	}
	admin.call("PUT", "/api/admin/profiles/"+itoa(pid)+"/bindings", map[string]any{"user_ids": []int64{}})
	f.hub.xfer.downloads["revoked"] = &downloadToken{nodeID: 1, userID: 2, csrf: alice.csrf, path: `C:\small.txt`, expires: time.Now().Add(time.Minute)}
	downloadCode, _ := alice.call("GET", "/api/dl/revoked", nil)
	if downloadCode != 404 {
		t.Errorf("revoked download status %d", downloadCode)
	}
	code, _ = alice.call("PUT", "/api/nodes/1/fs/uploads/test?offset=0", "x")
	if code != 404 {
		t.Errorf("revoked upload accepted: %d", code)
	}
}
func TestReview13SessionKindJSON(t *testing.T) {
	f := newFixture(t)
	sid := proto.NewSID()
	f.db.Exec(`INSERT INTO sessions(sid,node_id,owner_id,kind,created_at) VALUES (?,1,1,'claude',1)`, sid.String())
	row, e := f.reg.session(sid.String())
	if e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(row)
	if !strings.Contains(string(b), `"kind":"claude"`) {
		t.Fatalf("snapshot missing: %s", b)
	}
}

func TestReview13GapAndPasteTypist(t *testing.T) {
	f := newFixture(t)
	n := reviewNode(f)
	sid := proto.NewSID()
	f.db.Exec(`INSERT INTO sessions(sid,node_id,owner_id,created_at) VALUES (?,2,1,1)`, sid.String())
	rt := f.hub.ensureRoute(sid, 2, 80, 24)
	a := &attachment{rt: rt, ws: &wsConn{out: make(chan wsMsg, 10), done: make(chan struct{}), limit: 1 << 20}}
	rt.atts[a] = struct{}{}
	n.handleMsg(&proto.Msg{T: proto.MsgGap, SID: &sid, Next: 9})
	if len(a.ws.out) != 0 {
		t.Fatal("foreign gap forwarded")
	}
	n.node.ID = 2
	f.hub.nodes[2] = n
	n.handleMsg(&proto.Msg{T: proto.MsgGap, SID: &sid, Next: 9})
	if len(a.ws.out) != 1 {
		t.Fatal("owner gap not forwarded")
	}
	id := &auth.Identity{User: auth.User{ID: 1, Role: "admin"}, Reverified: true}
	u := &upload{userID: 1, nodeID: 2, sid: sid.String()}
	rt.owner = 1
	if e := f.hub.uploadAllowed(id, u); e != nil {
		t.Fatal(e)
	}
	rt.taker = 2
	if e := f.hub.uploadAllowed(id, u); e != ErrReadOnly {
		t.Fatalf("paste after takeover: %v", e)
	}
}
func TestReview13BrowserBatchResponse(t *testing.T) {
	f := newFixture(t)
	admin := f.login("root", pw, true)
	token, pid := f.nodeWithProfile(admin, "pc1")
	f.dialNode(token, "fp", nil)
	waitOnline(t, f.hub, 1, true)
	status, out := admin.call("POST", "/api/sessions", map[string]any{"profile_id": pid})
	if status != 200 {
		t.Fatal(out)
	}
	sid := out["session"].(map[string]any)["sid"].(string)
	v, _, e := admin.attach(sid, nil, 80, 24)
	if e != nil {
		t.Fatal(e)
	}
	if !v.wait(proto.MsgAttached).InputAck {
		t.Fatal("missing capability")
	}
	for _, id := range []uint64{91, 92} {
		data, _ := proto.EncodeMsg(&proto.Msg{T: proto.MsgInputBatch, ID: id, Data: []byte("x")})
		if e := v.ws.WriteMessage(websocket.TextMessage, data); e != nil {
			t.Fatal(e)
		}
		result := v.wait(proto.MsgInputResult)
		if result.Re != id || result.Code != "" {
			t.Fatalf("bad ack %+v", result)
		}
	}
}

func TestReview13RecoverLostSnapshot(t *testing.T) {
	f := newFixture(t)
	n := reviewNode(f)
	sid := proto.NewSID()
	f.db.Exec(`INSERT INTO sessions(sid,node_id,owner_id,profile_name,cwd,kind,created_at,ended_at,end_reason) VALUES (?,1,1,'original','C:\work','claude',1,2,'lost')`, sid.String())
	f.hub.reconcile(n, []proto.SessionInfo{{SID: sid, State: "running", Owner: "root"}})
	row, e := f.reg.session(sid.String())
	if e != nil || row.EndedAt != 0 || row.EndReason != "" || row.ProfileName != "original" || row.OwnerID != 1 {
		t.Fatalf("lost snapshot not recovered: %+v %v", row, e)
	}
}
