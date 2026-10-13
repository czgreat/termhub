//go:build linux

package session

import (
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type spawned struct {
	proc    *os.Process
	pid     uint32
	created int64 // the root's identity with pid, read right after the start
	// escapeRisk has no Linux counterpart: whatever leaves the terminal
	// session is still found as a descendant (tier 3).
	escapeRisk bool
}

// launch starts the root process on a new pty, as the leader of a new
// terminal session with the pty as its controlling terminal. Every failure
// leaves nothing behind.
func launch(spec Spec, cwd string, env []string) (*conPTY, *spawned, string, error) {
	argv, err := buildArgv(spec, env)
	if err != nil {
		return nil, nil, "", err
	}
	pty, err := newConPTY(spec.Cols, spec.Rows)
	if err != nil {
		return nil, nil, "", errf(CodeSpawnFailed, "pseudo terminal: "+err.Error())
	}
	proc, err := os.StartProcess(argv[0], argv, &os.ProcAttr{Dir: cwd, Env: env,
		Files: []*os.File{pty.slave, pty.slave, pty.slave},
		Sys:   &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}}) // Ctty: fd 0 in the child
	if err != nil {
		pty.closePipes()
		return nil, nil, "", errf(CodeSpawnFailed, argv[0]+": "+err.Error())
	}
	pty.releaseChildEnds()
	pty.pid = proc.Pid
	if st, ok := readStat(proc.Pid); ok { // not reaped yet: only waitProcess reaps it
		pty.created = st.created
	}
	return pty, &spawned{proc: proc, pid: uint32(proc.Pid), created: pty.created}, argv[0], nil
}

func (sp *spawned) newTree() *procTree { return newProcTree(int(sp.pid)) }

// killGroup is cleanup tier 2: SIGKILL to every process still in the
// terminal session. The root leads it, so its pid is the session id. Members
// are found through the snapshot; the root's own group goes by kill(-pid).
func (sp *spawned) killGroup() {
	// Once the root is reaped its pid may be given to another process, which
	// may lead a session of its own: then that pid is not ours to signal
	// (复核 PR #40 第 3 条). While the pid is free, a group under it can only
	// be ours: Linux does not hand out a pid still used as a group id.
	root, ok := readStat(int(sp.pid))
	if ok && (root.sid != int(sp.pid) || sp.created != 0 && root.created != sp.created) {
		return
	}
	unix.Kill(-int(sp.pid), unix.SIGKILL)
	snapMu.Lock()
	snapTime = time.Time{} // a fresh look, not one from before tier 1
	snapMu.Unlock()
	for _, p := range processTable() {
		if p.sid == int(sp.pid) && p.running() {
			killIfSame(uint32(p.pid), p.created)
		}
	}
}

func (sp *spawned) release() { sp.proc.Release() }

func (s *Session) waitProcess() {
	st, err := s.sp.proc.Wait()
	code := -1
	if err == nil {
		code = st.ExitCode()
		if ws, ok := st.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			code = 128 + int(ws.Signal()) // as a shell reports it
		}
	}
	s.exitCode.Store(int64(code))
	close(s.procDone)
}

// setUTF8CodePage: a pty has no code page; the encoding is LANG's, which
// userEnvironment sets to UTF-8 when the user's own leaves it open.
func setUTF8CodePage(pid uint32) bool { return true }
