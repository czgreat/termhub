//go:build windows

package host

import (
	"errors"
	"net"
	"sync"

	"termhub/internal/proto"
	"termhub/internal/session"
)

const replayPiece = 256 << 10

// client is the one agent connection (docs/M1 链路 C).
type client struct {
	h       *Host
	conn    net.Conn
	mux     *proto.Mux
	ctrl    chan []byte // encoded control frames; never blocks the sender
	replay  chan []byte // encoded replay frames; its producers may wait
	done    chan struct{}
	once    sync.Once
	greeted bool
}

func newClient(h *Host, conn net.Conn) *client {
	return &client{h: h, conn: conn, mux: proto.NewMux(proto.SessionQueue),
		ctrl: make(chan []byte, 2048), replay: make(chan []byte, 8), done: make(chan struct{})}
}

func (c *client) close() {
	c.once.Do(func() {
		close(c.done)
		c.conn.Close()
		h := c.h
		h.mu.Lock()
		if h.client == c {
			h.client, h.linkUp = nil, false
		}
		var stops []func()
		for _, e := range h.sessions {
			if e.stopFwd != nil {
				stops = append(stops, e.stopFwd)
				e.stopFwd = nil
			}
		}
		h.mu.Unlock()
		for _, stop := range stops {
			stop()
		}
	})
}

// send queues a control message. A client that lets 2048 of them pile up is
// broken and gets disconnected; the sessions are unaffected.
func (c *client) send(m *proto.Msg) {
	b, err := proto.EncodeMsg(m)
	if err != nil {
		return
	}
	f, err := proto.AppendFrame(nil, proto.Frame{Type: proto.TypeControl, Data: b})
	if err != nil {
		return
	}
	select {
	case c.ctrl <- f:
	case <-c.done:
	default:
		c.close()
	}
}

func (c *client) writeLoop() {
	write := func(f []byte) bool {
		if proto.WritePipeFrame(c.conn, f) != nil {
			c.close()
			return false
		}
		return true
	}
	for {
		select { // control first
		case f := <-c.ctrl:
			if !write(f) {
				return
			}
			continue
		default:
		}
		select {
		case f := <-c.replay:
			if !write(f) {
				return
			}
			continue
		default:
		}
		if it, ok := c.mux.Next(); ok {
			if it.Gap {
				sid := it.SID
				c.send(&proto.Msg{T: proto.MsgGap, SID: &sid, Next: it.Offset})
				continue
			}
			f, err := proto.AppendFrame(nil, proto.Frame{Type: proto.TypeOutput, SID: it.SID, Offset: it.Offset, Data: it.Data})
			if err == nil && !write(f) {
				return
			}
			continue
		}
		select {
		case f := <-c.ctrl:
			if !write(f) {
				return
			}
		case f := <-c.replay:
			if !write(f) {
				return
			}
		case <-c.mux.Wake():
		case <-c.done:
			return
		}
	}
}

func (c *client) run() {
	defer c.close()
	go c.writeLoop()
	for {
		raw, err := proto.ReadPipeFrame(c.conn)
		if err != nil {
			return // includes oversized frames: drop the connection, keep the host
		}
		f, err := proto.ParseFrame(raw)
		if err != nil {
			return
		}
		switch f.Type {
		case proto.TypeControl:
			m, err := proto.DecodeMsg(f.Data)
			if err != nil {
				return
			}
			if !c.greeted && m.T != proto.MsgHello {
				return
			}
			c.handle(m)
		case proto.TypeInput:
			if !c.greeted {
				return
			}
			sid := f.SID
			e, err := c.h.get(&sid)
			if err == nil {
				err = e.s.Write(f.Data)
			}
			if err != nil {
				c.send(failure(&proto.Msg{SID: &sid}, err))
			}
		default:
			return
		}
	}
}

func failure(req *proto.Msg, err error) *proto.Msg {
	var se *session.Error
	if errors.As(err, &se) {
		return req.Fail(se.Code, se.Msg)
	}
	return req.Fail(proto.ErrInternal, err.Error())
}

func (c *client) handle(m *proto.Msg) {
	h := c.h
	answer := func(err error) {
		if !m.IsRequest() {
			return
		}
		if err != nil {
			c.send(failure(m, err))
		} else {
			c.send(m.Reply())
		}
	}
	switch m.T {
	case proto.MsgHello:
		remote, err := proto.ParseVersion(m.Proto)
		if err == nil {
			remote, err = proto.Negotiate(remote)
		}
		if err != nil {
			c.send(m.Fail(proto.ErrProtoMismatch, "host speaks "+proto.Current.String()))
			return
		}
		c.greeted = true
		w := m.Reply()
		w.T, w.Proto, w.HostVer = proto.MsgWelcome, remote.String(), Version
		c.send(w)
		c.send(&proto.Msg{T: proto.MsgSessions, List: h.list()})
		h.mu.Lock()
		exits := h.exits
		h.exits = nil
		h.mu.Unlock()
		for _, e := range exits { // sessions that ended while no agent was connected
			c.send(e)
		}
	case proto.MsgPing:
		c.send(&proto.Msg{T: proto.MsgPong, Re: m.ID})
	case proto.MsgLink:
		h.mu.Lock()
		h.linkUp = m.Up != nil && *m.Up
		h.mu.Unlock()
	case proto.MsgSessions:
		r := m.Reply()
		r.List = h.list()
		c.send(r)
	case proto.MsgCreate:
		err := h.create(m)
		answer(err)
		if err == nil {
			c.send(&proto.Msg{T: proto.MsgSessions, List: h.list()})
		}
	case proto.MsgForward:
		e, err := h.get(m.SID)
		if err != nil {
			answer(err)
			return
		}
		if m.On != nil && *m.On {
			next := c.startForward(e, m.From)
			r := m.Reply()
			r.From = &next
			if m.IsRequest() {
				c.send(r)
			}
		} else {
			c.stopForward(e)
			answer(nil)
		}
	case proto.MsgReplay:
		e, err := h.get(m.SID)
		if err != nil {
			answer(err)
			return
		}
		go c.sendReplay(e, m)
	case proto.MsgResize:
		e, err := h.get(m.SID)
		if err == nil {
			err = e.s.Resize(m.Cols, m.Rows)
		}
		answer(err)
	case proto.MsgRedraw:
		e, err := h.get(m.SID)
		if err == nil {
			go e.s.Redraw()
		}
		answer(err)
	case proto.MsgClose:
		e, err := h.get(m.SID)
		if err == nil {
			mode := session.Graceful
			if m.Mode == proto.CloseForce {
				mode = session.Force
			}
			reason := m.Reason
			if reason == "" {
				reason = session.ReasonUser
			}
			e.s.Close(mode, reason)
		}
		answer(err)
	case proto.MsgShutdown:
		h.mu.Lock()
		n := len(h.sessions)
		h.mu.Unlock()
		if n > 0 && !m.Force {
			c.send(m.Fail(proto.ErrBusy, "sessions are running"))
			return
		}
		answer(nil)
		go h.Shutdown()
	default:
		if m.IsRequest() {
			c.send(m.Fail(proto.ErrUnsupported, m.T))
		}
	}
}

// startForward streams a session's live output into the mux, starting at
// *from when that offset is still buffered, otherwise at the current end. It
// returns the offset the stream really starts at.
func (c *client) startForward(e *entry, from *uint64) uint64 {
	c.stopForward(e)
	ch, cancel := e.s.Subscribe() // subscribe first, then look: nothing can fall between
	var next, start uint64
	if from != nil {
		r := e.s.Replay(from)
		next, start = r.End, r.End
		if r.Resume {
			c.mux.Push(e.sid, r.From, r.Data)
			start = r.From
		} else {
			c.mux.Gap(e.sid, next) // the requested offset is gone
		}
	} else {
		next = e.s.Info().End
		start = next
	}
	stop := make(chan struct{})
	var once sync.Once
	stopFn := func() { once.Do(func() { close(stop); cancel(); c.mux.Drop(e.sid) }) }
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-c.done:
				return
			case ck, ok := <-ch:
				if !ok {
					return
				}
				if ck.Gap {
					c.mux.Gap(e.sid, ck.Offset)
					next = ck.Offset
				}
				end := ck.Offset + uint64(len(ck.Data))
				switch {
				case end <= next: // already delivered by the resume above
					continue
				case ck.Offset > next:
					c.mux.Gap(e.sid, ck.Offset)
					next = ck.Offset
				}
				c.mux.Push(e.sid, next, ck.Data[next-ck.Offset:])
				next = end
			}
		}
	}()
	c.h.mu.Lock()
	e.stopFwd, e.idle = stopFn, 0
	c.h.mu.Unlock()
	return start
}

func (c *client) stopForward(e *entry) {
	c.h.mu.Lock()
	stop := e.stopFwd
	e.stopFwd = nil
	c.h.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// sendReplay answers a replay request: replay_begin (mode, start, size,
// prelude), the data in replay frames, replay_end (docs/M1 4.4).
func (c *client) sendReplay(e *entry, m *proto.Msg) {
	r := e.s.Replay(m.From)
	info := e.s.Info()
	mode := proto.ModeReplay
	if r.Resume {
		mode = proto.ModeResume
	}
	from := r.From
	c.send(&proto.Msg{T: proto.MsgReplayBegin, SID: m.SID, Req: m.Req, Re: m.ID, Mode: mode, From: &from,
		Cols: info.Cols, Rows: info.Rows, Prelude: r.Prelude, AltScreen: r.AltScreen})
	off := r.From
	for data := r.Data; len(data) > 0; {
		n := min(len(data), replayPiece)
		f, err := proto.AppendFrame(nil, proto.Frame{Type: proto.TypeReplay, SID: e.sid, Req: m.Req, Offset: off, Data: data[:n]})
		if err != nil {
			return
		}
		select {
		case c.replay <- f:
		case <-c.done:
			return
		}
		off += uint64(n)
		data = data[n:]
	}
	// replay_end must follow the last replay frame, so it travels the same queue.
	b, err := proto.EncodeMsg(&proto.Msg{T: proto.MsgReplayEnd, SID: m.SID, Req: m.Req, End: r.End})
	if err != nil {
		return
	}
	f, _ := proto.AppendFrame(nil, proto.Frame{Type: proto.TypeControl, Data: b})
	select {
	case c.replay <- f:
	case <-c.done:
	}
}
