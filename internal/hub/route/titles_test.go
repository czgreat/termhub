package route

import (
	"sync/atomic"
	"testing"
	"time"
)

// A burst of title changes is told once, and a change after that is told
// again (状态点复核 3).
func TestCoalescerBurstFiresOnceThenAgain(t *testing.T) {
	var n atomic.Int32
	c := coalescer{every: 40 * time.Millisecond, fire: func() { n.Add(1) }}
	for i := 0; i < 20; i++ {
		c.poke()
	}
	time.Sleep(150 * time.Millisecond)
	if got := n.Load(); got != 1 {
		t.Fatalf("burst told %d times, want 1", got)
	}
	c.poke()
	time.Sleep(150 * time.Millisecond)
	if got := n.Load(); got != 2 {
		t.Fatalf("later change: told %d times in all, want 2", got)
	}
}
