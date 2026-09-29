//go:build windows

package host

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"termhub/internal/proto"
	"termhub/internal/session"
)

// docs/M3 验收 4 前半: the host process is killed outright. Its Jobs close with
// it, so every session process must vanish without anyone cleaning up.
func TestKilledHostTakesItsSessionsDown(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "termhub-agent.exe")
	gobin := filepath.Join(runtime.GOROOT(), "bin", "go.exe")
	if _, err := os.Stat(gobin); err != nil {
		gobin = "go"
	}
	if out, err := exec.Command(gobin, "build", "-o", exe, "termhub/cmd/termhub-agent").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	pipe := fmt.Sprintf(`\\.\pipe\termhub-test-proc-%d`, time.Now().UnixNano())
	hostProc := exec.Command(exe, "host", "-pipe", pipe, "-state", filepath.Join(dir, "state"))
	if err := hostProc.Start(); err != nil {
		t.Fatal(err)
	}
	defer hostProc.Process.Kill()

	var a *agent
	for i := 0; i < 50 && a == nil; i++ { // wait for the pipe to appear
		time.Sleep(100 * time.Millisecond)
		if _, err := os.Stat(pipe); err == nil {
			a = dial(t, pipe)
		}
	}
	if a == nil {
		t.Fatal("host process did not start serving")
	}
	sid := a.create([]string{"-spawn", "2"}, 0)
	time.Sleep(3 * time.Second) // let the descendants appear and be tracked
	l := a.req(&proto.Msg{T: proto.MsgSessions})
	if len(l.List) != 1 || l.List[0].SID != sid {
		t.Fatalf("sessions: %+v", l.List)
	}

	// Find the session's processes from outside: children of the host process.
	before := testcliPIDs(t)
	if len(before) == 0 {
		t.Fatal("test setup: no testcli processes found")
	}
	if err := hostProc.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	hostProc.Wait()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		alive := 0
		for _, id := range before {
			if now, ok := session.IdentifyProcess(id.PID); ok && now.Created == id.Created {
				alive++
			}
		}
		if alive == 0 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("session processes survived the host: %+v", before)
}

func testcliPIDs(t *testing.T) []session.ProcID {
	t.Helper()
	out, err := exec.Command("pwsh.exe", "-NoProfile", "-Command",
		"(Get-Process testcli -ErrorAction SilentlyContinue).Id -join ','").Output()
	if err != nil {
		t.Fatal(err)
	}
	var ids []session.ProcID
	var pid uint32
	for _, c := range string(out) {
		switch {
		case c >= '0' && c <= '9':
			pid = pid*10 + uint32(c-'0')
		default:
			if pid != 0 {
				if id, ok := session.IdentifyProcess(pid); ok {
					ids = append(ids, id)
				}
			}
			pid = 0
		}
	}
	return ids
}
