//go:build linux

package e2e

import (
	"os"
	"path/filepath"
	"testing"
)

const exeSuffix = ""

// testPipe is a socket path short enough for sun_path (108 bytes).
func testPipe(t *testing.T) string {
	dir, err := os.MkdirTemp("", "the2e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "h.sock")
}
