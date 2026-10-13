//go:build windows

package session

import "golang.org/x/sys/windows"

// launch starts the root process in a new pseudo console. Every failure
// leaves nothing behind.
func launch(spec Spec, cwd string, env []string) (*conPTY, *spawned, string, error) {
	cl, err := buildCommand(spec, env)
	if err != nil {
		return nil, nil, "", err
	}
	pty, err := newConPTY(spec.Cols, spec.Rows)
	if err != nil {
		return nil, nil, "", errf(CodeSpawnFailed, "pseudo console: "+err.Error())
	}
	sp, err := pty.spawn(cl, cwd, env)
	if err != nil {
		pty.releaseChildEnds()
		pty.ClosePTY()
		pty.closePipes()
		return nil, nil, "", errf(CodeSpawnFailed, cl.exe+": "+err.Error())
	}
	return pty, sp, cl.exe, nil
}

func (sp *spawned) newTree() *procTree { return newProcTree(sp.pid, sp.process) }

// killGroup is cleanup tier 2: everything in the session's Job.
func (sp *spawned) killGroup() {
	windows.TerminateJobObject(sp.job, 1)
	windows.CloseHandle(sp.job)
}

func (sp *spawned) release() { windows.CloseHandle(sp.process) }

func (s *Session) waitProcess() {
	windows.WaitForSingleObject(s.sp.process, windows.INFINITE)
	var code uint32
	windows.GetExitCodeProcess(s.sp.process, &code)
	s.exitCode.Store(int64(code))
	close(s.procDone)
}
