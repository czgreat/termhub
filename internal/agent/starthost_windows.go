//go:build windows

package agent

import (
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// StartHostProcess launches "<this exe> host" detached from the agent: no
// console, no window, its own process group, and outside the agent's Job when
// the Job allows it, so that the agent's end is never the host's (docs/M3 第 3 节).
func StartHostProcess(pipe string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"host"}
	if pipe != "" {
		args = append(args, "-pipe", pipe)
	}
	for _, flags := range []uint32{
		windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_BREAKAWAY_FROM_JOB,
		windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP, // the Job forbids breaking away
	} {
		cmd := exec.Command(self, args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags, HideWindow: true}
		if err = cmd.Start(); err == nil {
			cmd.Process.Release()
			return nil
		}
	}
	return err
}
