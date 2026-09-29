package proto

import "sync"

// Tracker is the receiving side of a session's output stream (docs/M1 4.1).
// It knows the end offset received so far and classifies each incoming frame.
type Tracker struct {
	have uint64
}

// NewTracker starts a tracker that has already received everything below have.
func NewTracker(have uint64) *Tracker { return &Tracker{have: have} }

// Have returns the end offset received so far.
func (t *Tracker) Have() uint64 { return t.have }

// Reset moves the tracker, used after a full replay tells us where it starts.
func (t *Tracker) Reset(have uint64) { t.have = have }

// Accept classifies a frame starting at start. It returns the part of data not
// seen before (nil if all of it is a repeat). gap is true when bytes between
// Have and start are missing; the frame is then ignored and the caller must
// re-attach with Have (docs/M1 4.3).
func (t *Tracker) Accept(start uint64, data []byte) (fresh []byte, gap bool) {
	end := start + uint64(len(data))
	switch {
	case end < start: // overflow, treat as corrupt
		return nil, true
	case start > t.have:
		return nil, true
	case end <= t.have:
		return nil, false
	}
	fresh = data[t.have-start:]
	t.have = end
	return fresh, false
}

// Item is one unit handed to a link writer by a Mux.
type Item struct {
	SID    SID
	Gap    bool   // a gap notice: queued data was dropped, stream continues at Offset
	Offset uint64 // start offset of Data, or the continuation offset of a gap
	Data   []byte
}

type outQueue struct {
	chunks []Item
	bytes  int
	gapped bool
	next   uint64 // continuation offset to report when gapped and empty
}

// Mux holds one bounded output queue per session and serves them round-robin,
// so a flooding session cannot starve the others and a slow link never blocks
// the producer: on overflow the session's queue is dropped and a gap notice is
// queued instead (docs/M1 第 6 节).
type Mux struct {
	mu     sync.Mutex
	limit  int
	queues map[SID]*outQueue
	order  []SID
	idx    int
	wake   chan struct{}
}

// NewMux creates a Mux whose per-session queues hold at most limit bytes.
func NewMux(limit int) *Mux {
	return &Mux{limit: limit, queues: map[SID]*outQueue{}, wake: make(chan struct{}, 1)}
}

// Wake is signalled after a Push. Writers select on it, then drain with Next.
func (m *Mux) Wake() <-chan struct{} { return m.wake }

// Push queues output for sid. data is copied and split into frames of at most
// MaxOutputData. Push never blocks.
func (m *Mux) Push(sid SID, offset uint64, data []byte) {
	if len(data) == 0 {
		return
	}
	m.mu.Lock()
	q := m.queues[sid]
	if q == nil {
		q = &outQueue{}
		m.queues[sid] = q
		m.order = append(m.order, sid)
	}
	if q.bytes+len(data) > m.limit {
		q.chunks, q.bytes, q.gapped = nil, 0, true
		q.next = offset
		if len(data) > m.limit { // cannot hold even this one: skip it too
			q.next = offset + uint64(len(data))
			data = nil
		}
	}
	for len(data) > 0 {
		n := min(len(data), MaxOutputData)
		q.chunks = append(q.chunks, Item{SID: sid, Offset: offset, Data: append([]byte(nil), data[:n]...)})
		q.bytes += n
		offset += uint64(n)
		data = data[n:]
	}
	m.mu.Unlock()
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Next returns the next item in round-robin order, or ok=false when all queues
// are empty. A gap notice for a session is always returned before its data.
func (m *Mux) Next() (it Item, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for range m.order {
		if m.idx >= len(m.order) {
			m.idx = 0
		}
		sid := m.order[m.idx]
		q := m.queues[sid]
		m.idx++
		if q.gapped {
			q.gapped = false
			next := q.next
			if len(q.chunks) > 0 {
				next = q.chunks[0].Offset
			}
			return Item{SID: sid, Gap: true, Offset: next}, true
		}
		if len(q.chunks) > 0 {
			it = q.chunks[0]
			q.chunks[0] = Item{}
			q.chunks = q.chunks[1:]
			q.bytes -= len(it.Data)
			return it, true
		}
	}
	return Item{}, false
}

// Drop discards what is queued for sid without a gap notice, used when
// forwarding for that session is switched off.
func (m *Mux) Drop(sid SID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.queues[sid]; !ok {
		return
	}
	delete(m.queues, sid)
	for i, s := range m.order {
		if s == sid {
			m.order = append(m.order[:i], m.order[i+1:]...)
			if m.idx > i {
				m.idx--
			}
			break
		}
	}
}

// Pending reports queued bytes for sid.
func (m *Mux) Pending(sid SID) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if q := m.queues[sid]; q != nil {
		return q.bytes
	}
	return 0
}

// Gap records that output for sid was lost upstream of this queue (for
// example a slow subscriber inside the session layer). Whatever is queued is
// dropped and the consumer is told to continue at next.
func (m *Mux) Gap(sid SID, next uint64) {
	m.mu.Lock()
	q := m.queues[sid]
	if q == nil {
		q = &outQueue{}
		m.queues[sid] = q
		m.order = append(m.order, sid)
	}
	q.chunks, q.bytes, q.gapped, q.next = nil, 0, true, next
	m.mu.Unlock()
	select {
	case m.wake <- struct{}{}:
	default:
	}
}
