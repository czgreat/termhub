//go:build linux

package session

import (
	"crypto/sha256"
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

	"golang.org/x/sys/unix"
)

var testcli string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "termhub-session-test")
	if err != nil {
		panic(err)
	}
	testcli = filepath.Join(dir, "testcli")
	gobin := filepath.Join(runtime.GOROOT(), "bin", "go")
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
	if len(text) > 800 {
		text = text[len(text)-800:]
	}
	t.Fatalf("timed out waiting for %q; output so far:\n%q", pattern, text)
	return ""
}

func start(t *testing.T, spec Spec) *Session {
	t.Helper()
	if spec.Cwd == "" {
		spec.Cwd = t.TempDir()
	}
	if spec.Cols == 0 {
		spec.Cols, spec.Rows = 100, 30
	}
	s, err := Start(spec)
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

func direct(t *testing.T, args ...string) *Session {
	return start(t, Spec{Mode: ModeDirect, Command: testcli, Args: args})
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

func isCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

func pidAlive(pid int) bool {
	st, ok := readStat(pid)
	return ok && st.running()
}

func TestLinuxStartEchoAndExitCode(t *testing.T) {
	s := direct(t)
	waitText(t, s, `READY`, 15*time.Second)
	if err := s.Write([]byte("echo 中文 and 😀\r")); err != nil {
		t.Fatal(err)
	}
	waitText(t, s, `ECHO "中文 and 😀"`, 10*time.Second)
	if info := s.Info(); info.State != "running" || info.PID == 0 || info.CodePageUnset || info.JobEscapeRisk {
		t.Fatalf("info: %+v", info)
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

func TestLinuxStartErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := Start(Spec{Mode: ModeDirect, Command: "no-such-program-xyz", Cwd: dir}); !isCode(err, CodeCommandNotFound) {
		t.Fatalf("missing program: %v", err)
	}
	if _, err := Start(Spec{Mode: ModeDirect, Command: testcli, Cwd: filepath.Join(dir, "nope")}); !isCode(err, CodeCwdNotFound) {
		t.Fatalf("missing folder: %v", err)
	}
	notExec := filepath.Join(dir, "script")
	os.WriteFile(notExec, []byte("#!/bin/sh\n"), 0o644)
	if _, err := Start(Spec{Mode: ModeDirect, Command: notExec, Cwd: dir}); !isCode(err, CodeCommandNotFound) {
		t.Fatalf("not executable: %v", err)
	}
}

// Arguments reach the program exactly, directly and through a shell's -c,
// whatever quotes, dollars or spaces they hold.
func TestLinuxArgumentsVerbatim(t *testing.T) {
	args := []string{"plain", "two words", `it's "quoted"`, "$HOME `x` $(y)", "", "a\\b;c|d&e"}
	for _, spec := range []Spec{
		{Mode: ModeDirect, Command: testcli, Args: args},
		{Mode: ModeShell, ShellPath: "bash", Command: testcli, Args: args},
		{Mode: ModeShell, ShellPath: "/bin/sh", Command: testcli, Args: args},
	} {
		s := start(t, spec)
		waitText(t, s, `READY`, 15*time.Second)
		for i, a := range args {
			waitText(t, s, regexp.QuoteMeta(fmt.Sprintf("ARG %d=%q", i, a)), 5*time.Second)
		}
	}
}

// Shell mode: the CLI runs as a job of an interactive login shell, and when
// it exits the user is back at a prompt of the same shell.
func TestLinuxShellModeReturnsToPrompt(t *testing.T) {
	s := start(t, Spec{Mode: ModeShell, ShellPath: "bash", Command: testcli})
	waitText(t, s, `READY`, 15*time.Second)
	s.Write([]byte("quit 3\r"))
	time.Sleep(500 * time.Millisecond)
	s.Write([]byte("echo marker-$((40+2))\r"))
	waitText(t, s, `marker-42`, 10*time.Second)
	if st := s.Info().State; st != "running" {
		t.Fatalf("shell gone: %s", st)
	}
}

func TestLinuxEnvironment(t *testing.T) {
	t.Setenv("JOURNAL_STREAM", "8:1234")
	t.Setenv("TERMHUB_SECRET", "x")
	t.Setenv("TERM", "dumb")
	s := start(t, Spec{Mode: ModeShell, ShellPath: "/bin/sh", Env: map[string]string{"TH_PROFILE_VAR": "yes"}})
	s.Write([]byte(`echo "T=$TERM J=${JOURNAL_STREAM:-none} S=${TERMHUB_SECRET:-none} P=$TH_PROFILE_VAR TP=$TERM_PROGRAM"` + "\r"))
	waitText(t, s, `T=xterm-256color J=none S=none P=yes TP=termhub`, 10*time.Second)
}

func TestLinuxResizeReachesProgram(t *testing.T) {
	s := start(t, Spec{Mode: ModeShell, ShellPath: "/bin/sh"})
	s.Write([]byte("stty size\r"))
	waitText(t, s, `30 100`, 10*time.Second)
	if err := s.Resize(81, 21); err != nil {
		t.Fatal(err)
	}
	s.Write([]byte("stty size\r"))
	waitText(t, s, `21 81`, 10*time.Second)
}

// A descendant that left the terminal session (setsid) and was orphaned is
// still found and killed: tier 3, the counterpart of escaping the Job.
func TestLinuxCleanupKillsEscapedOrphans(t *testing.T) {
	s := start(t, Spec{Mode: ModeShell, ShellPath: "bash"})
	// setsid -w keeps the escaped process's parent alive, so the tracker sees
	// it as a descendant on a tick. One orphaned before any tick is not found,
	// as with a process that escapes the Job on Windows (docs/M2 第 9 节).
	s.Write([]byte("setsid -w sh -c 'echo ESCAPED=$$; exec sleep 3600' & sleep 3600 & echo INSESSION=$!\r"))
	escaped, _ := strconv.Atoi(strings.TrimPrefix(waitText(t, s, `ESCAPED=\d+`, 10*time.Second), "ESCAPED="))
	inSession, _ := strconv.Atoi(strings.TrimPrefix(waitText(t, s, `INSESSION=\d+`, 10*time.Second), "INSESSION="))
	if st, ok := readStat(escaped); !ok || st.sid == int(s.Info().PID) {
		t.Fatalf("test setup: %d should have its own session: %+v", escaped, st)
	}
	time.Sleep(3 * time.Second) // one tracker tick
	s.Close(Graceful, ReasonUser)
	info := finish(t, s, 20*time.Second)
	if info.Reason != ReasonUser || len(info.Leftover) != 0 {
		t.Fatalf("exit info: %+v", info)
	}
	for _, pid := range []int{escaped, inSession} {
		for end := time.Now().Add(5 * time.Second); pidAlive(pid) && time.Now().Before(end); {
			time.Sleep(100 * time.Millisecond)
		}
		if pidAlive(pid) {
			syscallKill(pid)
			t.Fatalf("%d survived", pid)
		}
	}
}

func TestLinuxCleanupKillsOrphanedChain(t *testing.T) {
	s := direct(t, "-spawn", "3")
	waitText(t, s, `SPAWNED \d+`, 15*time.Second)
	var orphan int
	for end := time.Now().Add(20 * time.Second); time.Now().Before(end) && orphan == 0; time.Sleep(200 * time.Millisecond) {
		for _, p := range processTable() {
			root := int(s.Info().PID)
			if p.name == "testcli" && p.pid != root && p.ppid != root && p.sid == root && p.running() { // re-parented away: an orphan
				orphan = p.pid
			}
		}
	}
	if orphan == 0 {
		t.Fatal("test setup: no orphaned descendant found")
	}
	s.Close(Graceful, ReasonUser)
	finish(t, s, 20*time.Second)
	for end := time.Now().Add(5 * time.Second); pidAlive(orphan) && time.Now().Before(end); {
		time.Sleep(100 * time.Millisecond)
	}
	if pidAlive(orphan) {
		syscallKill(orphan)
		t.Fatalf("orphan %d survived", orphan)
	}
}

// A program that ignores SIGHUP and SIGTERM is still gone after a forced
// close, quickly.
func TestLinuxForceCloseIgnoringProgram(t *testing.T) {
	s := direct(t, "-ignore-close")
	waitText(t, s, `READY`, 15*time.Second)
	pid := int(s.Info().PID)
	begin := time.Now()
	s.Close(Force, ReasonIdle)
	info := finish(t, s, 15*time.Second)
	if info.Reason != ReasonIdle || pidAlive(pid) {
		t.Fatalf("info %+v alive=%v", info, pidAlive(pid))
	}
	if d := time.Since(begin); d > 6*time.Second {
		t.Fatalf("forced close took %v", d)
	}
}

func TestLinuxFastExitKeepsOutput(t *testing.T) {
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

// A large paste arrives whole: the input queue feeds the pty in pieces while
// the program reads raw bytes.
func TestLinuxLargeInputArrivesWhole(t *testing.T) {
	s := direct(t)
	waitText(t, s, `READY`, 15*time.Second)
	data := []byte(strings.Repeat("0123456789abcdef", 64<<10/16)) // 64 KiB
	s.Write([]byte(fmt.Sprintf("sink %d\r", len(data))))
	time.Sleep(300 * time.Millisecond)
	if err := s.Write(data); err != nil {
		t.Fatal(err)
	}
	waitText(t, s, fmt.Sprintf("SINK 65536 %x <nil>", sha256.Sum256(data)), 20*time.Second)
}

// The identity check: a pid with another start time is not ours to kill.
func TestLinuxKillIfSame(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	id, ok := IdentifyProcess(uint32(cmd.Process.Pid))
	if !ok || id.Created == 0 {
		t.Fatalf("identify: %+v %v", id, ok)
	}
	if KillIfSame(ProcID{PID: id.PID, Created: id.Created + 1}) {
		t.Fatal("killed a process with another start time")
	}
	if !KillIfSame(id) {
		t.Fatal("did not kill the process")
	}
	cmd.Wait()
}

func TestParseStatOddNames(t *testing.T) {
	st, ok := parseStat(42, []byte("42 (a) b (c)) S 7 42 9 0 -1 4194560 0 0 0 0 0 0 0 0 20 0 1 0 12345 0 0"))
	if !ok || st.name != "a) b (c)" || st.ppid != 7 || st.sid != 9 || st.state != 'S' || st.created != bootMark()|12345 {
		t.Fatalf("%+v %v", st, ok)
	}
}

// A recorded process that ended, and a stranger given its pid: the
// stranger's children are not the session's (复核 PR #40 第 1 条).
func TestProcTreeIgnoresRecycledPid(t *testing.T) {
	const root, gone, kid = 1_000_001, 1_000_002, 1_000_003 // fake pids, only in the snapshot
	tr := &procTree{root: root, procs: map[uint32]*tracked{
		root: {name: "root", created: 10}, gone: {name: "make", created: 20}}}
	tr.scanTable([]procStat{
		{pid: root, ppid: 1, sid: root, state: 'S', created: 10},
		{pid: gone, ppid: 1, sid: 7, state: 'S', created: 90}, // the pid again, another process
		{pid: kid, ppid: gone, sid: 7, state: 'S', created: 95},
	})
	if _, ok := tr.procs[kid]; ok {
		t.Fatal("took in a child of a recycled pid")
	}
	if _, ok := tr.procs[gone]; ok {
		t.Fatal("kept the record of an ended process")
	}
	if _, ok := tr.procs[root]; !ok {
		t.Fatal("dropped the root")
	}

	// The real thing still works: a child of a recorded, living process.
	tr.procs[gone] = &tracked{name: "make", created: 20}
	tr.scanTable([]procStat{
		{pid: root, ppid: 1, sid: root, state: 'S', created: 10},
		{pid: gone, ppid: root, sid: 7, state: 'S', created: 20},
		{pid: kid, ppid: gone, sid: 7, state: 'S', created: 25},
	})
	if _, ok := tr.procs[kid]; !ok {
		t.Fatal("missed a real descendant")
	}

	// The root is never pruned; once its pid is someone else's, that one's
	// children are not taken in either.
	other := &procTree{root: root, procs: map[uint32]*tracked{root: {name: "root", created: 10}}}
	other.scanTable([]procStat{
		{pid: root, ppid: 1, sid: root, state: 'S', created: 70},
		{pid: kid, ppid: root, sid: root, state: 'S', created: 75},
	})
	if _, ok := other.procs[kid]; ok {
		t.Fatal("took in a child of the root's recycled pid")
	}
}

func TestPosixQuote(t *testing.T) {
	for in, want := range map[string]string{"abc": "abc", "/usr/bin/x": "/usr/bin/x", "": "''", "a b": "'a b'", "it's": `'it'\''s'`, "$x": "'$x'"} {
		if got := posixQuote(in); got != want {
			t.Errorf("posixQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func syscallKill(pid int) { unix.Kill(pid, unix.SIGKILL) }
