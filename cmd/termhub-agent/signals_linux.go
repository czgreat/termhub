package main

import (
	"os"
	"syscall"
)

// agentStopSignals end the run loop: SIGTERM is systemd stopping the unit.
var agentStopSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

// hostStopSignals make the host end its sessions properly before it exits:
// the uninstall script, or the system shutting down (复核 PR #40 第 4 条).
var hostStopSignals = []os.Signal{syscall.SIGTERM}
