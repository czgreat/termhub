//go:build windows

// Package e2e runs the real components together: Hub handlers, agent, session
// host, pseudo console. Only the CLI is the project's test CLI (docs/M12 第 4 节).
package e2e

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"termhub/internal/agent"
	"termhub/internal/host"
	"termhub/internal/hub/auth"
	"termhub/internal/hub/route"
	"termhub/internal/hub/store"
	"termhub/internal/proto"
	"termhub/internal/session"
)

var testcli string

func TestMain(m *testing.M) {
	dir, _ := os.MkdirTemp("", "termhub-e2e")
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

type world struct {
	t      *testing.T
	db     *store.DB
	srv    *httptest.Server
	tlsc   *tls.Config
	client *http.Client
	csrf   string
	cookie string
	pipe   string
	base   string // the Hub's public URL

	agentData string
}

func (w *world) call(method, path string, body any) (int, map[string]any) {
	w.t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, w.base+path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", w.base)
	req.Header.Set("X-TH-CSRF", w.csrf)
	resp, err := w.client.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func newWorld(t *testing.T) *world {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	svc, _ := auth.New(db, make([]byte, 32), auth.Config{Hash: auth.HashParams{MemoryKiB: 64, Time: 1, Threads: 1}})
	mux := http.NewServeMux()
	srv := httptest.NewTLSServer(auth.SecurityHeaders(mux))
	t.Cleanup(srv.Close)
	web, _ := auth.NewHTTP(svc, srv.URL, nil, quiet)
	seal, _ := auth.NewSealer(make([]byte, 32))
	hub := route.NewHub(route.NewRegistry(db, seal, nil), web, route.Limits{}, quiet)
	web.Register(mux)
	hub.Register(mux)

	jar, _ := cookiejar.New(nil)
	c := *srv.Client()
	c.Jar = jar
	w := &world{t: t, db: db, srv: srv, base: srv.URL, client: &c, tlsc: srv.Client().Transport.(*http.Transport).TLSClientConfig,
		pipe: fmt.Sprintf(`\\.\pipe\termhub-e2e-%d`, time.Now().UnixNano())}
	const pw = "correct horse battery"
	svc.Setup(svc.SetupToken(), "root", pw, "")
	if code, _ := w.call("POST", "/api/auth/login", map[string]string{"username": "root", "password": pw}); code != 200 {
		t.Fatalf("login: %d", code)
	}
	// The forced first-login steps are covered by the auth tests; skip them here.
	db.Exec(`UPDATE users SET totp_confirmed_at=1, totp_secret=x'00'`)
	db.Exec(`UPDATE login_sessions SET reverified_until=?`, time.Now().Add(time.Hour).Unix())
	_, me := w.call("GET", "/api/me", nil)
	w.csrf = me["csrf"].(string)
	req, _ := http.NewRequest("GET", srv.URL, nil)
	for _, ck := range jar.Cookies(req.URL) {
		w.cookie = ck.Name + "=" + ck.Value
	}

	h, err := host.New(host.Config{PipeName: w.pipe, StateDir: t.TempDir(), IdleExit: time.Hour, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Shutdown)
	return w
}

func (w *world) startAgent(token string) context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	if w.agentData == "" {
		w.agentData = w.t.TempDir() // never this machine's real termhub directory
	}
	a := agent.New(agent.Config{HubURL: w.base, Token: token, PipeName: w.pipe, TLS: w.tlsc, DataDir: w.agentData,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	go a.Run(ctx)
	w.t.Cleanup(cancel)
	return cancel
}

func (w *world) waitNode(online bool) {
	w.t.Helper()
	for end := time.Now().Add(15 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		_, out := w.call("GET", "/api/nodes", nil)
		if nodes, _ := out["nodes"].([]any); len(nodes) == 1 && nodes[0].(map[string]any)["online"] == online {
			return
		}
	}
	w.t.Fatalf("node never became online=%v", online)
}

type viewer struct {
	t    *testing.T
	ws   *websocket.Conn
	msgs chan *proto.Msg
	mu   sync.Mutex
	tr   *proto.Tracker
	text bytes.Buffer
	hole bool
}

func (w *world) view(sid string) *viewer {
	w.t.Helper()
	d := websocket.Dialer{TLSClientConfig: w.tlsc}
	ws, _, err := d.Dial("wss"+strings.TrimPrefix(w.base, "https")+"/ws/session/"+sid, http.Header{"Origin": {w.base}, "Cookie": {w.cookie}})
	if err != nil {
		w.t.Fatal(err)
	}
	w.t.Cleanup(func() { ws.Close() })
	v := &viewer{t: w.t, ws: ws, msgs: make(chan *proto.Msg, 1024), tr: proto.NewTracker(0)}
	go func() {
		defer close(v.msgs)
		for {
			kind, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if kind == websocket.BinaryMessage {
				if off, d, err := proto.ParseBrowserOutput(data); err == nil {
					v.mu.Lock()
					fresh, gap := v.tr.Accept(off, d)
					v.hole = v.hole || gap
					v.text.Write(fresh)
					v.mu.Unlock()
				}
				continue
			}
			if m, err := proto.DecodeMsg(data); err == nil {
				if m.T == proto.MsgAttached && m.From != nil && m.Mode == proto.ModeReplay {
					v.mu.Lock()
					v.tr.Reset(*m.From)
					v.text.Reset()
					v.mu.Unlock()
				}
				v.msgs <- m
			}
		}
	}()
	return v
}

func (v *viewer) attach(resume bool) *proto.Msg {
	v.t.Helper()
	m := &proto.Msg{T: proto.MsgAttach, Cols: 100, Rows: 30}
	if resume {
		v.mu.Lock()
		have := v.tr.Have()
		v.mu.Unlock()
		m.Have = &have
	}
	b, _ := proto.EncodeMsg(m)
	v.ws.WriteMessage(websocket.TextMessage, b)
	return v.wait(proto.MsgAttached)
}

func (v *viewer) wait(typ string) *proto.Msg {
	v.t.Helper()
	deadline := time.After(20 * time.Second)
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

var ansi = regexp.MustCompile(`\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b\[[0-9;?<>=!]*[ -/]*[@-~]|\x1b[()][0-9A-B]|\x1b[=>]`)

func (v *viewer) plain() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return ansi.ReplaceAllString(v.text.String(), "")
}

func (v *viewer) waitText(want string) {
	v.t.Helper()
	for end := time.Now().Add(30 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if strings.Contains(v.plain(), want) {
			return
		}
	}
	s := v.plain()
	v.t.Fatalf("never saw %q; tail: %q", want, s[max(0, len(s)-300):])
}

func (v *viewer) typeIn(s string) { v.ws.WriteMessage(websocket.BinaryMessage, []byte(s)) }

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

// Browser -> Hub -> agent -> host -> pseudo console and back, surviving an
// agent crash without losing or repeating a byte (docs/M1 验收 3、4、7; M3 验收 1).
func TestTerminalThroughTheWholeStack(t *testing.T) {
	w := newWorld(t)
	_, out := w.call("POST", "/api/admin/nodes", map[string]string{"name": "this-pc"})
	token := out["token"].(string)
	_, out = w.call("POST", "/api/admin/profiles", route.Profile{NodeID: 1, Name: "testcli", Mode: "direct", Command: testcli,
		Args: []string{"-modes", "paste,focus", "-title", "e2e"}})
	pid := out["profile"].(map[string]any)["id"]

	stopAgent := w.startAgent(token)
	w.waitNode(true)

	code, out := w.call("POST", "/api/sessions", map[string]any{"profile_id": pid, "cwd": t.TempDir(), "cols": 100, "rows": 30})
	if code != 200 {
		t.Fatalf("create session: %d %v", code, out)
	}
	sid := out["session"].(map[string]any)["sid"].(string)

	v := w.view(sid)
	att := v.attach(false)
	if att.Mode != proto.ModeReplay || att.Cols != 100 || att.Rows != 30 {
		t.Fatalf("attached: %+v", att)
	}
	v.waitText("READY")
	v.typeIn("echo 你好 termhub\r")
	v.waitText(`ECHO "你好 termhub"`)
	v.typeIn("seq 0 300\r")
	v.waitText("SEQ 00000299")

	// The agent crashes. The session must not notice.
	stopAgent()
	v.wait(proto.MsgNodeOffline)
	w.waitNode(false)
	w.startAgent(token)
	v.wait(proto.MsgNodeOnline)
	if a := v.attach(true); a.Mode != proto.ModeResume {
		t.Fatalf("after the agent came back the viewer should resume, got %+v", a)
	}
	v.typeIn("seq 300 300\r")
	v.waitText("SEQ 00000599")
	if v.hole {
		t.Fatal("the viewer's stream had a hole")
	}
	checkSequence(t, v.plain(), 600)

	// A second device joining late still learns the terminal modes set at startup.
	v2 := w.view(sid)
	a2 := v2.attach(false)
	var tr session.Tracker
	tr.Feed(a2.Prelude)
	v2.waitText("SEQ 00000599")
	v2.mu.Lock()
	tr.Feed(v2.text.Bytes())
	v2.mu.Unlock()
	if !tr.Modes.BracketedPaste || !tr.Modes.FocusEvents || tr.Modes.Title != "e2e" {
		t.Fatalf("late joiner's terminal state is wrong: %+v", tr.Modes)
	}
	checkSequence(t, v2.plain(), 600)

	// Ending it from the browser side ends the real process.
	if code, _ := w.call("DELETE", "/api/sessions/"+sid, nil); code != 200 {
		t.Fatalf("close: %d", code)
	}
	ex := v.wait(proto.MsgExited)
	if ex.Reason != "user" {
		t.Fatalf("exit notice: %+v", ex)
	}
	_, out = w.call("GET", "/api/sessions", nil)
	if out["sessions"] != nil {
		t.Fatalf("session still listed: %v", out)
	}
}
