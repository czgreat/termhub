package route

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"termhub/internal/hub/auth"
	"termhub/internal/hub/store"
	"termhub/internal/proto"
)

const pw = "correct horse battery"

type fixture struct {
	t    *testing.T
	db   *store.DB
	svc  *auth.Service
	reg  *Registry
	hub  *Hub
	srv  *httptest.Server
	tlsc *tls.Config
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	svc, err := auth.New(db, make([]byte, 32), auth.Config{Hash: auth.HashParams{MemoryKiB: 64, Time: 1, Threads: 1}})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	web, err := auth.NewHTTP(svc, srv.URL, nil, quiet)
	if err != nil {
		t.Fatal(err)
	}
	seal, _ := auth.NewSealer(make([]byte, 32))
	reg := NewRegistry(db, seal, nil)
	hub := NewHub(reg, web, Limits{}, quiet)
	web.Register(mux)
	hub.Register(mux)
	f := &fixture{t: t, db: db, svc: svc, reg: reg, hub: hub, srv: srv,
		tlsc: srv.Client().Transport.(*http.Transport).TLSClientConfig}
	if err := svc.Setup(svc.SetupToken(), "root", pw, ""); err != nil {
		t.Fatal(err)
	}
	return f
}

// user is a logged-in browser. The forced first-login steps are skipped by
// marking them done in the database after logging in.
type user struct {
	f      *fixture
	c      *http.Client
	csrf   string
	cookie string
	name   string
}

func (f *fixture) login(name, password string, reverified bool) *user {
	f.t.Helper()
	jar, _ := cookiejar.New(nil)
	c := *f.srv.Client()
	c.Jar = jar
	u := &user{f: f, c: &c, name: name}
	// Without a confirmed second factor a password alone yields a session.
	f.db.Exec(`UPDATE users SET totp_confirmed_at=NULL WHERE username=?`, name)
	if code, out := u.call("POST", "/api/auth/login", map[string]string{"username": name, "password": password}); code != 200 || out["done"] != true {
		f.t.Fatalf("login %s: %d %v", name, code, out)
	}
	f.db.Exec(`UPDATE users SET totp_confirmed_at=1, totp_secret=x'00', must_change_password=0 WHERE username=?`, name)
	if reverified {
		f.db.Exec(`UPDATE login_sessions SET reverified_until=? WHERE user_id=(SELECT id FROM users WHERE username=?)`, time.Now().Add(time.Hour).Unix(), name)
	}
	_, me := u.call("GET", "/api/me", nil)
	u.csrf, _ = me["csrf"].(string)
	req, _ := http.NewRequest("GET", f.srv.URL, nil)
	for _, ck := range jar.Cookies(req.URL) {
		u.cookie = ck.Name + "=" + ck.Value
	}
	return u
}

func (u *user) call(method, path string, body any) (int, map[string]any) {
	u.f.t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, u.f.srv.URL+path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", u.f.srv.URL)
	req.Header.Set("X-TH-CSRF", u.csrf)
	resp, err := u.c.Do(req)
	if err != nil {
		u.f.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// ---- a fake node speaking link B ----

type fakeSession struct {
	buf        []byte
	forwarding bool
	cols, rows int
	owner      string
}

type fakeNode struct {
	t        *testing.T
	ws       *websocket.Conn
	wmu      sync.Mutex
	mu       sync.Mutex
	sessions map[proto.SID]*fakeSession
	forwards []bool // history of forward on/off, for assertions
	closed   chan struct{}
	created  []*proto.Msg  // every create request, for assertions
	history  []*proto.Msg  // every history request, for assertions
	hold     chan struct{} // when set, history answers wait until it is closed
}

func (f *fixture) dialNode(token, fingerprint string, preset map[proto.SID]*fakeSession) (*fakeNode, *http.Response, error) {
	d := websocket.Dialer{TLSClientConfig: f.tlsc}
	ws, resp, err := d.Dial("wss"+strings.TrimPrefix(f.srv.URL, "https")+"/node/link", http.Header{"X-TH-Node-Token": {token}})
	if err != nil {
		return nil, resp, err
	}
	n := &fakeNode{t: f.t, ws: ws, sessions: map[proto.SID]*fakeSession{}, closed: make(chan struct{})}
	for k, v := range preset {
		n.sessions[k] = v
	}
	n.send(&proto.Msg{T: proto.MsgHello, ID: 1, Proto: proto.Current.String(), AgentVer: "fake", Fingerprint: fingerprint})
	go n.loop()
	f.t.Cleanup(func() { ws.Close() })
	return n, resp, nil
}

func (n *fakeNode) send(m *proto.Msg) {
	b, _ := proto.EncodeMsg(m)
	n.wmu.Lock()
	n.ws.WriteMessage(websocket.TextMessage, b)
	n.wmu.Unlock()
}

func (n *fakeNode) frame(f proto.Frame) {
	b, _ := proto.AppendFrame(nil, f)
	n.wmu.Lock()
	n.ws.WriteMessage(websocket.BinaryMessage, b)
	n.wmu.Unlock()
}

func (n *fakeNode) list() []proto.SessionInfo {
	var out []proto.SessionInfo
	for sid, s := range n.sessions {
		out = append(out, proto.SessionInfo{SID: sid, State: "running", Owner: s.owner, Cols: s.cols, Rows: s.rows, End: uint64(len(s.buf)), Created: time.Now().Unix()})
	}
	return out
}

// emit appends program output to a session and forwards it if anyone watches.
func (n *fakeNode) emit(sid proto.SID, text string) {
	n.mu.Lock()
	s := n.sessions[sid]
	off := uint64(len(s.buf))
	s.buf = append(s.buf, text...)
	fwd := s.forwarding
	n.mu.Unlock()
	if fwd {
		n.frame(proto.Frame{Type: proto.TypeOutput, SID: sid, Offset: off, Data: []byte(text)})
	}
}

func (n *fakeNode) loop() {
	defer close(n.closed)
	for {
		kind, data, err := n.ws.ReadMessage()
		if err != nil {
			return
		}
		if kind == websocket.BinaryMessage {
			if f, err := proto.ParseFrame(data); err == nil && f.Type == proto.TypeInput {
				n.emit(f.SID, "IN:"+string(f.Data))
			}
			continue
		}
		m, err := proto.DecodeMsg(data)
		if err != nil {
			continue
		}
		switch m.T {
		case proto.MsgWelcome:
			n.mu.Lock()
			l := n.list()
			n.mu.Unlock()
			n.send(&proto.Msg{T: proto.MsgSessions, List: l})
		case proto.MsgCreate:
			if m.Profile.Command == "missing" {
				n.send(m.Fail("command_not_found", "missing"))
				continue
			}
			n.mu.Lock()
			n.sessions[*m.SID] = &fakeSession{cols: m.Cols, rows: m.Rows, owner: m.Owner, buf: []byte("READY\r\n")}
			n.created = append(n.created, m)
			n.mu.Unlock()
			n.send(m.Reply())
		case proto.MsgUsage:
			n.mu.Lock()
			n.history = append(n.history, m)
			n.mu.Unlock()
			r := m.Reply()
			r.Payload = []byte(`{"conv":"c-new","model":"claude-opus-5-5","context":100000,` +
				`"buckets":[{"model":"claude-opus-5-5","input":1000000,"output":100000}]}`)
			n.send(r)
		case proto.MsgHistoryList, proto.MsgHistoryRead:
			n.mu.Lock()
			n.history = append(n.history, m)
			hold := n.hold
			n.mu.Unlock()
			r := m.Reply()
			if m.T == proto.MsgHistoryList {
				r.Payload = []byte(`{"items":[{"id":"c-new","cwd":"C:\\w\\a","title":"newest","updated":300},` +
					`{"id":"c-old","cwd":"C:\\w\\a","first":"older","updated":200},{"id":"c-b","cwd":"C:\\w\\b","updated":100}],"scanned":3}`)
			} else {
				r.Payload = []byte(`{"id":"c-new","cwd":"C:\\w\\a","messages":[{"role":"user","text":"hi"},{"role":"assistant","text":"hello"}],"cursor":0}`)
			}
			if hold != nil {
				go func() { <-hold; n.send(r) }()
				continue
			}
			n.send(r)
		case proto.MsgForward:
			n.mu.Lock()
			if s := n.sessions[*m.SID]; s != nil {
				s.forwarding = *m.On
			}
			n.forwards = append(n.forwards, *m.On)
			n.mu.Unlock()
		case proto.MsgReplay:
			n.mu.Lock()
			s := n.sessions[*m.SID]
			from, mode := uint64(0), proto.ModeReplay
			if m.From != nil && *m.From <= uint64(len(s.buf)) {
				from, mode = *m.From, proto.ModeResume
			}
			data := append([]byte(nil), s.buf[from:]...)
			end := uint64(len(s.buf))
			cols, rows := s.cols, s.rows
			n.mu.Unlock()
			var prelude []byte
			if mode == proto.ModeReplay {
				prelude = []byte("\x1b[?2004h")
			}
			n.send(&proto.Msg{T: proto.MsgReplayBegin, SID: m.SID, Req: m.Req, Mode: mode, From: &from, Cols: cols, Rows: rows, Prelude: prelude})
			if len(data) > 0 {
				n.frame(proto.Frame{Type: proto.TypeReplay, SID: *m.SID, Req: m.Req, Offset: from, Data: data})
			}
			n.send(&proto.Msg{T: proto.MsgReplayEnd, SID: m.SID, Req: m.Req, End: end})
		case proto.MsgResize:
			n.mu.Lock()
			if s := n.sessions[*m.SID]; s != nil {
				s.cols, s.rows = m.Cols, m.Rows
			}
			n.mu.Unlock()
		case proto.MsgClose:
			n.mu.Lock()
			delete(n.sessions, *m.SID)
			n.mu.Unlock()
			n.send(m.Reply())
			zero := 0
			n.send(&proto.Msg{T: proto.MsgSessionExited, SID: m.SID, Reason: "user", ExitCode: &zero})
		}
	}
}

// ---- a viewer ----

type viewer struct {
	t    *testing.T
	ws   *websocket.Conn
	msgs chan *proto.Msg
	drv  chan *proto.Msg // driver messages, kept apart: they come in any order with the others
	tr   *proto.Tracker
	mu   sync.Mutex
	text bytes.Buffer
}

func (u *user) attach(sid string, have *uint64, cols, rows int) (*viewer, *http.Response, error) {
	d := websocket.Dialer{TLSClientConfig: u.f.tlsc}
	ws, resp, err := d.Dial("wss"+strings.TrimPrefix(u.f.srv.URL, "https")+"/ws/session/"+sid,
		http.Header{"Origin": {u.f.srv.URL}, "Cookie": {u.cookie}})
	if err != nil {
		return nil, resp, err
	}
	v := &viewer{t: u.f.t, ws: ws, msgs: make(chan *proto.Msg, 256), drv: make(chan *proto.Msg, 64), tr: proto.NewTracker(0)}
	u.f.t.Cleanup(func() { ws.Close() })
	go func() {
		defer close(v.msgs)
		for {
			kind, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if kind == websocket.BinaryMessage {
				off, d, err := proto.ParseBrowserOutput(data)
				if err != nil {
					continue
				}
				v.mu.Lock()
				fresh, gap := v.tr.Accept(off, d)
				if gap {
					v.text.WriteString("<<HOLE>>")
				}
				v.text.Write(fresh)
				v.mu.Unlock()
				continue
			}
			if m, err := proto.DecodeMsg(data); err == nil {
				if m.T == proto.MsgAttached && m.From != nil {
					v.mu.Lock()
					v.tr.Reset(*m.From)
					v.mu.Unlock()
				}
				if m.T == proto.MsgDriver {
					v.drv <- m
					continue
				}
				v.msgs <- m
			}
		}
	}()
	b, _ := proto.EncodeMsg(&proto.Msg{T: proto.MsgAttach, Have: have, Cols: cols, Rows: rows})
	ws.WriteMessage(websocket.TextMessage, b)
	return v, resp, nil
}

func (v *viewer) wait(typ string) *proto.Msg {
	v.t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case m, ok := <-v.msgs:
			if !ok {
				v.t.Fatalf("connection closed while waiting for %s", typ)
			}
			if m.T == typ {
				return m
			}
		case <-deadline:
			v.t.Fatalf("timed out waiting for %s", typ)
		}
	}
}

func (v *viewer) waitText(want string) {
	v.t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		v.mu.Lock()
		s := v.text.String()
		v.mu.Unlock()
		if strings.Contains(s, want) {
			return
		}
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.t.Fatalf("never saw %q; got %q", want, v.text.String())
}

func (v *viewer) content() string { v.mu.Lock(); defer v.mu.Unlock(); return v.text.String() }

func (v *viewer) input(s string) { v.ws.WriteMessage(websocket.BinaryMessage, []byte(s)) }

// ---- setup helpers ----

func (f *fixture) nodeWithProfile(admin *user, nodeName string) (token string, profileID int64) {
	f.t.Helper()
	code, out := admin.call("POST", "/api/admin/nodes", map[string]string{"name": nodeName})
	if code != 200 {
		f.t.Fatalf("create node: %d %v", code, out)
	}
	token = out["token"].(string)
	nodeID := int64(out["node"].(map[string]any)["id"].(float64))
	code, out = admin.call("POST", "/api/admin/profiles", Profile{NodeID: nodeID, Name: "cli", Mode: "direct", Command: "testcli",
		Env: map[string]string{"SECRET_KEY": "s3cr3t-value"}, IdleTimeout: 60})
	if code != 200 {
		f.t.Fatalf("create profile: %d %v", code, out)
	}
	return token, int64(out["profile"].(map[string]any)["id"].(float64))
}

func waitOnline(t *testing.T, h *Hub, nodeID int64, want bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if h.Online(nodeID) == want {
			return
		}
	}
	t.Fatalf("node online=%v never reached", want)
}

// ---- tests ----

func TestNodeAuthentication(t *testing.T) {
	f := newFixture(t)
	admin := f.login("root", pw, true)
	token, _ := f.nodeWithProfile(admin, "pc1")

	if _, resp, err := f.dialNode("thn_wrong", "fp", nil); err == nil || resp == nil || resp.StatusCode != 401 {
		t.Fatalf("wrong token must be refused before the upgrade: %v %v", resp, err)
	}
	n, _, err := f.dialNode(token, "fp-A", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitOnline(t, f.hub, 1, true)
	// Same token from another machine: the fingerprint does not match.
	n2, _, err := f.dialNode(token, "fp-B", nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-n2.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("a node with a foreign fingerprint must be dropped")
	}
	var flagged bool
	f.db.QueryRow(`SELECT fingerprint_mismatch FROM nodes WHERE id=1`).Scan(&flagged)
	if !flagged || !f.hub.Online(1) {
		t.Fatalf("mismatch flagged=%v, legitimate node online=%v", flagged, f.hub.Online(1))
	}
	// Rotation: the new token's first use retires the old one.
	_, out := admin.call("POST", "/api/admin/nodes/1/rotate-token", nil)
	newToken := out["token"].(string)
	n.ws.Close()
	waitOnline(t, f.hub, 1, false)
	if _, _, err := f.dialNode(token, "fp-A", nil); err != nil {
		t.Fatalf("old token should still work until the new one is used: %v", err)
	}
	if _, _, err := f.dialNode(newToken, "fp-A", nil); err != nil {
		t.Fatal(err)
	}
	if _, resp, err := f.dialNode(token, "fp-A", nil); err == nil || resp.StatusCode != 401 {
		t.Fatal("old token must be dead after the new one was used")
	}
	// Disabling the node cuts it off and refuses it.
	admin.call("POST", "/api/admin/nodes/1/disable", nil)
	waitOnline(t, f.hub, 1, false)
	if _, resp, err := f.dialNode(newToken, "fp-A", nil); err == nil || resp.StatusCode != 401 {
		t.Fatal("a disabled node must be refused")
	}
}

func TestCreateAttachInputAndSecondViewer(t *testing.T) {
	f := newFixture(t)
	admin := f.login("root", pw, true)
	token, pid := f.nodeWithProfile(admin, "pc1")
	node, _, _ := f.dialNode(token, "fp", nil)
	waitOnline(t, f.hub, 1, true)

	code, out := admin.call("POST", "/api/sessions", map[string]any{"profile_id": pid, "cwd": `C:\work`, "cols": 100, "rows": 30})
	if code != 200 {
		t.Fatalf("create: %d %v", code, out)
	}
	sid := out["session"].(map[string]any)["sid"].(string)
	psid, _ := proto.ParseSID(sid)

	v1, _, err := admin.attach(sid, nil, 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	att := v1.wait(proto.MsgAttached)
	if att.Mode != proto.ModeReplay || string(att.Prelude) != "\x1b[?2004h" || att.Cols != 100 {
		t.Fatalf("attached: %+v", att)
	}
	v1.waitText("READY")
	v1.input("hello")
	v1.waitText("IN:hello")

	// A second device joins: full replay, then the same live stream.
	v2, _, _ := admin.attach(sid, nil, 80, 24)
	v2.wait(proto.MsgAttached)
	v2.waitText("IN:hello")
	node.emit(psid, "LIVE-1 ")
	node.emit(psid, "LIVE-2 ")
	v1.waitText("LIVE-2")
	v2.waitText("LIVE-2")
	for i, v := range []*viewer{v1, v2} {
		if got := v.content(); got != "READY\r\nIN:helloLIVE-1 LIVE-2 " {
			t.Fatalf("viewer %d stream has a hole or a repeat: %q", i+1, got)
		}
	}
	// Typing on the second device makes it lead: its size goes to the node, both are told.
	v2.input("x")
	s1 := v1.wait(proto.MsgSize)
	if s1.Cols != 80 || s1.Rows != 24 {
		t.Fatalf("size broadcast: %+v", s1)
	}
	time.Sleep(200 * time.Millisecond)
	node.mu.Lock()
	cols, rows := node.sessions[psid].cols, node.sessions[psid].rows
	node.mu.Unlock()
	if cols != 80 || rows != 24 {
		t.Fatalf("node size %dx%d", cols, rows)
	}
	// Resume: a viewer that comes back with its offset gets only what it missed.
	have := uint64(len("READY\r\n"))
	v3, _, _ := admin.attach(sid, &have, 100, 30)
	if a := v3.wait(proto.MsgAttached); a.Mode != proto.ModeResume || *a.From != have || len(a.Prelude) != 0 {
		t.Fatalf("resume: %+v", a)
	}
	v3.waitText("LIVE-2")
	if strings.Contains(v3.content(), "READY") {
		t.Fatal("a resume must not repeat what the viewer already has")
	}
	// Nobody watching: the node is told to stop forwarding.
	v1.ws.Close()
	v2.ws.Close()
	v3.ws.Close()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		node.mu.Lock()
		last := len(node.forwards) > 0 && !node.forwards[len(node.forwards)-1]
		node.mu.Unlock()
		if last {
			break
		}
	}
	node.mu.Lock()
	if n := len(node.forwards); n < 2 || node.forwards[n-1] {
		t.Fatalf("forward history %v: must end with off", node.forwards)
	}
	node.mu.Unlock()
	// Closing through the API ends it and the register records why.
	if code, _ := admin.call("DELETE", "/api/sessions/"+sid, nil); code != 200 {
		t.Fatalf("close: %d", code)
	}
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if row, _ := f.reg.session(sid); row.EndedAt != 0 {
			if row.EndReason != "user" || row.ExitCode == nil {
				t.Fatalf("end record: %+v", row)
			}
			return
		}
	}
	t.Fatal("session never marked ended")
}

func TestStartFailureReachesTheBrowser(t *testing.T) {
	f := newFixture(t)
	admin := f.login("root", pw, true)
	token, _ := f.nodeWithProfile(admin, "pc1")
	f.dialNode(token, "fp", nil)
	waitOnline(t, f.hub, 1, true)
	_, out := admin.call("POST", "/api/admin/profiles", Profile{NodeID: 1, Name: "broken", Mode: "direct", Command: "missing"})
	pid := int64(out["profile"].(map[string]any)["id"].(float64))
	code, out := admin.call("POST", "/api/sessions", map[string]any{"profile_id": pid})
	if code == 200 || out["code"] != "command_not_found" {
		t.Fatalf("the node's error code must arrive unchanged: %d %v", code, out)
	}
}

func TestPermissions(t *testing.T) {
	f := newFixture(t)
	admin := f.login("root", pw, true)
	token, pid := f.nodeWithProfile(admin, "pc1")
	f.dialNode(token, "fp", nil)
	waitOnline(t, f.hub, 1, true)
	mk := func(name string) *user {
		_, out := admin.call("POST", "/api/admin/users", map[string]string{"username": name, "role": "user"})
		return f.login(name, out["temp_password"].(string), false)
	}
	alice, bob := mk("alice"), mk("bob")

	if _, out := alice.call("GET", "/api/profiles", nil); out["profiles"] != nil {
		t.Fatalf("unbound user sees profiles: %v", out)
	}
	if code, _ := alice.call("POST", "/api/sessions", map[string]any{"profile_id": pid}); code != 403 {
		t.Fatalf("unbound user created a session: %d", code)
	}
	if code, _ := alice.call("POST", "/api/admin/nodes", map[string]string{"name": "x"}); code != 403 {
		t.Fatalf("user created a node: %d", code)
	}
	admin.call("PUT", "/api/admin/profiles/"+itoa(pid)+"/bindings", map[string]any{"user_ids": []int64{2}})
	_, out := alice.call("GET", "/api/profiles", nil)
	list := out["profiles"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["env"] != nil {
		t.Fatalf("bound user must see the profile without its environment secrets: %v", list)
	}
	code, out := alice.call("POST", "/api/sessions", map[string]any{"profile_id": pid})
	if code != 200 {
		t.Fatalf("bound user: %d %v", code, out)
	}
	sid := out["session"].(map[string]any)["sid"].(string)

	// Bob: not his session. Guessing the id gets him the same 404 as a random id.
	if _, resp, err := bob.attach(sid, nil, 80, 24); err == nil || resp.StatusCode != 404 {
		t.Fatalf("bob attached to alice's session: %v", err)
	}
	if code, _ := bob.call("DELETE", "/api/sessions/"+sid, nil); code != 404 {
		t.Fatalf("bob closed alice's session: %d", code)
	}
	if _, out := bob.call("GET", "/api/sessions", nil); out["sessions"] != nil {
		t.Fatalf("bob sees sessions: %v", out)
	}
	// An administrator needs a fresh second factor to look into someone's terminal.
	plain := f.login("root", pw, false)
	f.db.Exec(`UPDATE login_sessions SET reverified_until=0 WHERE token_hash NOT IN (SELECT token_hash FROM login_sessions ORDER BY created_at LIMIT 1)`)
	if _, resp, err := plain.attach(sid, nil, 80, 24); err == nil || resp.StatusCode != 403 {
		t.Fatalf("admin without reverification attached: %v", err)
	}
	av, _, err := admin.attach(sid, nil, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	av.wait(proto.MsgAttached)
	// Alice is told somebody else is looking.
	alv, _, _ := alice.attach(sid, nil, 80, 24)
	for {
		if p := alv.wait(proto.MsgPeers); p.Count == 2 {
			break
		}
	}
	// Unbinding closes her terminal connection at once; the session keeps running.
	admin.call("PUT", "/api/admin/profiles/"+itoa(pid)+"/bindings", map[string]any{"user_ids": []int64{}})
	select {
	case _, ok := <-alv.msgs:
		for ok {
			_, ok = <-alv.msgs
		}
	case <-time.After(5 * time.Second):
		t.Fatal("unbound user's connection stayed open")
	}
	if row, _ := f.reg.session(sid); row.EndedAt != 0 {
		t.Fatal("unbinding must not end the session")
	}
	var leaked int
	f.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE detail LIKE '%s3cr3t%' OR object LIKE '%s3cr3t%'`).Scan(&leaked)
	if leaked != 0 {
		t.Fatal("a profile secret reached the audit log")
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestReconcileOnReconnect(t *testing.T) {
	f := newFixture(t)
	admin := f.login("root", pw, true)
	token, pid := f.nodeWithProfile(admin, "pc1")
	node, _, _ := f.dialNode(token, "fp", nil)
	waitOnline(t, f.hub, 1, true)
	_, out := admin.call("POST", "/api/sessions", map[string]any{"profile_id": pid})
	kept := out["session"].(map[string]any)["sid"].(string)
	_, out = admin.call("POST", "/api/sessions", map[string]any{"profile_id": pid})
	lost := out["session"].(map[string]any)["sid"].(string)
	keptSID, _ := proto.ParseSID(kept)

	v, _, _ := admin.attach(kept, nil, 80, 24)
	v.wait(proto.MsgAttached)
	node.ws.Close() // the machine reboots
	v.wait(proto.MsgNodeOffline)

	// It comes back with one of the two sessions, plus one the Hub never heard of.
	stranger := proto.NewSID()
	node.mu.Lock()
	preset := map[proto.SID]*fakeSession{keptSID: node.sessions[keptSID], stranger: {buf: []byte("X"), owner: "root", cols: 80, rows: 24}}
	node.mu.Unlock()
	f.dialNode(token, "fp", preset)
	v.wait(proto.MsgNodeOnline)

	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if row, _ := f.reg.session(lost); row.EndedAt != 0 {
			break
		}
	}
	if row, _ := f.reg.session(lost); row.EndReason != "lost" {
		t.Fatalf("a session the node no longer has must be marked lost: %+v", row)
	}
	if row, _ := f.reg.session(kept); row.EndedAt != 0 {
		t.Fatalf("the surviving session was ended: %+v", row)
	}
	if row, err := f.reg.session(stranger.String()); err != nil || row.OwnerID != 1 {
		t.Fatalf("an unknown session with a known owner must be adopted: %+v %v", row, err)
	}
}
