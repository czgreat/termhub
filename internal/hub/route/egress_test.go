package route

import (
	"github.com/gorilla/websocket"
	"strings"
	"termhub/internal/egress"
	"termhub/internal/hub/auth"
	"termhub/internal/proto"
	"testing"
	"time"
)

func protect(t *testing.T, h *Hub) {
	t.Helper()
	m, e := egress.New([]egress.Config{{NodeID: 1, URL: "https://192.168.1.1/status", Pin: strings.Repeat("0", 64), Token: strings.Repeat("a", 64), ExpectedIP: "203.0.113.1"}})
	if e != nil {
		t.Fatal(e)
	}
	h.Egress = m
}
func TestEgressEntryPointsAndInterrupt(t *testing.T) {
	f := newFixture(t)
	admin := f.login("root", pw, true)
	_, pid := f.nodeWithProfile(admin, "pc1")
	n := reviewNode(f)
	protect(t, f.hub)
	id := &auth.Identity{User: auth.User{ID: 1, Username: "root", Role: "admin"}, Reverified: true}
	if _, e := f.hub.CreateSession(id, NewSession{ProfileID: pid}); e != ErrEgress {
		t.Fatalf("create: %v", e)
	}
	for _, msg := range []*proto.Msg{{T: proto.MsgCreate}, {T: proto.MsgFsMkdir}, {T: proto.MsgClipboard}} {
		if _, e := n.request(msg, time.Second); e != ErrEgress {
			t.Fatalf("%s: %v", msg.T, e)
		}
	}
	for _, dir := range []string{proto.DirUp, proto.DirPaste} {
		if _, _, e := f.hub.openTransfer(1, &proto.Msg{Dir: dir}); e != ErrEgress {
			t.Fatalf("transfer: %v", e)
		}
	}
	if e := f.hub.uploadAllowed(id, &upload{nodeID: 1}); e != ErrEgress {
		t.Fatalf("chunk/commit: %v", e)
	}
	sid := proto.NewSID()
	rt := f.hub.ensureRoute(sid, 1, 80, 24)
	rt.owner = 1
	a := &attachment{rt: rt, id: id, ws: &wsConn{out: make(chan wsMsg, 20), done: make(chan struct{}), limit: 1 << 20}}
	if code := a.queueInput([]byte("normal")); code != ErrEgress.Code {
		t.Fatal(code)
	}
	if code := a.queueInputEpoch([]byte("normal"), 123, false); code != ErrEgress.Code {
		t.Fatal(code)
	}
	if len(n.ws.out) != 0 {
		t.Fatal("blocked operation reached node")
	}
	if code := a.queueInputEpoch([]byte{3}, 0, true); code != "" {
		t.Fatal(code)
	}
	frame := <-n.ws.out
	fmsg, e := proto.ParseFrame(frame.data)
	if e != nil || string(fmsg.Data) != "\x03" {
		t.Fatal("interrupt is not fixed Ctrl+C")
	}
	rt.taker = 2
	if code := a.queueInputEpoch([]byte{3}, 0, true); code != proto.ErrReadOnly {
		t.Fatal("watcher interrupted")
	}
	// Read-only control traffic survives the guard.
	if !n.sendMsg(&proto.Msg{T: proto.MsgRedraw, SID: &sid}) {
		t.Fatal("redraw blocked")
	}
}
func TestEgressWriterRechecksQueuedMessages(t *testing.T) {
	w := &wsConn{out: make(chan wsMsg, 20), done: make(chan struct{}), limit: 1 << 20}
	healthy := true
	epoch := uint64(1)
	rejected := 0
	check := func() bool { return healthy && epoch == 1 }
	if !w.sendChecked(websocket.BinaryMessage, []byte("input"), check, func() { rejected++ }) {
		t.Fatal("queue")
	}
	w.send(websocket.TextMessage, []byte("query"))
	healthy = false
	epoch++ // transition already seen while the writer was blocked
	healthy = true
	m, _ := w.take()
	if m.permitted() || rejected != 1 {
		t.Fatal("old input delivered after recovery")
	}
	m, _ = w.take()
	if !m.permitted() || string(m.data) != "query" {
		t.Fatal("query lost")
	}
}
func TestEgressDroppedCreateHasDefiniteFailure(t *testing.T) {
	f := newFixture(t)
	n := reviewNode(f)
	sid := proto.NewSID()
	result := make(chan error, 1)
	go func() { _, e := n.request(&proto.Msg{T: proto.MsgCreate, SID: &sid}, time.Second); result <- e }()
	// Test the writer's real rejection callback after admission, without a slow peer.
	var queued wsMsg
	select {
	case queued = <-n.ws.out:
	case <-time.After(time.Second):
		t.Fatal("not enqueued")
	}
	protect(t, f.hub)
	if queued.permitted() {
		t.Fatal("create not guarded")
	}
	select {
	case e := <-result:
		if errCode(e) != ErrEgress.Code {
			t.Fatalf("%v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("request left pending")
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.creates) != 0 || len(n.creating) != 0 {
		t.Fatal("create reservation retained")
	}
}

func TestEgressCreateRejectedAfterTimeout(t *testing.T) {
	f := newFixture(t)
	n := reviewNode(f)
	sid := proto.NewSID()
	f.db.Exec(`INSERT INTO sessions(sid,node_id,owner_id,created_at,end_reason) VALUES (?,1,1,1,'starting')`, sid.String())
	_, err := n.request(&proto.Msg{T: proto.MsgCreate, SID: &sid}, time.Millisecond)
	if errCode(err) != "node_timeout" {
		t.Fatal(err)
	}
	f.db.Exec(`UPDATE sessions SET end_reason='start_unknown' WHERE sid=?`, sid.String())
	queued, _ := n.ws.take()
	protect(t, f.hub)
	if queued.permitted() {
		t.Fatal("stale create allowed")
	}
	row, err := f.reg.session(sid.String())
	if err != nil || row.EndedAt == 0 || row.EndReason != "start_failed" {
		t.Fatalf("reservation survived: %+v %v", row, err)
	}
}

func TestEgressBatchAcknowledgesWriterNotQueue(t *testing.T) {
	f := newFixture(t)
	n := reviewNode(f)
	sid := proto.NewSID()
	rt := f.hub.ensureRoute(sid, 1, 80, 24)
	rt.owner = 1
	a := &attachment{rt: rt, id: &auth.Identity{User: auth.User{ID: 1}}, ws: &wsConn{out: make(chan wsMsg, 20), done: make(chan struct{}), limit: 1 << 20}}
	if code := a.queueInputTracked([]byte("body"), 0, false, 42); code != "" {
		t.Fatal(code)
	}
	if len(a.ws.out) != 0 {
		t.Fatal("batch acknowledged while still queued")
	}
	queued, _ := n.ws.take()
	protect(t, f.hub)
	if queued.permitted() {
		t.Fatal("queued body survived guard")
	}
	reply, _ := a.ws.take()
	msg, err := proto.DecodeMsg(reply.data)
	if err != nil || msg.T != proto.MsgInputResult || msg.Re != 42 || msg.Code != ErrEgress.Code {
		t.Fatalf("rejection lost batch identity: %s", reply.data)
	}
	// Success and write failure are reported only by the writer completion callback.
	for _, ok := range []bool{true, false} {
		other := reviewNode(f)
		f.hub.Egress = nil
		b := &attachment{rt: rt, id: a.id, ws: &wsConn{out: make(chan wsMsg, 20), done: make(chan struct{}), limit: 1 << 20}}
		if code := b.queueInputTracked([]byte("text"), 0, false, 43); code != "" {
			t.Fatal(code)
		}
		if len(b.ws.out) != 0 {
			t.Fatal("early ack")
		}
		frame, _ := other.ws.take()
		frame.written(ok)
		r, _ := b.ws.take()
		m, _ := proto.DecodeMsg(r.data)
		if m.Re != 43 || (ok && m.Code != "") || (!ok && m.Code != "unknown") {
			t.Fatal("wrong completion result")
		}
	}
}
