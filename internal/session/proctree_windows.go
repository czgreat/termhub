//go:build windows

package session

import (
	"fmt"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// A process snapshot is shared by all sessions of the host and refreshed at
// most once per second.
var (
	snapMu   sync.Mutex
	snapTime time.Time
	snapKids map[uint32][]procEntry // parent pid -> children
)

type procEntry struct {
	pid  uint32
	name string
}

func processChildren() map[uint32][]procEntry {
	snapMu.Lock()
	defer snapMu.Unlock()
	if time.Since(snapTime) < time.Second && snapKids != nil {
		return snapKids
	}
	h, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return snapKids
	}
	defer windows.CloseHandle(h)
	kids := map[uint32][]procEntry{}
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	for err = windows.Process32First(h, &pe); err == nil; err = windows.Process32Next(h, &pe) {
		kids[pe.ParentProcessID] = append(kids[pe.ParentProcessID],
			procEntry{pe.ProcessID, windows.UTF16ToString(pe.ExeFile[:])})
	}
	snapKids, snapTime = kids, time.Now()
	return kids
}

// tracked is a descendant we hold an open handle to. While a handle is open
// the system cannot hand the pid to another process, so terminating through
// the handle can never hit an unrelated process (docs/M2 第 9 节: PID reuse).
type tracked struct {
	handle  windows.Handle
	name    string
	created int64 // creation time, 100ns ticks; identifies the process with pid
}

// procTree accumulates every descendant ever seen under a session's root.
// Accumulating matters: once a parent dies its children are orphans, and a
// single look at parent links would no longer find them.
type procTree struct {
	mu    sync.Mutex
	procs map[uint32]*tracked
}

const maxTracked = 2048

func newProcTree(rootPID uint32, root windows.Handle) *procTree {
	t := &procTree{procs: map[uint32]*tracked{}}
	var dup windows.Handle
	cur := windows.CurrentProcess()
	if windows.DuplicateHandle(cur, root, cur, &dup, 0, false, windows.DUPLICATE_SAME_ACCESS) == nil {
		t.procs[rootPID] = &tracked{handle: dup, name: "root", created: creationTime(dup)}
	}
	return t
}

func creationTime(h windows.Handle) int64 {
	var c, e, k, u windows.Filetime
	if windows.GetProcessTimes(h, &c, &e, &k, &u) != nil {
		return 0
	}
	return c.Nanoseconds() / 100
}

// scan adds newly appeared descendants.
func (t *procTree) scan() {
	kids := processChildren()
	t.mu.Lock()
	defer t.mu.Unlock()
	queue := make([]uint32, 0, len(t.procs))
	for pid := range t.procs {
		queue = append(queue, pid)
	}
	for len(queue) > 0 && len(t.procs) < maxTracked {
		parent := queue[0]
		queue = queue[1:]
		for _, c := range kids[parent] {
			if _, known := t.procs[c.pid]; known || c.pid == 0 {
				continue
			}
			h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, c.pid)
			if err != nil {
				continue
			}
			created := creationTime(h)
			// A child cannot be older than its parent. If it is, the parent's
			// pid was recycled before we held a handle to it: not ours.
			if p := t.procs[parent]; p != nil && created != 0 && p.created != 0 && created < p.created {
				windows.CloseHandle(h)
				continue
			}
			t.procs[c.pid] = &tracked{handle: h, name: c.name, created: created}
			queue = append(queue, c.pid)
		}
	}
}

func isAlive(h windows.Handle) bool {
	ev, _ := windows.WaitForSingleObject(h, 0)
	return ev == uint32(windows.WAIT_TIMEOUT)
}

// survivors lists tracked processes still running.
func (t *procTree) survivors() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []string
	for pid, p := range t.procs {
		if isAlive(p.handle) {
			out = append(out, fmt.Sprintf("%s(%d)", p.name, pid))
		}
	}
	return out
}

// anyAlive reports whether any tracked process still runs.
func (t *procTree) anyAlive() bool { return len(t.survivors()) > 0 }

// killAll terminates every tracked process that still runs.
func (t *procTree) killAll() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, p := range t.procs {
		if isAlive(p.handle) {
			windows.TerminateProcess(p.handle, 1)
		}
	}
}

func (t *procTree) close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for pid, p := range t.procs {
		windows.CloseHandle(p.handle)
		delete(t.procs, pid)
	}
}

// ProcID identifies a process beyond its pid: a recycled pid has another
// creation time.
type ProcID struct {
	PID     uint32 `json:"pid"`
	Created int64  `json:"created"`
	Name    string `json:"name,omitempty"`
}

// KillIfSame terminates the process only if pid still denotes the process
// that was created at the recorded time. It is used to clean up what a
// crashed host left behind (docs/M3 第 7 节).
func KillIfSame(p ProcID) bool {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, p.PID)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	if p.Created == 0 || creationTime(h) != p.Created || !isAlive(h) {
		return false
	}
	return windows.TerminateProcess(h, 1) == nil
}

// Descendants lists the processes tracked for this session that still run.
func (s *Session) Descendants() []ProcID {
	s.tree.mu.Lock()
	defer s.tree.mu.Unlock()
	var out []ProcID
	for pid, p := range s.tree.procs {
		if isAlive(p.handle) {
			out = append(out, ProcID{PID: pid, Created: p.created, Name: p.name})
		}
	}
	return out
}
