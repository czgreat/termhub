//go:build windows

package session

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var testcli string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "termhub-session-test")
	if err != nil {
		panic(err)
	}
	testcli = filepath.Join(dir, "testcli.exe")
	gobin := filepath.Join(runtime.GOROOT(), "bin", "go.exe")
	if _, err := os.Stat(gobin); err != nil {
		gobin = "go"
	}
	out, err := exec.Command(gobin, "build", "-o", testcli, "termhub/tools/testcli").CombinedOutput()
	if err != nil {
		fmt.Printf("cannot build testcli: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

var stripANSI = regexp.MustCompile(`\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b\[[0-9;?<>=!]*[ -/]*[@-~]|\x1b[()][0-9A-B]|\x1b[=>]`)

// waitText polls the session's whole retained output for a pattern.
func waitText(t *testing.T, s *Session, pattern string, d time.Duration) string {
	t.Helper()
	re := regexp.MustCompile(pattern)
	var text string
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		text = stripANSI.ReplaceAllString(string(s.Replay(nil).Data), "")
		if m := re.FindString(text); m != "" {
			return m
		}
	}
	t.Fatalf("timed out waiting for %q; output so far:\n%q", pattern, tailOf(text, 600))
	return ""
}

func tailOf(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

func direct(t *testing.T, args ...string) *Session {
	t.Helper()
	s, err := Start(Spec{Mode: ModeDirect, Command: testcli, Args: args, Cwd: t.TempDir(), Cols: 100, Rows: 30})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		s.Close(Force, ReasonUser)
		select {
		case <-s.Done():
		case <-time.After(15 * time.Second):
			t.Error("session did not finish cleanup")
		}
	})
	return s
}

func finish(t *testing.T, s *Session, d time.Duration) ExitInfo {
	t.Helper()
	select {
	case info := <-s.Done():
		s.done <- info // let the Cleanup read it again
		return info
	case <-time.After(d):
		t.Fatal("session did not end")
		return ExitInfo{}
	}
}

func TestStartEchoAndExitCode(t *testing.T) {
	s := direct(t)
	waitText(t, s, `READY`, 15*time.Second)
	if err := s.Write([]byte("echo 中文 and 😀\r")); err != nil {
		t.Fatal(err)
	}
	waitText(t, s, `ECHO "中文 and 😀"`, 10*time.Second)
	if info := s.Info(); info.State != "running" || info.PID == 0 || info.CodePageUnset {
		t.Fatalf("info: %+v", info)
	}
	// A test runner can itself be in a Job (e.g. Codex on Windows). Its child
	// inherits that Job before ours is assigned, which correctly sets this flag.
	var parentInJob int32
	ok, _, err := procIsProcInJob.Call(uintptr(windows.CurrentProcess()), 0, uintptr(unsafe.Pointer(&parentInJob)))
	if ok == 0 {
		t.Fatal(err)
	}
	if parentInJob == 0 && s.Info().JobEscapeRisk {
		t.Fatal("unexpected Job escape risk outside a parent Job")
	}
	s.Write([]byte("quit 7\r"))
	info := finish(t, s, 15*time.Second)
	if info.Reason != ReasonSelf || info.ExitCode != 7 || len(info.Leftover) != 0 {
		t.Fatalf("exit info: %+v", info)
	}
	if err := s.Write([]byte("x")); !isCode(err, CodeNotRunning) {
		t.Fatalf("write after exit: %v", err)
	}
}

func isCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

func TestStartErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := Start(Spec{Mode: ModeDirect, Command: "no-such-program-xyz", Cwd: dir}); !isCode(err, CodeCommandNotFound) {
		t.Errorf("missing command: %v", err)
	}
	if _, err := Start(Spec{Mode: ModeDirect, Command: testcli, Cwd: filepath.Join(dir, "missing")}); !isCode(err, CodeCwdNotFound) {
		t.Errorf("missing cwd: %v", err)
	}
	ps1 := filepath.Join(dir, "tool.ps1")
	os.WriteFile(ps1, []byte("'hi'"), 0o644)
	if _, err := Start(Spec{Mode: ModeDirect, Command: ps1, Cwd: dir}); !isCode(err, CodeUnsupportedScript) {
		t.Errorf("ps1 as root: %v", err)
	}
	if _, err := Start(Spec{Mode: ModeShell, Cwd: dir}); !isCode(err, CodeBadSpec) {
		t.Errorf("shell mode without shell: %v", err)
	}
}

// Live output must be numbered without holes or repeats, and Replay+Subscribe
// must join seamlessly.
func TestSubscribeContinuity(t *testing.T) {
	s := direct(t)
	waitText(t, s, `READY`, 15*time.Second)
	r := s.Replay(nil)
	ch, cancel := s.Subscribe()
	defer cancel()
	s.Write([]byte("seq 0 3000\r"))

	var stream bytes.Buffer
	stream.Write(r.Data)
	next := r.End
	deadline := time.After(30 * time.Second)
	for !bytes.Contains(stream.Bytes(), []byte("SEQ 00002999")) {
		select {
		case c, ok := <-ch:
			if !ok {
				t.Fatal("subscription closed early")
			}
			if c.Gap {
				t.Fatal("unexpected gap for a fast subscriber")
			}
			if c.Offset+uint64(len(c.Data)) <= next {
				continue // produced between Replay and Subscribe: already have it
			}
			if c.Offset > next {
				t.Fatalf("hole: got offset %d, expected %d", c.Offset, next)
			}
			stream.Write(c.Data[next-c.Offset:])
			next = c.Offset + uint64(len(c.Data))
		case <-deadline:
			t.Fatal("timed out")
		}
	}
	text := stripANSI.ReplaceAllString(stream.String(), "")
	for i, m := range regexp.MustCompile(`SEQ (\d{8})`).FindAllStringSubmatch(text, -1) {
		if n, _ := strconv.Atoi(m[1]); n != i {
			t.Fatalf("sequence broken at position %d: got %d", i, n)
		}
	}
}

// Small in-memory boundaries, no console flood on an in-use node.
func TestSlowSubscriberGetsGap(t *testing.T) {
	sub := &subscriber{ch: make(chan Chunk, 1)}
	s := &Session{buf: NewBuffer(128), subs: map[int]*subscriber{0: sub}}
	s.publish([]byte("one"))
	s.publish([]byte("two"))
	s.publish([]byte("three"))
	got := <-sub.ch
	if !got.Gap || string(got.Data) != "three" {
		t.Fatalf("missing gap: %+v", got)
	}
}
func TestReplayCarriesModesAfterEviction(t *testing.T) {
	s := &Session{buf: NewBuffer(128), subs: map[int]*subscriber{}}
	s.publish([]byte("\x1b[?2004h\x1b[?1004h"))
	s.publish(bytes.Repeat([]byte("x"), 256))
	r := s.Replay(nil)
	if r.From == 0 {
		t.Fatal("expected eviction")
	}
	var tr Tracker
	tr.Feed(r.Prelude)
	if !tr.Modes.BracketedPaste || !tr.Modes.FocusEvents {
		t.Fatalf("lost modes: %q", r.Prelude)
	}
}

func TestResizeReachesProgram(t *testing.T) {
	cmd, _ := exec.LookPath("cmd.exe")
	s, err := Start(Spec{Mode: ModeShell, ShellPath: cmd, Cwd: t.TempDir(), Cols: 100, Rows: 30})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close(Force, ReasonUser); <-s.Done() }()
	waitText(t, s, `>`, 15*time.Second)
	if err := s.Resize(91, 27); err != nil {
		t.Fatal(err)
	}
	s.Write([]byte("mode con\r"))
	waitText(t, s, `(?s)27.*91|Columns:\s+91`, 10*time.Second)
	if err := s.Resize(1, 1); !isCode(err, CodeBadSpec) {
		t.Fatalf("out of range resize: %v", err)
	}
	if err := s.Redraw(); err != nil {
		t.Fatal(err)
	}
	if i := s.Info(); i.Cols != 91 || i.Rows != 27 {
		t.Fatalf("size after redraw: %dx%d", i.Cols, i.Rows)
	}
}

// The console must be in UTF-8 without anything having been typed into it.
func TestCodePageIsUTF8(t *testing.T) {
	cmd, _ := exec.LookPath("cmd.exe")
	s, err := Start(Spec{Mode: ModeShell, ShellPath: cmd, Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close(Force, ReasonUser); <-s.Done() }()
	waitText(t, s, `>`, 15*time.Second)
	s.Write([]byte("chcp\r"))
	waitText(t, s, `65001`, 10*time.Second)
}

// Shell mode passes the CLI through the shell's arguments; awkward characters
// must arrive intact. The assertions look at what the program really received.
func TestShellModeQuoting(t *testing.T) {
	// Everything awkward except a double quote, which is refused (see TestDoubleQuoteRule).
	args := []string{"--", `a b&c 中文 $x 100% (p) ^ | <> 'single'`, "plain", ""}
	want := []string{`ARG 0="a b&c 中文 $x 100% (p) ^ | <> 'single'"`, `ARG 1="plain"`, `ARG 2=""`}
	for _, shell := range []string{"cmd.exe", `C:\Program Files\PowerShell\7\pwsh.exe`} {
		t.Run(filepath.Base(shell), func(t *testing.T) {
			path, err := exec.LookPath(shell)
			if err != nil {
				t.Skip("not installed")
			}
			s, err := Start(Spec{Mode: ModeShell, ShellPath: path, Command: testcli, Args: args, Cwd: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { s.Close(Force, ReasonUser); <-s.Done() }()
			waitText(t, s, `READY`, 40*time.Second)
			text := stripANSI.ReplaceAllString(string(s.Replay(nil).Data), "")
			for _, w := range want {
				if !strings.Contains(text, w) {
					t.Fatalf("missing %s in:\n%s", w, tailOf(text, 500))
				}
			}
			// The CLI ends, the shell stays: that is the point of shell mode.
			s.Write([]byte("quit\r"))
			time.Sleep(2 * time.Second)
			if s.Info().State != "running" {
				t.Fatal("shell should stay after the command finished")
			}
		})
	}
}

// An npm-style .cmd shim as the root process: it runs under "cmd /c".
func TestDirectModeCmdShim(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "mycli.cmd")
	os.WriteFile(shim, []byte("@\""+testcli+"\" -- %*\r\n"), 0o644)
	s, err := Start(Spec{Mode: ModeDirect, Command: "mycli", Args: []string{"two words", "a&b"},
		Env: map[string]string{"PATH": dir + ";" + os.Getenv("PATH")}, Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close(Force, ReasonUser); <-s.Done() }()
	waitText(t, s, `READY`, 30*time.Second)
	text := stripANSI.ReplaceAllString(string(s.Replay(nil).Data), "")
	if !strings.Contains(text, `ARG 0="two words"`) || !strings.Contains(text, `ARG 1="a&b"`) {
		t.Fatalf("shim arguments mangled:\n%s", tailOf(text, 400))
	}
	if !strings.HasSuffix(strings.ToLower(s.Info().Exe), "cmd.exe") {
		t.Fatalf("a .cmd must run under cmd.exe, got %s", s.Info().Exe)
	}
}

// Double quotes: fine straight to an .exe, refused through a shell or a script.
func TestDoubleQuoteRule(t *testing.T) {
	dir := t.TempDir()
	quoted := `say "hi" \ end\`
	s, err := Start(Spec{Mode: ModeDirect, Command: testcli, Args: []string{"--", quoted}, Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close(Force, ReasonUser); <-s.Done() }()
	waitText(t, s, `READY`, 20*time.Second)
	if text := string(s.Replay(nil).Data); !strings.Contains(text, `ARG 0="say \"hi\" \\ end\\"`) {
		t.Fatalf("direct exe must receive quotes intact:\n%s", tailOf(text, 300))
	}
	cmd, _ := exec.LookPath("cmd.exe")
	if _, err := Start(Spec{Mode: ModeShell, ShellPath: cmd, Command: testcli, Args: []string{quoted}, Cwd: dir}); !isCode(err, CodeBadSpec) {
		t.Fatalf("shell mode with a quote: %v", err)
	}
	shim := filepath.Join(dir, "q.cmd")
	os.WriteFile(shim, []byte("@echo off\r\n"), 0o644)
	if _, err := Start(Spec{Mode: ModeDirect, Command: shim, Args: []string{quoted}, Cwd: dir}); !isCode(err, CodeBadSpec) {
		t.Fatalf("cmd shim with a quote: %v", err)
	}
}

func pidAlive(pid uint32) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	return windows.GetExitCodeProcess(h, &code) == nil && code == 259
}

// Descendants, including an orphan whose parents already exited, die with the session.
func TestCleanupKillsOrphanedDescendants(t *testing.T) {
	s := direct(t, "-spawn", "3")
	waitText(t, s, `SPAWNED \d+`, 15*time.Second)
	// The chain spawns, the middle links exit, and the tracker must have seen
	// the orphan on one of its ticks: poll instead of guessing a fixed delay
	// (the fixed 3 s was flaky when the whole suite ran at once).
	var orphan uint32
	for end := time.Now().Add(20 * time.Second); time.Now().Before(end) && orphan == 0; {
		for _, list := range processChildren() {
			for _, c := range list {
				if strings.EqualFold(c.name, "testcli.exe") && c.pid != s.Info().PID && pidAlive(c.pid) {
					orphan = c.pid
				}
			}
		}
		if orphan == 0 {
			time.Sleep(200 * time.Millisecond)
		}
	}
	if orphan == 0 {
		t.Fatal("test setup: no orphaned descendant found")
	}
	time.Sleep(3 * time.Second) // one tracker tick after the orphan appeared
	s.Close(Graceful, ReasonUser)
	info := finish(t, s, 20*time.Second)
	if info.Reason != ReasonUser || len(info.Leftover) != 0 {
		t.Fatalf("exit info: %+v", info)
	}
	for end := time.Now().Add(10 * time.Second); pidAlive(orphan) && time.Now().Before(end); {
		time.Sleep(200 * time.Millisecond) // the kill is asynchronous to Close returning
	}
	if pidAlive(orphan) {
		t.Fatalf("orphan %d survived", orphan)
	}
}

// A program that ignores the close event is still gone after a forced close, quickly.
func TestForceCloseIgnoringProgram(t *testing.T) {
	s := direct(t, "-ignore-close")
	waitText(t, s, `READY`, 15*time.Second)
	pid := s.Info().PID
	start := time.Now()
	s.Close(Force, ReasonIdle)
	info := finish(t, s, 15*time.Second)
	if info.Reason != ReasonIdle || pidAlive(pid) {
		t.Fatalf("info %+v alive=%v", info, pidAlive(pid))
	}
	if d := time.Since(start); d > 6*time.Second {
		t.Fatalf("forced close took %v", d)
	}
}

// A handle held on a tracked process pins its pid, so a kill can never land on
// an unrelated process that reused the number.
func TestTrackedHandlePinsIdentity(t *testing.T) {
	s := direct(t)
	waitText(t, s, `READY`, 15*time.Second)
	s.tree.scan()
	s.tree.mu.Lock()
	root := s.tree.procs[s.Info().PID]
	s.tree.mu.Unlock()
	if root == nil || root.created == 0 || !isAlive(root.handle) {
		t.Fatalf("root not tracked: %+v", root)
	}
	// A child claiming to be older than its parent is rejected as a recycled pid.
	s.tree.mu.Lock()
	root.created = 1 << 62
	s.tree.mu.Unlock()
	before := len(s.tree.survivors())
	s.Write([]byte("echo x\r"))
	self := windows.GetCurrentProcessId()
	snapMu.Lock()
	snapKids = map[uint32][]procEntry{s.Info().PID: {{pid: self, name: "impostor"}}}
	snapTime = time.Now().Add(time.Hour)
	snapMu.Unlock()
	s.tree.scan()
	snapMu.Lock()
	snapTime = time.Time{}
	snapMu.Unlock()
	if len(s.tree.survivors()) != before {
		t.Fatal("an older 'child' was accepted into the tree")
	}
}

// A program that prints and exits at once must not lose its output, and the
// session must report its exit code.
func TestFastExitKeepsOutput(t *testing.T) {
	s, err := Start(Spec{Mode: ModeDirect, Command: testcli, Args: []string{"-seq", "5", "-exit-after", "1ms", "-exit-code", "3"}, Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	info := <-s.Done()
	text := stripANSI.ReplaceAllString(string(s.Replay(nil).Data), "")
	if info.Reason != ReasonSelf || info.ExitCode != 3 {
		t.Errorf("exit info: %+v", info)
	}
	if !strings.Contains(text, "SEQ 00000004") {
		t.Errorf("output lost: %q", text)
	}
}

// While the receiver is busy with one title, later frames are coalesced and
// the last one delivered is the newest: Claude Code's final ✳ never loses to
// an older spinner frame (状态点复核 2).
func TestTitlesDeliveredInOrderNewestLast(t *testing.T) {
	s := &Session{titleWake: make(chan struct{}, 1), readDone: make(chan struct{})}
	release := make(chan struct{})
	got := make(chan string, 16)
	first := true
	s.OnTitle(func(title string) {
		if first {
			first = false
			<-release // the Hub link is slow with the first frame
		}
		got <- title
	})
	go s.titleLoop()
	s.setTitle("◐ a")
	time.Sleep(50 * time.Millisecond) // titleLoop is now inside the slow call
	for _, f := range []string{"◓ a", "◑ a", "◒ a", "✳ a"} {
		s.setTitle(f)
	}
	close(release)
	var last string
	for deadline := time.After(2 * time.Second); ; {
		select {
		case last = <-got:
			continue
		case <-time.After(200 * time.Millisecond):
		case <-deadline:
		}
		break
	}
	close(s.readDone)
	if last != "✳ a" {
		t.Fatalf("last title delivered %q, want the newest", last)
	}
}
