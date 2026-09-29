//go:build windows

package host

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"

	"termhub/internal/proto"
	"termhub/internal/session"
)

var testcli string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "termhub-host-test")
	if err != nil {
		panic(err)
	}
	testcli = filepath.Join(dir, "testcli.exe")
	gobin := filepath.Join(runtime.GOROOT(), "bin", "go.exe")
	if _, err := os.Stat(gobin); err != nil {
		gobin = "go"
	}
	if out, err := exec.Command(gobin, "build", "-o", testcli, "termhub/tools/testcli").CombinedOutput(); err != nil {
		fmt.Printf("cannot build testcli: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func startHost(t *testing.T, tick time.Duration) (*Host, string) {
	t.Helper()
	pipe := fmt.Sprintf(`\\.\pipe\termhub-test-%d-%d`, os.Getpid(), time.Now().UnixNano())
	h, err := New(Config{PipeName: pipe, StateDir: t.TempDir(), Tick: tick, IdleExit: time.Hour,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Shutdown)
	return h, pipe
}

// agent is a minimal test double for the real agent.
type agent struct {
	t      *testing.T
	conn   net.Conn
	msgs   chan *proto.Msg
	frames chan proto.Frame
	nextID uint64
	stash  []*proto.Msg
}

func dial(t *testing.T, pipe string) *agent {
	t.Helper()
	d := 5 * time.Second
	conn, err := winio.DialPipe(pipe, &d)
	if err != nil {
		t.Fatal(err)
	}
	a := &agent{t: t, conn: conn, msgs: make(chan *proto.Msg, 4096), frames: make(chan proto.Frame, 8192)}
	go func() {
		defer close(a.msgs)
		for {
			raw, err := proto.ReadPipeFrame(conn)
			if err != nil {
				return
			}
			f, err := proto.ParseFrame(raw)
			if err != nil {
				return
			}
			if f.Type == proto.TypeControl {
				if m, err := proto.DecodeMsg(f.Data); err == nil {
					a.msgs <- m
				}
				continue
			}
			f.Data = append([]byte(nil), f.Data...)
			a.frames <- f
		}
	}()
	t.Cleanup(func() { conn.Close() })
	w := a.req(&proto.Msg{T: proto.MsgHello, Proto: proto.Current.String(), AgentVer: "test"})
	if w.T != proto.MsgWelcome {
		t.Fatalf("expected welcome, got %+v", w)
	}
	return a
}

func (a *agent) write(f proto.Frame) {
	a.t.Helper()
	b, err := proto.AppendFrame(nil, f)
	if err == nil {
		err = proto.WritePipeFrame(a.conn, b)
	}
	if err != nil {
		a.t.Fatalf("write: %v", err)
	}
}

func (a *agent) send(m *proto.Msg) {
	a.t.Helper()
	b, err := proto.EncodeMsg(m)
	if err != nil {
		a.t.Fatal(err)
	}
	a.write(proto.Frame{Type: proto.TypeControl, Data: b})
}

// req sends a request and returns its answer; other messages are stashed.
func (a *agent) req(m *proto.Msg) *proto.Msg {
	a.t.Helper()
	a.nextID++
	m.ID = a.nextID
	a.send(m)
	return a.wait(func(r *proto.Msg) bool { return r.Re == m.ID }, 20*time.Second)
}

func (a *agent) wait(match func(*proto.Msg) bool, d time.Duration) *proto.Msg {
	a.t.Helper()
	for i, m := range a.stash {
		if match(m) {
			a.stash = append(a.stash[:i], a.stash[i+1:]...)
			return m
		}
	}
	deadline := time.After(d)
	for {
		select {
		case m, ok := <-a.msgs:
			if !ok {
				a.t.Fatal("connection closed while waiting")
			}
			if match(m) {
				return m
			}
			a.stash = append(a.stash, m)
		case <-deadline:
			a.t.Fatal("timed out waiting for a message")
		}
	}
}

func ofType(t string) func(*proto.Msg) bool { return func(m *proto.Msg) bool { return m.T == t } }

func (a *agent) create(args []string, idleSeconds int) proto.SID {
	a.t.Helper()
	sid := proto.NewSID()
	r := a.req(&proto.Msg{T: proto.MsgCreate, SID: &sid, Owner: "tester", Cwd: a.t.TempDir(), Cols: 100, Rows: 30,
		Profile: &proto.Profile{Name: "testcli", Mode: "direct", Command: testcli, Args: args, IdleTimeout: idleSeconds}})
	if r.T != proto.MsgOK {
		a.t.Fatalf("create failed: %+v", r)
	}
	return sid
}

func (a *agent) forward(sid proto.SID, from *uint64) uint64 {
	a.t.Helper()
	on := true
	r := a.req(&proto.Msg{T: proto.MsgForward, SID: &sid, On: &on, From: from})
	if r.T != proto.MsgOK || r.From == nil {
		a.t.Fatalf("forward failed: %+v", r)
	}
	return *r.From
}

// collect applies output frames to a tracker until the text matches.
func (a *agent) collect(tr *proto.Tracker, buf *bytes.Buffer, pattern string, d time.Duration) {
	a.t.Helper()
	re := regexp.MustCompile(pattern)
	deadline := time.After(d)
	for !re.Match(buf.Bytes()) {
		select {
		case f := <-a.frames:
			if f.Type != proto.TypeOutput {
				continue
			}
			fresh, gap := tr.Accept(f.Offset, f.Data)
			if gap {
				a.t.Fatalf("gap: frame at %d, have %d", f.Offset, tr.Have())
			}
			buf.Write(fresh)
		case <-deadline:
			a.t.Fatalf("timed out waiting for %q; got %q", pattern, tail(buf.String(), 300))
		}
	}
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

func checkSequence(t *testing.T, text string, count int) {
	t.Helper()
	ms := regexp.MustCompile(`SEQ (\d{8})`).FindAllStringSubmatch(text, -1)
	if len(ms) != count {
		t.Fatalf("expected %d numbered lines, got %d", count, len(ms))
	}
	for i, m := range ms {
		if n, _ := strconv.Atoi(m[1]); n != i {
			t.Fatalf("sequence broken at %d: got %d", i, n)
		}
	}
}

// The reason this module exists: the agent dies, the session does not, and a
// new agent picks the output up exactly where the old one stopped.
func TestSessionSurvivesAgentAndResumes(t *testing.T) {
	_, pipe := startHost(t, time.Second)
	a1 := dial(t, pipe)
	sid := a1.create(nil, 0)
	start := a1.forward(sid, new(uint64)) // from offset 0
	tr, out := proto.NewTracker(start), &bytes.Buffer{}
	a1.write(proto.Frame{Type: proto.TypeInput, SID: sid, Data: []byte("seq 0 200\r")})
	a1.collect(tr, out, `SEQ 00000199`, 20*time.Second)
	a1.conn.Close() // the agent crashes

	time.Sleep(500 * time.Millisecond)
	a2 := dial(t, pipe)
	list := a2.wait(ofType(proto.MsgSessions), 10*time.Second)
	if len(list.List) != 1 || list.List[0].SID != sid || list.List[0].State != "running" || list.List[0].Owner != "tester" {
		t.Fatalf("session list after reconnect: %+v", list.List)
	}
	have := tr.Have()
	if got := a2.forward(sid, &have); got != have {
		t.Fatalf("resume should start at %d, started at %d", have, got)
	}
	a2.write(proto.Frame{Type: proto.TypeInput, SID: sid, Data: []byte("seq 200 200\r")})
	a2.collect(tr, out, `SEQ 00000399`, 20*time.Second)
	checkSequence(t, out.String(), 400)
}

func TestSecondHostIsRefused(t *testing.T) {
	_, pipe := startHost(t, time.Second)
	if h2, err := New(Config{PipeName: pipe, StateDir: t.TempDir()}); err == nil {
		h2.Shutdown()
		t.Fatal("a second host on the same pipe must fail")
	}
	dial(t, pipe) // the first one still serves
}

func TestNewAgentReplacesOld(t *testing.T) {
	_, pipe := startHost(t, time.Second)
	a1 := dial(t, pipe)
	dial(t, pipe)
	select {
	case _, ok := <-a1.msgs:
		for ok {
			_, ok = <-a1.msgs
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the old connection was not closed")
	}
}

// Idle time runs only while the agent says the Hub link is up and nobody watches.
func TestIdleTimeout(t *testing.T) {
	_, pipe := startHost(t, 50*time.Millisecond)
	a := dial(t, pipe)
	idle := a.create(nil, 1)
	watched := a.create(nil, 1)
	a.forward(watched, nil)

	time.Sleep(2500 * time.Millisecond) // link is down: nothing may expire
	if l := a.req(&proto.Msg{T: proto.MsgSessions}); len(l.List) != 2 {
		t.Fatalf("a session expired while the link was down: %+v", l.List)
	}
	up := true
	a.send(&proto.Msg{T: proto.MsgLink, Up: &up})
	ex := a.wait(ofType(proto.MsgSessionExited), 10*time.Second)
	if *ex.SID != idle || ex.Reason != session.ReasonIdle {
		t.Fatalf("expected the unwatched session to idle out, got %+v", ex)
	}
	time.Sleep(1500 * time.Millisecond)
	if l := a.req(&proto.Msg{T: proto.MsgSessions}); len(l.List) != 1 || l.List[0].SID != watched {
		t.Fatalf("the watched session must stay: %+v", l.List)
	}
}

func TestReplayRequest(t *testing.T) {
	_, pipe := startHost(t, time.Second)
	a := dial(t, pipe)
	sid := a.create([]string{"-modes", "paste,focus"}, 0)
	time.Sleep(1500 * time.Millisecond)
	a.nextID++
	a.send(&proto.Msg{T: proto.MsgReplay, ID: a.nextID, SID: &sid, Req: 9})
	begin := a.wait(ofType(proto.MsgReplayBegin), 10*time.Second)
	end := a.wait(ofType(proto.MsgReplayEnd), 10*time.Second)
	if begin.Req != 9 || end.Req != 9 || begin.Mode != proto.ModeReplay || begin.Cols != 100 || begin.Rows != 30 {
		t.Fatalf("begin %+v end %+v", begin, end)
	}
	var tr session.Tracker
	tr.Feed(begin.Prelude)
	var data bytes.Buffer
	next := *begin.From
	for next < end.End {
		f := <-a.frames
		if f.Type != proto.TypeReplay || f.Req != 9 || f.Offset != next {
			t.Fatalf("unexpected frame type=%#x req=%d offset=%d want %d", f.Type, f.Req, f.Offset, next)
		}
		data.Write(f.Data)
		next += uint64(len(f.Data))
	}
	tr.Feed(data.Bytes())
	if !tr.Modes.BracketedPaste || !tr.Modes.FocusEvents || !bytes.Contains(data.Bytes(), []byte("READY")) {
		t.Fatalf("replay incomplete: modes %+v data %q", tr.Modes, tail(data.String(), 200))
	}
}

func TestErrorsReachTheAgent(t *testing.T) {
	_, pipe := startHost(t, time.Second)
	a := dial(t, pipe)
	sid := proto.NewSID()
	r := a.req(&proto.Msg{T: proto.MsgCreate, SID: &sid, Cwd: t.TempDir(), Profile: &proto.Profile{Mode: "direct", Command: "no-such-cli"}})
	if r.T != proto.MsgErr || r.Code != session.CodeCommandNotFound {
		t.Fatalf("got %+v", r)
	}
	if r := a.req(&proto.Msg{T: proto.MsgResize, SID: &sid, Cols: 80, Rows: 24}); r.Code != proto.ErrNotFound {
		t.Fatalf("resize of unknown session: %+v", r)
	}
	if r := a.req(&proto.Msg{T: "from_the_future"}); r.Code != proto.ErrUnsupported {
		t.Fatalf("unknown request: %+v", r)
	}
	real := a.create(nil, 0)
	if r := a.req(&proto.Msg{T: proto.MsgCreate, SID: &real, Cwd: t.TempDir(), Profile: &proto.Profile{Mode: "direct", Command: testcli}}); r.Code != proto.ErrBadRequest {
		t.Fatalf("duplicate session id: %+v", r)
	}
}

// Garbage on the pipe costs the sender its connection, never the host or its sessions.
func TestMalformedInputKeepsHost(t *testing.T) {
	_, pipe := startHost(t, time.Second)
	a := dial(t, pipe)
	sid := a.create(nil, 0)
	d := 5 * time.Second
	for _, junk := range [][]byte{
		{0xff, 0xff, 0xff, 0xff},             // absurd length
		{0, 0, 0, 3, 0x7f, 1, 2},             // unknown frame type
		{0, 0, 0, 5, 0x10, '{', '{', '{', 0}, // control frame that is not JSON
		{0, 0, 0, 2, 0x01, 0x00},             // truncated output frame
	} {
		c, err := winio.DialPipe(pipe, &d)
		if err != nil {
			t.Fatal(err)
		}
		c.Write(junk)
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		io.Copy(io.Discard, c) // returns once the host drops us
		c.Close()
	}
	a2 := dial(t, pipe)
	if l := a2.wait(ofType(proto.MsgSessions), 10*time.Second); len(l.List) != 1 || l.List[0].SID != sid {
		t.Fatalf("session lost: %+v", l.List)
	}
}

func TestShutdownRules(t *testing.T) {
	h, pipe := startHost(t, time.Second)
	a := dial(t, pipe)
	a.create(nil, 0)
	if r := a.req(&proto.Msg{T: proto.MsgShutdown}); r.Code != proto.ErrBusy {
		t.Fatalf("shutdown with sessions must be refused: %+v", r)
	}
	if r := a.req(&proto.Msg{T: proto.MsgShutdown, Force: true}); r.T != proto.MsgOK {
		t.Fatalf("forced shutdown: %+v", r)
	}
	done := make(chan struct{})
	go func() { h.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("host did not stop")
	}
	if out, _ := exec.Command("tasklist", "/FI", "IMAGENAME eq testcli.exe", "/FO", "CSV", "/NH").Output(); bytes.Contains(out, []byte(filepath.Base(testcli))) {
		// Other tests run sessions too, but not in parallel with this one.
		t.Fatalf("testcli still running after shutdown:\n%s", out)
	}
}

// A crashed host's escaped processes are killed by the next host, by identity not by pid.
func TestReapLeftovers(t *testing.T) {
	spawn := func() *exec.Cmd {
		c := exec.Command(testcli, "-role", "child", "-spawn", "0", "-child-life", "1m")
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Process.Kill() })
		return c
	}
	victim, bystander := spawn(), spawn()
	time.Sleep(300 * time.Millisecond)
	vid, ok1 := session.IdentifyProcess(uint32(victim.Process.Pid))
	bid, ok2 := session.IdentifyProcess(uint32(bystander.Process.Pid))
	if !ok1 || !ok2 {
		t.Fatal("cannot identify test processes")
	}
	bid.Created++ // same pid, another creation time: "the pid was reused"
	dir := t.TempDir()
	b, _ := json.Marshal(stateFile{Sessions: []stateSession{{SID: proto.NewSID().String(), Procs: []session.ProcID{vid, bid}}}})
	os.WriteFile(filepath.Join(dir, "state.json"), b, 0o600)

	pipe := fmt.Sprintf(`\\.\pipe\termhub-test-reap-%d`, time.Now().UnixNano())
	h, err := New(Config{PipeName: pipe, StateDir: dir, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Shutdown()
	exited := make(chan struct{})
	go func() { victim.Wait(); close(exited) }()
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the recorded leftover was not killed")
	}
	if _, alive := session.IdentifyProcess(uint32(bystander.Process.Pid)); !alive {
		t.Fatal("a process with a different creation time must not be touched")
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json")); err == nil {
		t.Fatal("state file should be consumed")
	}
}
