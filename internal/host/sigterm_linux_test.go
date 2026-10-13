//go:build linux

package host

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"termhub/internal/proto"
)

// SIGTERM to the host program (the uninstall script, the system shutting
// down) ends its sessions properly: a program that ignores SIGHUP does not
// outlive the host (复核 PR #40 第 4 条).
func TestHostProgramEndsSessionsOnSIGTERM(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "termhub-agent")
	if out, err := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", bin, "termhub/cmd/termhub-agent").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	pipe := testPipe(t, "sigterm")
	cmd := exec.Command(bin, "host", "-pipe", pipe, "-state", t.TempDir())
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if c, err := Dial(pipe, time.Second); err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("host did not come up")
		}
		time.Sleep(100 * time.Millisecond)
	}
	// A program that ignores SIGHUP and never reads the terminal: closing
	// the terminal alone does not end it.
	a := dial(t, pipe)
	sleep := fmt.Sprintf("sleep %d", 100000+os.Getpid()) // a command line no one else has
	sid := proto.NewSID()
	if r := a.req(&proto.Msg{T: proto.MsgCreate, SID: &sid, Owner: "tester", Cwd: t.TempDir(), Cols: 100, Rows: 30,
		Profile: &proto.Profile{Name: "sh", Mode: "direct", Command: "/bin/sh", Args: []string{"-c", "trap '' HUP; " + sleep}}}); r.T != proto.MsgOK {
		t.Fatalf("create failed: %+v", r)
	}
	running := func() string {
		out, _ := exec.Command("pgrep", "-fx", sleep).Output()
		return strings.TrimSpace(string(out))
	}
	defer exec.Command("pkill", "-fx", sleep).Run()
	time.Sleep(500 * time.Millisecond)
	if running() == "" {
		t.Fatal("the session's program is not running")
	}

	cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("host did not stop on SIGTERM")
	}
	if out := running(); out != "" {
		t.Fatalf("a session's program outlived the host:\n%s", out)
	}
}
