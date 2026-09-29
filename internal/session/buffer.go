package session

import "sync"

// DefaultBufferBytes is the per-session output buffer size (总体设计, 原 Q8).
const DefaultBufferBytes = 4 << 20

// safeInterval is the minimum distance between recorded safe points.
const safeInterval = 64 << 10

// safePoint is an offset at which the stream may be cut, together with the
// terminal modes in force at that offset.
type safePoint struct {
	off   uint64
	modes Modes
}

// Buffer is a session's output history: a ring of the most recent bytes,
// addressed by 64-bit stream offsets (docs/M1 第 4 节), plus the mode tracker
// and the safe points that make a replay start cleanly (docs/M2 第 6、7 节).
//
// Write is called only from the session's read loop and never waits on
// anything but this mutex, which no slow operation is ever performed under.
type Buffer struct {
	mu      sync.Mutex
	ring    []byte
	base    uint64 // offset of the oldest byte kept
	end     uint64 // offset one past the newest byte
	tracker Tracker
	safes   []safePoint
}

// NewBuffer creates a buffer keeping the last size bytes.
func NewBuffer(size int) *Buffer {
	if size <= 0 {
		size = DefaultBufferBytes
	}
	return &Buffer{ring: make([]byte, size), safes: []safePoint{{}}}
}

// OnTitle registers a callback invoked (under the buffer lock, so it must not
// block) when the program changes the terminal title.
func (b *Buffer) OnTitle(f func(string)) { b.tracker.OnTitle(f) }

// Range returns the offsets currently held: [base, end).
func (b *Buffer) Range() (base, end uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.base, b.end
}

// Modes returns the current terminal modes.
func (b *Buffer) Modes() Modes {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tracker.Modes
}

// Write appends program output and returns the offset it starts at.
func (b *Buffer) Write(p []byte) (start uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	start = b.end
	size := uint64(len(b.ring))
	for _, c := range p {
		b.ring[b.end%size] = c
		b.end++
		b.tracker.Step(c)
		// With no safe point left, take the first one available; otherwise
		// space them out.
		if b.tracker.Safe() && (len(b.safes) == 0 || b.end-b.safes[len(b.safes)-1].off >= safeInterval) {
			b.safes = append(b.safes, safePoint{b.end, b.tracker.Modes})
		}
	}
	if b.end-b.base > size {
		b.base = b.end - size
	}
	// A point below base cannot be cut at any more. safes may become empty:
	// that is the pathological case of one endless sequence filling the ring,
	// and Replay then degrades to "modes only".
	i := 0
	for i < len(b.safes) && b.safes[i].off < b.base {
		i++
	}
	if i > 0 {
		b.safes = append(b.safes[:0], b.safes[i:]...)
	}
	return start
}

// Replay is what a newly attached viewer needs first (docs/M1 4.2).
type Replay struct {
	Resume    bool   // true: continue from From, terminal state is kept
	From      uint64 // offset of Data[0]
	End       uint64 // offset one past Data; live output continues here
	Prelude   []byte // mode-setting sequences, only when !Resume
	Data      []byte
	AltScreen bool // the session is on the alternate screen now: redraw after replay
}

// Replay builds the catch-up data for a viewer that has received everything
// below *have, or nothing when have is nil.
func (b *Buffer) Replay(have *uint64) Replay {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := Replay{End: b.end, AltScreen: b.tracker.Modes.AltScreen != 0}
	if have != nil && *have >= b.base && *have <= b.end {
		r.Resume, r.From = true, *have
		r.Data = b.copyRange(*have, b.end)
		return r
	}
	if len(b.safes) == 0 { // nothing in the ring is cuttable
		r.From = b.end
		r.Prelude = b.tracker.Modes.Prelude()
		return r
	}
	sp := b.safes[0]
	r.From = sp.off
	r.Prelude = sp.modes.Prelude()
	r.Data = b.copyRange(sp.off, b.end)
	return r
}

// Slice copies [from, end) if it is still held.
func (b *Buffer) Slice(from uint64) (data []byte, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if from < b.base || from > b.end {
		return nil, false
	}
	return b.copyRange(from, b.end), true
}

func (b *Buffer) copyRange(from, to uint64) []byte {
	if to <= from {
		return nil
	}
	size := uint64(len(b.ring))
	out := make([]byte, to-from)
	start := from % size
	n := copy(out, b.ring[start:min(size, start+(to-from))])
	copy(out[n:], b.ring)
	return out
}
