package route

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const (
	wsControlLimit = 512 << 10 // G2: reserved bytes, independent of terminal output
	wsWriteWait    = 15 * time.Second
	wsPingEvery    = 20 * time.Second
	wsDeadAfter    = 60 * time.Second
)

type wsMsg struct {
	kind int
	data []byte
}

// wsConn gives a WebSocket a single writer and a bounded, byte-accounted send
// queue. Producers never block: when the queue is full, send reports false and
// the caller decides what that means (docs/M1 第 6 节).
type wsConn struct {
	mu           sync.Mutex // serializes enqueue, dequeue and selective drop
	wake         chan struct{}
	controlBytes int64
	c            *websocket.Conn
	out          chan wsMsg
	queued       atomic.Int64
	limit        int64
	done         chan struct{}
	once         sync.Once
}

func newWSConn(c *websocket.Conn, limitBytes int64, readLimit int64) *wsConn {
	w := &wsConn{c: c, out: make(chan wsMsg, 4096), limit: limitBytes, wake: make(chan struct{}, 1), done: make(chan struct{})}
	c.SetReadLimit(readLimit)
	c.SetReadDeadline(time.Now().Add(wsDeadAfter))
	c.SetPongHandler(func(string) error { return c.SetReadDeadline(time.Now().Add(wsDeadAfter)) })
	go w.writeLoop()
	return w
}

// touch extends the read deadline; any received frame proves the peer is alive.
func (w *wsConn) touch() { w.c.SetReadDeadline(time.Now().Add(wsDeadAfter)) }

func (w *wsConn) send(kind int, data []byte) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	select {
	case <-w.done:
		return false
	default:
	}
	if kind == websocket.TextMessage {
		if w.controlBytes+int64(len(data)) > wsControlLimit {
			return false
		}
	} else {
		// Reserve slots as well as bytes for control messages. Keep one FIFO:
		// attached/replay_end must not overtake the output they describe.
		reserve := min(128, cap(w.out)/2)
		if w.queued.Load()-w.controlBytes+int64(len(data)) > w.limit || len(w.out) >= cap(w.out)-reserve {
			return false
		}
	}
	select {
	case w.out <- wsMsg{kind, data}:
		w.queued.Add(int64(len(data)))
		if kind == websocket.TextMessage {
			w.controlBytes += int64(len(data))
		}
		select {
		case w.wake <- struct{}{}:
		default:
		}
		return true
	case <-w.done:
		return false // a closed link did not accept input
	default:
		return false
	}
}

// drop discards output only; input results and other controls retain FIFO order.
func (w *wsConn) drop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	var controls []wsMsg
	for len(w.out) > 0 {
		m := <-w.out
		if m.kind == websocket.TextMessage {
			controls = append(controls, m)
		} else {
			w.queued.Add(-int64(len(m.data)))
		}
	}
	for _, m := range controls {
		w.out <- m
	}
}

func (w *wsConn) take() (wsMsg, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	select {
	case m := <-w.out:
		w.queued.Add(-int64(len(m.data)))
		if m.kind == websocket.TextMessage {
			w.controlBytes -= int64(len(m.data))
		}
		return m, true
	default:
		return wsMsg{}, false
	}
}

func (w *wsConn) writeLoop() {
	ping := time.NewTicker(wsPingEvery)
	defer ping.Stop()
	for {
		select {
		case <-w.done:
			return
		case <-ping.C:
			w.c.SetWriteDeadline(time.Now().Add(wsWriteWait))
			if w.c.WriteMessage(websocket.PingMessage, nil) != nil {
				w.close()
				return
			}
		default:
		}
		if m, ok := w.take(); ok {
			w.c.SetWriteDeadline(time.Now().Add(wsWriteWait))
			if w.c.WriteMessage(m.kind, m.data) != nil {
				w.close()
				return
			}
			continue
		}
		select {
		case <-w.wake:
		case <-ping.C:
			w.c.SetWriteDeadline(time.Now().Add(wsWriteWait))
			if w.c.WriteMessage(websocket.PingMessage, nil) != nil {
				w.close()
				return
			}
		case <-w.done:
			return
		}
	}
}

func (w *wsConn) close() {
	w.once.Do(func() {
		close(w.done)
		if w.c != nil {
			w.c.Close()
		}
	})
}
