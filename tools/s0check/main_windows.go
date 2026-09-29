//go:build windows

// Command s0check verifies, with the project's real session layer, that the
// environment it is started in can run terminal sessions in each candidate
// shell. It exists to be run from a scheduled task that runs "whether the user
// is logged on or not" (session 0), which is how the agent runs in production
// (docs/M5 第 4 节). Results go to the file named by the first argument.
//
// It deliberately starts no official CLI: tests never touch a machine's real
// Claude Code or Codex (docs/M12 第 4 节).
package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"golang.org/x/sys/windows"

	"termhub/internal/session"
)

var stripANSI = regexp.MustCompile(`\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b\[[0-9;?<>=!]*[ -/]*[@-~]|\x1b[()][0-9A-B]|\x1b[=>]`)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: s0check <result-file> <cwd>")
		os.Exit(2)
	}
	out, err := os.Create(os.Args[1])
	if err != nil {
		os.Exit(1)
	}
	defer out.Close()
	cwd := os.Args[2]

	var sess uint32
	windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &sess)
	fmt.Fprintf(out, "context: user=%s windows-session=%d\n", os.Getenv("USERNAME"), sess)

	shells := []struct{ name, path string }{
		{"pwsh 7 (MSI)", `C:\Program Files\PowerShell\7\pwsh.exe`},
		{"powershell 5.1", `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`},
		{"cmd", `C:\Windows\System32\cmd.exe`},
	}
	for _, sh := range shells {
		status, detail := "PASS", ""
		if _, err := os.Stat(sh.path); err != nil {
			status, detail = "FAIL", "not installed"
		} else if info, err := check(sh.path, cwd); err != nil {
			status, detail = "FAIL", err.Error()
		} else {
			detail = fmt.Sprintf("codepage_unset=%v job_escape_risk=%v", info.CodePageUnset, info.JobEscapeRisk)
		}
		fmt.Fprintf(out, "[%s] %-16s %s\n", status, sh.name, detail)
		out.Sync()
	}
	fmt.Fprintln(out, "DONE")
}

// check opens a plain shell, has it echo a marker containing non-ASCII text,
// then ends the session and insists that nothing is left behind.
func check(shell, cwd string) (session.Info, error) {
	s, err := session.Start(session.Spec{Mode: session.ModeShell, ShellPath: shell, Cwd: cwd})
	if err != nil {
		return session.Info{}, err
	}
	time.Sleep(3 * time.Second) // let the prompt come up; typing earlier would be lost in some shells
	s.Write([]byte("echo TERMHUB_中文_OK\r"))
	found := false
	for end := time.Now().Add(30 * time.Second); time.Now().Before(end) && !found; time.Sleep(200 * time.Millisecond) {
		text := stripANSI.ReplaceAllString(string(s.Replay(nil).Data), "")
		// The typed command is echoed too; the marker must appear twice.
		found = strings.Count(text, "TERMHUB_中文_OK") >= 2
	}
	info := s.Info()
	s.Close(session.Force, session.ReasonUser)
	select {
	case exit := <-s.Done():
		if len(exit.Leftover) > 0 {
			return info, fmt.Errorf("processes left behind: %v", exit.Leftover)
		}
	case <-time.After(20 * time.Second):
		return info, fmt.Errorf("cleanup did not finish")
	}
	if !found {
		return info, fmt.Errorf("shell started but did not echo the marker")
	}
	return info, nil
}
