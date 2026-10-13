//go:build linux

package host

import (
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A socket left behind by a host that crashed is replaced, while a live host
// is never displaced (TestSecondHostIsRefused).
func TestStaleSocketIsReplaced(t *testing.T) {
	pipe := testPipe(t, "stale")
	ln, err := net.Listen("unix", pipe)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close() // the file stays, nobody serves it: a crashed host
	if _, err := os.Stat(pipe); err != nil {
		t.Fatal("test setup: no stale socket")
	}
	h, err := New(Config{PipeName: pipe, StateDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("stale socket not replaced: %v", err)
	}
	defer h.Shutdown()
	c, err := Dial(pipe, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}

func TestSocketIsPrivate(t *testing.T) {
	pipe := filepath.Join(testPipe(t, "mode"), "..", "sub", "h.sock")
	os.MkdirAll(filepath.Dir(pipe), 0o755) // too open: tightened by the host
	h, err := New(Config{PipeName: pipe, StateDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Shutdown()
	for p, want := range map[string]os.FileMode{filepath.Dir(pipe): 0o700, pipe: 0o600} {
		st, err := os.Stat(p)
		if err != nil || st.Mode().Perm() != want {
			t.Errorf("%s: mode %v, want %o (%v)", p, st.Mode().Perm(), want, err)
		}
	}
}
