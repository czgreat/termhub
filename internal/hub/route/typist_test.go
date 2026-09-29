package route

import (
	"strings"
	"sync"
	"testing"
	"time"

	"termhub/internal/proto"
)

// driverIs waits for the next driver message and checks it.
func (v *viewer) driverIs(on bool, by string) {
	v.t.Helper()
	var m *proto.Msg
	select {
	case m = <-v.drv:
	case <-time.After(10 * time.Second):
		v.t.Fatal("timed out waiting for driver")
	}
	if m.On == nil || *m.On != on || m.By != by {
		v.t.Fatalf("driver: on=%v by=%q, want on=%v by=%q", m.On, m.By, on, by)
	}
}

// One session, one person at the keyboard: an administrator looking into
// someone's terminal only watches until taking it over (reverified each
// time); the owner takes it back, and gets it back when the administrator
// leaves.
func TestOneTypistAndTakeOver(t *testing.T) {
	f := newFixture(t)
	admin := f.login("root", pw, true)
	token, pid := f.nodeWithProfile(admin, "pc1")
	node, _, _ := f.dialNode(token, "fp", nil)
	waitOnline(t, f.hub, 1, true)
	_, out := admin.call("POST", "/api/admin/users", map[string]string{"username": "alice", "role": "user"})
	alice := f.login("alice", out["temp_password"].(string), false)
	admin.call("PUT", "/api/admin/profiles/"+itoa(pid)+"/bindings", map[string]any{"user_ids": []int64{2}})
	code, out := alice.call("POST", "/api/sessions", map[string]any{"profile_id": pid, "cols": 100, "rows": 30})
	if code != 200 {
		t.Fatalf("create: %d %v", code, out)
	}
	sid := out["session"].(map[string]any)["sid"].(string)
	psid, _ := proto.ParseSID(sid)

	al, _, _ := alice.attach(sid, nil, 100, 30)
	al.wait(proto.MsgAttached)
	al.driverIs(true, "")
	// her phone as well: the same person types on both
	phone, _, _ := alice.attach(sid, nil, 60, 20)
	phone.wait(proto.MsgAttached)
	phone.driverIs(true, "")
	phone.input("from-phone")
	al.waitText("IN:from-phone")
	phone.ws.Close()
	ad, _, err := admin.attach(sid, nil, 40, 10)
	if err != nil {
		t.Fatal(err)
	}
	ad.wait(proto.MsgAttached)
	ad.driverIs(false, "")

	// Watching only: input refused, the window does not reshape hers, no paste.
	ad.input("from-admin")
	if m := ad.wait(proto.MsgErr); m.Code != proto.ErrReadOnly {
		t.Fatalf("admin input: %+v", m)
	}
	ad.ws.WriteJSON(proto.Msg{T: proto.MsgResize, Cols: 40, Rows: 10})
	al.input("from-alice")
	al.waitText("IN:from-alice")
	if strings.Contains(al.content(), "from-admin") {
		t.Fatal("a watcher's input reached the session")
	}
	node.mu.Lock()
	cols := node.sessions[psid].cols
	node.mu.Unlock()
	if cols != 100 {
		t.Fatalf("a watcher resized the session to %d columns", cols)
	}
	if code, out := admin.call("POST", "/api/sessions/"+sid+"/paste-file", map[string]any{"name": "a.txt", "size": 3}); code != 409 || out["code"] != proto.ErrReadOnly {
		t.Fatalf("watcher paste: %d %v", code, out)
	}

	// Taken over only from an open terminal: a request from an administrator
	// not looking at it would leave nobody to hand it back.
	_, out = admin.call("POST", "/api/admin/users", map[string]string{"username": "root2", "role": "admin"})
	root2 := f.login("root2", out["temp_password"].(string), true)
	if code, out := root2.call("POST", "/api/sessions/"+sid+"/take", nil); code != 409 || out["code"] != "not_attached" {
		t.Fatalf("take-over without the terminal open: %d %v", code, out)
	}
	// Taking over asks for a second factor given within the last minute.
	f.db.Exec(`UPDATE login_sessions SET reverified_until=? WHERE user_id=1`, time.Now().Add(5*time.Minute).Unix())
	if code, out := admin.call("POST", "/api/sessions/"+sid+"/take", nil); code != 403 || out["code"] != "reverify_required" {
		t.Fatalf("take-over on an old reverification: %d %v", code, out)
	}
	f.db.Exec(`UPDATE login_sessions SET reverified_until=? WHERE user_id=1`, time.Now().Add(10*time.Minute).Unix())
	if code, out := admin.call("POST", "/api/sessions/"+sid+"/take", nil); code != 200 {
		t.Fatalf("take-over: %d %v", code, out)
	}
	al.driverIs(false, "管理员 root")
	ad.driverIs(true, "管理员 root")
	ad.input("admin-now")
	al.waitText("IN:admin-now")
	al.input("alice-late")
	if m := al.wait(proto.MsgErr); m.Code != proto.ErrReadOnly {
		t.Fatalf("owner input while taken over: %+v", m)
	}
	var n int
	f.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE event='session_take' AND object=?`, sid).Scan(&n)
	if n != 1 {
		t.Fatalf("take-over audited %d times", n)
	}

	// The owner takes it back, no second factor needed.
	if code, _ := alice.call("POST", "/api/sessions/"+sid+"/take", nil); code != 200 {
		t.Fatalf("take back: %d", code)
	}
	al.driverIs(true, "")
	ad.driverIs(false, "")

	// Taken over again, then the administrator leaves: back to the owner.
	f.db.Exec(`UPDATE login_sessions SET reverified_until=? WHERE user_id=1`, time.Now().Add(10*time.Minute).Unix())
	admin.call("POST", "/api/sessions/"+sid+"/take", nil)
	al.driverIs(false, "管理员 root")
	ad.ws.Close()
	al.driverIs(true, "")
	al.input("mine-again")
	al.waitText("IN:mine-again")

	// Nobody else takes it: another user gets the same 404 as for a stranger.
	_, out = admin.call("POST", "/api/admin/users", map[string]string{"username": "bob", "role": "user"})
	bob := f.login("bob", out["temp_password"].(string), false)
	if code, _ := bob.call("POST", "/api/sessions/"+sid+"/take", nil); code != 404 {
		t.Fatalf("bob took alice's session: %d", code)
	}
}

// One AI CLI of a kind per folder and machine, whoever starts it; Claude Code
// and Codex do not count against each other, a plain shell is not limited.
func TestOneCLIPerFolder(t *testing.T) {
	f := newFixture(t)
	admin := f.login("root", pw, true)
	token, custom := f.nodeWithProfile(admin, "pc1")
	f.dialNode(token, "fp", nil)
	waitOnline(t, f.hub, 1, true)
	profile := func(name, kind string) int64 {
		code, out := admin.call("POST", "/api/admin/profiles", Profile{NodeID: 1, Name: name, Kind: kind, Mode: "direct", Command: "testcli"})
		if code != 200 {
			t.Fatalf("profile %s: %d %v", name, code, out)
		}
		return int64(out["profile"].(map[string]any)["id"].(float64))
	}
	claude, claude2, codex := profile("c1", "claude"), profile("c2", "claude"), profile("x1", "codex")
	_, out := admin.call("POST", "/api/admin/users", map[string]string{"username": "alice", "role": "user"})
	alice := f.login("alice", out["temp_password"].(string), false)
	admin.call("PUT", "/api/admin/profiles/"+itoa(claude2)+"/bindings", map[string]any{"user_ids": []int64{2}})
	start := func(u *user, pid int64, cwd string) (int, map[string]any) {
		return u.call("POST", "/api/sessions", map[string]any{"profile_id": pid, "cwd": cwd})
	}

	code, out := start(admin, claude, `C:\work`)
	if code != 200 {
		t.Fatalf("first: %d %v", code, out)
	}
	first := out["session"].(map[string]any)["sid"].(string)
	for _, c := range []struct {
		u   *user
		pid int64
		cwd string
		who string
	}{{admin, claude, `c:\WORK\`, "你"}, {admin, claude2, `C:/work`, "你"}, {admin, claude, `C:\work\sub\..`, "你"},
		{alice, claude2, `C:\work`, "另一位用户"}} {
		code, out := start(c.u, c.pid, c.cwd)
		if code != 409 || out["code"] != proto.ErrFolderBusy || !strings.HasPrefix(out["msg"].(string), c.who+" ") || strings.Contains(out["msg"].(string), "root") {
			t.Fatalf("second Claude Code in %s: %d %v", c.cwd, code, out)
		}
	}
	for _, c := range []struct {
		pid int64
		cwd string
	}{{codex, `C:\work`}, {custom, `C:\work`}, {custom, `C:\work`}, {claude, `C:\work\sub`}} {
		if code, out := start(admin, c.pid, c.cwd); code != 200 {
			t.Fatalf("%d in %s must start: %d %v", c.pid, c.cwd, code, out)
		}
	}
	// Codex first, then Claude Code: apart as well; a second Codex is not.
	if code, _ := start(admin, codex, `C:\cx`); code != 200 {
		t.Fatal("codex first")
	}
	if code, _ := start(admin, claude, `C:\cx`); code != 200 {
		t.Fatal("Claude Code beside Codex")
	}
	if code, _ := start(admin, codex, `C:\cx`); code != 409 {
		t.Fatal("a second Codex")
	}
	// Two presses at once in a free folder: one session.
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); c, _ := start(admin, claude, `C:\race`); codes <- c }()
	}
	wg.Wait()
	close(codes)
	ok := 0
	for c := range codes {
		if c == 200 {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("%d sessions started in one folder at once", ok)
	}
	// Ended: the folder is free again.
	admin.call("DELETE", "/api/sessions/"+first, nil)
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if row, _ := f.reg.session(first); row.EndedAt != 0 {
			break
		}
	}
	if code, out := start(alice, claude2, `C:\work`); code != 200 {
		t.Fatalf("after the first ended: %d %v", code, out)
	}
	// The kind a session started as holds, though its profile is deleted or
	// changed while it runs (Codex 第五轮 2).
	c3, c4 := profile("c3", "claude"), profile("c4", "claude")
	if code, _ := start(admin, c3, `C:\del`); code != 200 {
		t.Fatal("c3")
	}
	if code, _ := start(admin, c4, `C:\kind`); code != 200 {
		t.Fatal("c4")
	}
	admin.call("DELETE", "/api/admin/profiles/"+itoa(c3), nil)
	admin.call("POST", "/api/admin/profiles", Profile{ID: c4, NodeID: 1, Name: "c4", Kind: "custom", Mode: "direct", Command: "testcli"})
	for _, cwd := range []string{`C:\del`, `C:\kind`} {
		if code, out := start(admin, claude, cwd); code != 409 {
			t.Fatalf("second Claude Code in %s after its profile changed: %d %v", cwd, code, out)
		}
	}
	if code, _ := start(admin, codex, `C:\kind`); code != 200 {
		t.Fatal("Codex beside a Claude Code whose profile became custom")
	}
}
