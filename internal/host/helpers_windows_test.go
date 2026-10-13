//go:build windows

package host

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"testing"
)

const exeSuffix = ".exe"

func testPipe(t *testing.T, tag string) string { return `\\.\pipe\termhub-test-` + tag }

// runningTestcli lists running testcli processes, "" when there are none.
func runningTestcli() string {
	if out, _ := exec.Command("tasklist", "/FI", "IMAGENAME eq testcli.exe", "/FO", "CSV", "/NH").Output(); bytes.Contains(out, []byte(filepath.Base(testcli))) {
		return string(out)
	}
	return ""
}
