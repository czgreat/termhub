//go:build linux

package agent

import (
	"os"
	"os/exec"
	"syscall"
)

// StartHostProcess launches "<this executable> host" detached from the agent:
// a session of its own, no terminal, standard streams on /dev/null, so that
// the agent's end is never the host's (docs/M3 第 3 节). Under systemd the
// agent's unit uses KillMode=process, so stopping or restarting the agent
// leaves the host and its sessions alone.
func StartHostProcess(pipe string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"host"}
	if pipe != "" {
		args = append(args, "-pipe", pipe)
	}
	cmd := exec.Command(self, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Dir = "/"
	if err := cmd.Start(); err != nil { // nil Stdin/Stdout/Stderr: /dev/null
		return err
	}
	go cmd.Wait() // reap it if it ever ends while the agent still runs
	return nil
}
