//go:build windows

package session

import (
	"crypto/sha256"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// docs/M2 验收 3: a large paste must reach the program complete and in order.
// The browser sends it as a burst of input frames; nothing may be dropped on
// the way into the pseudo console.
func TestLargePasteArrivesIntact(t *testing.T) {
	const size = 2 << 20
	s := direct(t)
	waitText(t, s, `READY`, 15*time.Second)
	kept := filepath.Join(t.TempDir(), "received.bin")
	if err := s.Write([]byte(fmt.Sprintf("sink %d %s\r", size, kept))); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond) // let it switch to raw input

	data := make([]byte, size)
	rng := rand.New(rand.NewSource(1))
	for i := range data {
		data[i] = byte('a' + rng.Intn(26)) // printable: the console must not interpret any of it
	}
	start := time.Now()
	for off := 0; off < size; off += 64 << 10 { // the frame size a browser uses (docs/M1 第 10 节)
		if err := s.Write(data[off:min(off+64<<10, size)]); err != nil {
			t.Fatalf("input refused at offset %d of a %d byte paste: %v", off, size, err)
		}
	}
	want := fmt.Sprintf("SINK %d %x <nil>", size, sha256.Sum256(data))
	got := waitText(t, s, `SINK \d+ [0-9a-f]+ \S+`, 120*time.Second)
	if got != want {
		recv, _ := os.ReadFile(kept)
		at := 0
		for at < len(recv) && at < len(data) && recv[at] == data[at] {
			at++
		}
		lo, hi := max(0, at-16), min(len(data), at+32)
		t.Fatalf("paste damaged at byte %d (frame %d, offset in frame %d):\n sent %q\n recv %q",
			at, at/(64<<10), at%(64<<10), data[lo:hi], recv[lo:min(len(recv), hi)])
	}
	t.Logf("2 MB of input through the pseudo console in %v", time.Since(start).Round(time.Millisecond))
}
