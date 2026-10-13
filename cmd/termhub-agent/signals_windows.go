package main

import "os"

// agentStopSignals: Ctrl+C only, as before Linux nodes. Go reports a console
// close, logoff or shutdown as SIGTERM; left unhandled, the agent ends with a
// failure code and the scheduled task restarts it (复核 PR #40 第 5 条).
var agentStopSignals = []os.Signal{os.Interrupt}

// hostStopSignals: none; the host has no console.
var hostStopSignals []os.Signal
