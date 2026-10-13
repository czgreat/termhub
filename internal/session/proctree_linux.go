//go:build linux

package session

import (
	"bytes"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// bootMark tells one boot from another: the hash of the kernel's boot_id in
// bits 40-62 of a creation time, above the start time in clock ticks since
// boot (2^40 ticks is over 300 years). Not /proc/stat's btime plus the start
// time: btime moves when the clock is stepped (NTP after boot), and a host
// restarted after that would no longer recognise the processes it recorded
// (复核 PR #40 第 6 条).
var bootMark = sync.OnceValue(func() int64 {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return 0
	}
	h := fnv.New64a()
	h.Write(bytes.TrimSpace(b))
	return int64(h.Sum64()&(1<<23-1)) << 40
})

// procStat is what /proc/<pid>/stat says about a process.
type procStat struct {
	pid, ppid, sid int
	state          byte
	name           string
	created        int64 // bootMark | start time in ticks since boot: with pid, the identity of a process
}

func readStat(pid int) (procStat, bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return procStat{}, false
	}
	return parseStat(pid, b)
}

// parseStat reads "pid (comm) state ppid pgrp session ... starttime". comm may
// contain spaces and parentheses, so it ends at the last ')'.
func parseStat(pid int, b []byte) (procStat, bool) {
	open, end := bytes.IndexByte(b, '('), bytes.LastIndexByte(b, ')')
	if open < 0 || end < open || end+2 > len(b) {
		return procStat{}, false
	}
	f := strings.Fields(string(b[end+2:])) // f[0] is field 3 (state)
	if len(f) < 20 {
		return procStat{}, false
	}
	ppid, _ := strconv.Atoi(f[1])
	sid, _ := strconv.Atoi(f[3])
	start, err := strconv.ParseInt(f[19], 10, 64) // field 22
	if err != nil {
		return procStat{}, false
	}
	return procStat{pid: pid, ppid: ppid, sid: sid, state: f[0][0], name: string(b[open+1 : end]), created: bootMark() | start}, true
}

func (p procStat) running() bool { return p.state != 'Z' && p.state != 'X' }

// A process table snapshot is shared by all sessions of the host and refreshed
// at most once per second.
var (
	snapMu   sync.Mutex
	snapTime time.Time
	snapAll  []procStat
)

func processTable() []procStat {
	snapMu.Lock()
	defer snapMu.Unlock()
	if time.Since(snapTime) < time.Second && snapAll != nil {
		return snapAll
	}
	des, err := os.ReadDir("/proc")
	if err != nil {
		return snapAll
	}
	all := make([]procStat, 0, len(des))
	for _, de := range des {
		pid, err := strconv.Atoi(de.Name())
		if err != nil || pid <= 0 {
			continue
		}
		if st, ok := readStat(pid); ok {
			all = append(all, st)
		}
	}
	snapAll, snapTime = all, time.Now()
	return all
}

// tracked is a descendant: its pid and creation time, which together tell it
// apart from a later process that reuses the pid (docs/M2 第 9 节).
type tracked struct {
	name    string
	created int64
}

// procTree accumulates every process ever seen in a session: descendants of
// the root, and members of the root's terminal session. Accumulating matters:
// an orphan is re-parented away from the tree, and a single look at parent
// links would no longer find it.
type procTree struct {
	mu    sync.Mutex
	root  int
	procs map[uint32]*tracked
}

const maxTracked = 2048

func newProcTree(rootPID int) *procTree {
	t := &procTree{root: rootPID, procs: map[uint32]*tracked{}}
	if st, ok := readStat(rootPID); ok {
		t.procs[uint32(rootPID)] = &tracked{name: "root", created: st.created}
	}
	return t
}

// scan adds newly appeared descendants and session members.
func (t *procTree) scan() { t.scanTable(processTable()) }

func (t *procTree) scanTable(all []procStat) {
	kids := map[int][]procStat{}
	byPID := make(map[int]procStat, len(all))
	for _, p := range all {
		kids[p.ppid] = append(kids[p.ppid], p)
		byPID[p.pid] = p
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	// Forget what has ended. Nothing holds a Linux process the way a Windows
	// handle does, so an ended one's pid can be recycled; a stranger under
	// that pid must not bring its children in, nor be killed at the end
	// (复核 PR #40 第 1 条). The root stays: newer than the snapshot, perhaps,
	// and its record is what the session-id check below compares against.
	for pid, p := range t.procs {
		if st, ok := byPID[int(pid)]; int(pid) != t.root && (!ok || st.created != p.created || !st.running()) {
			delete(t.procs, pid)
		}
	}
	// Same terminal session: started under it, whatever its parent now. A
	// session id is the leader's pid; once the leader is gone that pid may
	// lead someone else's session, which then is not ours.
	ours := true
	if r := t.procs[uint32(t.root)]; r == nil {
		ours = false
	} else {
		st, ok := byPID[t.root]
		if !ok { // started after the snapshot was taken
			st, ok = readStat(t.root)
		}
		if ok && st.created != r.created {
			ours = false
		}
	}
	for _, p := range all {
		if ours && p.sid == t.root && len(t.procs) < maxTracked && p.running() {
			if _, known := t.procs[uint32(p.pid)]; !known {
				t.procs[uint32(p.pid)] = &tracked{name: p.name, created: p.created}
			}
		}
	}
	queue := make([]int, 0, len(t.procs))
	for pid := range t.procs {
		queue = append(queue, int(pid))
	}
	for len(queue) > 0 && len(t.procs) < maxTracked {
		parent := queue[0]
		queue = queue[1:]
		// Children only of the process we recorded, not of a later one that
		// was given the same pid (the root may have ended since).
		if st, ok := byPID[parent]; !ok || st.created != t.procs[uint32(parent)].created {
			continue
		}
		for _, c := range kids[parent] {
			if _, known := t.procs[uint32(c.pid)]; known || !c.running() {
				continue
			}
			// A child cannot be older than its parent. If it is, the parent's
			// pid was recycled: not ours.
			if c.created < t.procs[uint32(parent)].created {
				continue
			}
			t.procs[uint32(c.pid)] = &tracked{name: c.name, created: c.created}
			queue = append(queue, c.pid)
		}
	}
}

// isAlive: the pid still denotes the process created at that time, and it
// has not ended (a zombie has).
func isAlive(pid uint32, created int64) bool {
	st, ok := readStat(int(pid))
	return ok && st.created == created && st.running()
}

func (t *procTree) survivors() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []string
	for pid, p := range t.procs {
		if isAlive(pid, p.created) {
			out = append(out, fmt.Sprintf("%s(%d)", p.name, pid))
		}
	}
	return out
}

func (t *procTree) anyAlive() bool { return len(t.survivors()) > 0 }

func (t *procTree) killAll() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for pid, p := range t.procs {
		killIfSame(pid, p.created)
	}
}

func (t *procTree) close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	clear(t.procs)
}

// killIfSame sends SIGKILL through a pidfd opened before the identity check:
// once open, the pidfd names that process and no other, so a pid recycled in
// between can never be hit.
func killIfSame(pid uint32, created int64) bool {
	if created == 0 {
		return false
	}
	fd, err := unix.PidfdOpen(int(pid), 0)
	if errors.Is(err, unix.ENOSYS) { // kernels before 5.3: best effort
		if isAlive(pid, created) {
			return unix.Kill(int(pid), unix.SIGKILL) == nil
		}
		return false
	}
	if err != nil {
		return false
	}
	defer unix.Close(fd)
	if !isAlive(pid, created) {
		return false
	}
	return unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0) == nil
}

// KillIfSame terminates the process only if pid still denotes the process
// created at the recorded time. It cleans up what a crashed host left behind
// (docs/M3 第 7 节).
func KillIfSame(p ProcID) bool { return killIfSame(p.PID, p.Created) }

// IdentifyProcess returns the pid together with its creation time.
func IdentifyProcess(pid uint32) (ProcID, bool) {
	st, ok := readStat(int(pid))
	if !ok {
		return ProcID{}, false
	}
	return ProcID{PID: pid, Created: st.created}, true
}

// Descendants lists the processes tracked for this session that still run.
func (s *Session) Descendants() []ProcID {
	s.tree.mu.Lock()
	defer s.tree.mu.Unlock()
	var out []ProcID
	for pid, p := range s.tree.procs {
		if isAlive(pid, p.created) {
			out = append(out, ProcID{PID: pid, Created: p.created, Name: p.name})
		}
	}
	return out
}
