//go:build linux

package host

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const exeSuffix = ""

// testPipe is a socket path short enough for sun_path (108 bytes).
func testPipe(t *testing.T, tag string) string {
	dir, err := os.MkdirTemp("", "th")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "h.sock")
}

// runningTestcli lists running processes of this test's testcli, "" when none.
func runningTestcli() string {
	out, _ := exec.Command("pgrep", "-af", testcli).Output()
	return strings.TrimSpace(string(out))
}
