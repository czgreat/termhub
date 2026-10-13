//go:build windows

package host

import (
	"fmt"
	"golang.org/x/sys/windows"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unsafe"

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
	before := testcliPIDs(t, uint32(hostProc.Process.Pid))
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
			if now, ok := session.IdentifyProcess(id.PID); ok && now.Created == id.Created && testProcessRunning(t, id.PID) {
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

// A terminated Windows process may retain its identity while another handle is
// open. Identity is not liveness; wait on the process object, without killing it.
func testProcessRunning(t *testing.T, pid uint32) bool {
	t.Helper()
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	result, err := windows.WaitForSingleObject(h, 0)
	if err != nil {
		t.Fatal(err)
	}
	return result == uint32(windows.WAIT_TIMEOUT)
}
func testcliPIDs(t *testing.T, hostPID uint32) []session.ProcID {
	t.Helper()
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(snapshot)
	rows := map[uint32]windows.ProcessEntry32{}
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	for err = windows.Process32First(snapshot, &pe); err == nil; err = windows.Process32Next(snapshot, &pe) {
		rows[pe.ProcessID] = pe
	}
	if err != windows.ERROR_NO_MORE_FILES {
		t.Fatal(err)
	}
	var ids []session.ProcID
	for pid, row := range rows {
		if !strings.EqualFold(windows.UTF16ToString(row.ExeFile[:]), "testcli.exe") {
			continue
		}
		parent := row.ParentProcessID
		seen := map[uint32]bool{}
		for parent != 0 && parent != hostPID && !seen[parent] {
			seen[parent] = true
			parent = rows[parent].ParentProcessID
		}
		if parent == hostPID {
			if id, ok := session.IdentifyProcess(pid); ok {
				ids = append(ids, id)
			}
		}
	}
	return ids
}
