package route

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"termhub/internal/hub/auth"
	"termhub/internal/proto"
)

// route is the in-memory side of a running session: who is attached, who
// drives the size, whether the node is forwarding (docs/M7 第 7 节).
type route struct {
	hub    *Hub
	sid    proto.SID
	nodeID int64

	mu         sync.Mutex
	atts       map[*attachment]struct{}
	dominant   *attachment
	cols, rows int
	forwarding bool
	// Who types: the owner's devices only, unless an administrator took the
	// session over (taker); everyone else attached only watches. One session,
	// one person at the keyboard.
	owner, taker int64
	takerName    string
	telling      sync.Mutex // driver news goes out in the order it happened
}

// attachment is one browser viewing the session.
type attachment struct {
	rt   *route
	ws   *wsConn
	id   *auth.Identity
	name string // device label shown to the other viewers

	mu         sync.Mutex
	next       uint64 // next offset this viewer needs
	replaying  bool
	held       []heldFrame // live frames that arrived during a replay
	cols, rows int
}

type heldFrame struct {
	offset uint64
	data   []byte
}

func (a *attachment) sendMsg(m *proto.Msg) {
	if b, err := json.Marshal(m); err == nil {
		if !a.ws.send(websocket.TextMessage, b) {
			// G2: disconnect if even the reserved control budget is exhausted;
			// do not silently lose results on an apparently healthy connection.
			a.ws.close()
		}
	}
}

// deliver sends output to this viewer, keeping its stream free of holes and
// repeats. A viewer that cannot keep up loses its queue and is told to
// re-attach; nobody else is affected (docs/M1 第 6 节).
func (a *attachment) deliver(offset uint64, data []byte) {
	end := offset + uint64(len(data))
	if end <= a.next {
		return
	}
	if offset > a.next {
		a.gapLocked(offset)
		return
	}
	data = data[a.next-offset:]
	frame, err := proto.AppendBrowserOutput(nil, a.next, data)
	if err != nil {
		return
	}
	if !a.ws.send(websocket.BinaryMessage, frame) {
		a.ws.drop()
		a.gapLocked(end)
		return
	}
	a.next = end
}

func (a *attachment) gapLocked(next uint64) {
	a.replaying, a.held = true, nil // ignore live data until the viewer re-attaches
	a.sendMsg(&proto.Msg{T: proto.MsgGap, Next: next})
}

func (a *attachment) live(offset uint64, data []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.replaying {
		if len(a.held) < 4096 {
			a.held = append(a.held, heldFrame{offset, data})
		}
		return
	}
	a.deliver(offset, data)
}

func (a *attachment) replayBegin(m *proto.Msg) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if m.From != nil {
		a.next = *m.From
	}
	a.sendMsg(&proto.Msg{T: proto.MsgAttached, Mode: m.Mode, From: m.From, Cols: m.Cols, Rows: m.Rows,
		InputAck: true, Prelude: m.Prelude, AltScreen: m.AltScreen})
}

func (a *attachment) replayData(offset uint64, data []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.deliver(offset, data)
}

// replayEnd releases the live frames held back during the replay: whatever
// lies beyond the replay's end follows it without a hole or a repeat.
// The viewer then gets a replay_end of its own (docs/M1 4.2).
func (a *attachment) replayEnd(end uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	held := a.held
	a.replaying, a.held = false, nil
	for _, f := range held {
		a.deliver(f.offset, f.data)
	}
	a.sendMsg(&proto.Msg{T: proto.MsgReplayEnd}) // link A: the viewer may drop its "restoring" notice
}

func (rt *route) output(offset uint64, data []byte) {
	rt.mu.Lock()
	atts := make([]*attachment, 0, len(rt.atts))
	for a := range rt.atts {
		atts = append(atts, a)
	}
	rt.mu.Unlock()
	data = append([]byte(nil), data...) // the frame buffer is reused by the reader
	for _, a := range atts {
		a.live(offset, data)
	}
}

// gap: the node dropped queued output for this session. Every viewer re-attaches.
func (rt *route) gap(next uint64) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for a := range rt.atts {
		a.mu.Lock()
		a.gapLocked(next)
		a.mu.Unlock()
	}
}

func (rt *route) each(fn func(*attachment)) {
	rt.mu.Lock()
	atts := make([]*attachment, 0, len(rt.atts))
	for a := range rt.atts {
		atts = append(atts, a)
	}
	rt.mu.Unlock()
	for _, a := range atts {
		fn(a)
	}
}

func (rt *route) nodeOffline() {
	rt.mu.Lock()
	rt.forwarding = false
	rt.mu.Unlock()
	rt.each(func(a *attachment) { a.sendMsg(&proto.Msg{T: proto.MsgNodeOffline}) })
}

// nodeOnline: the viewers re-attach by themselves when told the node is back.
func (rt *route) nodeOnline() {
	rt.each(func(a *attachment) { a.sendMsg(&proto.Msg{T: proto.MsgNodeOnline}) })
}

func (rt *route) ended(reason string, code *int) {
	rt.each(func(a *attachment) {
		a.sendMsg(&proto.Msg{T: proto.MsgExited, Reason: reason, ExitCode: code})
		go func() { time.Sleep(2 * time.Second); a.ws.close() }()
	})
}

func (rt *route) peers() {
	rt.mu.Lock()
	n := len(rt.atts)
	rt.mu.Unlock()
	rt.each(func(a *attachment) { a.sendMsg(&proto.Msg{T: proto.MsgPeers, Count: n}) })
}

// setForwarding switches the node's live stream with the first and last viewer.
func (rt *route) setForwarding(on bool) {
	rt.mu.Lock()
	if rt.forwarding == on {
		rt.mu.Unlock()
		return
	}
	rt.forwarding = on
	rt.mu.Unlock()
	if n := rt.hub.nodeConn(rt.nodeID); n != nil {
		sid := rt.sid
		n.sendMsg(&proto.Msg{T: proto.MsgForward, SID: &sid, On: &on})
	}
}

// drives: a's user is the one who types into the session now.
func (rt *route) drives(a *attachment) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.driverLocked() == a.id.User.ID
}

func (rt *route) driverLocked() int64 {
	if rt.taker != 0 {
		return rt.taker
	}
	return rt.owner
}

// tellDriver tells each viewer (or only a) whether it may type, and who took
// the session over, if anyone.
func (rt *route) tellDriver(only *attachment) {
	rt.telling.Lock()
	defer rt.telling.Unlock()
	rt.mu.Lock()
	driver, by, cols, rows := rt.driverLocked(), "", rt.cols, rt.rows
	if rt.taker != 0 {
		by = "管理员 " + rt.takerName
	}
	rt.mu.Unlock()
	rt.each(func(a *attachment) {
		if only == nil || a == only {
			on := a.id.User.ID == driver // a watcher shows the typist's size
			a.sendMsg(&proto.Msg{T: proto.MsgDriver, On: &on, By: by, Cols: cols, Rows: rows})
		}
	})
}

// lead makes a the device whose size counts: the last one to type or resize
// (docs/M1 5.2). Its size goes to the node; everyone is told the new size.
func (rt *route) lead(a *attachment) {
	a.mu.Lock()
	cols, rows := a.cols, a.rows
	a.mu.Unlock()
	rt.mu.Lock()
	changed := rt.dominant != a
	rt.dominant = a
	resize := cols > 0 && (cols != rt.cols || rows != rt.rows)
	if resize {
		rt.cols, rt.rows = cols, rows
	}
	rt.mu.Unlock()
	if resize {
		if n := rt.hub.nodeConn(rt.nodeID); n != nil {
			sid := rt.sid
			n.sendMsg(&proto.Msg{T: proto.MsgResize, SID: &sid, Cols: cols, Rows: rows})
		}
	}
	if resize || changed {
		rt.each(func(o *attachment) { o.sendMsg(&proto.Msg{T: proto.MsgSize, Cols: cols, Rows: rows, By: a.name}) })
	}
}

// ServeSession handles /ws/session/{sid}.
func (h *Hub) ServeSession(w http.ResponseWriter, r *http.Request) {
	id, err := h.web.AuthenticateWS(r)
	if err != nil {
		h.web.Fail(w, r, err)
		return
	}
	sidText := r.PathValue("sid")
	sid, perr := proto.ParseSID(sidText)
	row, err := h.reg.session(sidText)
	// The same answer for "does not exist" and "not yours": ids are not secrets,
	// but they must not confirm anything either.
	if perr != nil || err != nil || !h.reg.CanAccessSession(id, row) || row.EndedAt != 0 {
		h.web.Fail(w, r, ErrNotFound)
		return
	}
	foreign := row.OwnerID != id.User.ID
	if foreign && !id.Reverified { // an administrator looking into someone else's terminal
		h.web.Fail(w, r, auth.ErrReverifyRequired)
		return
	}
	rt := h.route(sid)
	if rt == nil {
		rt = h.ensureRoute(sid, row.NodeID, 0, 0)
	}
	rt.mu.Lock()
	rt.owner = row.OwnerID
	rt.mu.Unlock()
	c, err := h.up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	a := &attachment{rt: rt, ws: newWSConn(c, proto.SessionQueue, proto.MaxJSON), id: id,
		name: id.User.Username, replaying: true}
	if foreign {
		a.name = "管理员 " + id.User.Username
		h.web.S.Audit(&id.User, id.IP, "session_attach_foreign", sidText, "ok", "")
	}
	stop := h.watchLogin(r, a.ws, func(now *auth.Identity) bool { return h.reg.CanAccessSession(now, row) })
	defer func() {
		close(stop)
		a.ws.close()
		rt.mu.Lock()
		delete(rt.atts, a)
		if rt.dominant == a {
			rt.dominant = nil
		}
		empty := len(rt.atts) == 0
		// the administrator who took it over is gone: the keyboard goes back to the owner
		back := rt.taker != 0 && !rt.attachedLocked(rt.taker)
		if back {
			rt.taker, rt.takerName = 0, ""
		}
		rt.mu.Unlock()
		if back {
			rt.tellDriver(nil)
			h.web.S.Audit(nil, "", "session_take_back", sidText, "ok", "detached")
		}
		if empty {
			rt.setForwarding(false) // nobody watches: the node stops sending
		}
		rt.peers()
		h.web.S.Audit(&id.User, id.IP, "session_detach", sidText, "ok", "")
	}()

	for {
		kind, data, err := c.ReadMessage()
		if err != nil {
			return
		}
		a.ws.touch()
		if kind == websocket.BinaryMessage { // terminal input
			if proto.CheckBrowserInput(data) != nil {
				return
			}
			if code := a.queueInput(data); code != "" {
				a.sendMsg(&proto.Msg{T: proto.MsgErr, Code: code})
			}
			continue
		}
		m, err := proto.DecodeMsg(data)
		if err != nil {
			return
		}
		switch m.T {
		case proto.MsgInputBatch:
			if m.ID == 0 || proto.CheckBrowserInput(m.Data) != nil {
				return
			}
			code := a.queueInput(m.Data)
			a.sendMsg(&proto.Msg{T: proto.MsgInputResult, Re: m.ID, Code: code})
		case proto.MsgAttach:
			a.mu.Lock()
			a.cols, a.rows = m.Cols, m.Rows
			a.replaying, a.held = true, nil
			a.mu.Unlock()
			rt.mu.Lock()
			first := len(rt.atts) == 0
			_, already := rt.atts[a]
			rt.atts[a] = struct{}{}
			rt.mu.Unlock()
			if !already {
				h.web.S.Audit(&id.User, id.IP, "session_attach", sidText, "ok", "")
			}
			rt.tellDriver(a) // node there or not: a watcher knows at once
			n := h.nodeConn(rt.nodeID)
			if n == nil {
				a.sendMsg(&proto.Msg{T: proto.MsgNodeOffline})
				continue
			}
			// Live stream first, then the replay: the replay's end can only lie
			// at or beyond the point where live data starts, so nothing is lost.
			rt.setForwarding(true)
			n.startReplay(a, m.Have)
			if first && m.Cols > 0 && rt.drives(a) {
				rt.lead(a)
			}
			rt.peers()
		case proto.MsgResize:
			a.mu.Lock()
			a.cols, a.rows = m.Cols, m.Rows
			a.mu.Unlock()
			if rt.drives(a) { // a watcher's window does not reshape the typist's screen
				rt.lead(a)
			}
		case proto.MsgRedraw:
			if n := h.nodeConn(rt.nodeID); n != nil {
				n.sendMsg(&proto.Msg{T: proto.MsgRedraw, SID: &sid})
			}
		case proto.MsgPing:
			a.sendMsg(&proto.Msg{T: proto.MsgPong})
		}
	}
}

// attachedLocked: one of the user's devices is attached. rt.mu held.
func (rt *route) attachedLocked(user int64) bool {
	for a := range rt.atts {
		if a.id.User.ID == user {
			return true
		}
	}
	return false
}

// TakeSession: an administrator takes someone else's session over, so that
// only they type into it (reverified each time, 管理员接管); the owner takes
// it back. Either way everyone attached is told.
func (h *Hub) TakeSession(id *auth.Identity, sidText string) error {
	row, err := h.reg.session(sidText)
	sid, perr := proto.ParseSID(sidText)
	if perr != nil || err != nil || !h.reg.CanAccessSession(id, row) || row.EndedAt != 0 {
		return ErrNotFound
	}
	foreign := row.OwnerID != id.User.ID
	if foreign && !h.web.S.FreshlyReverified(id) {
		return auth.ErrReverifyRequired
	}
	rt := h.ensureRoute(sid, row.NodeID, 0, 0)
	rt.mu.Lock()
	rt.owner = row.OwnerID
	if foreign && !rt.attachedLocked(id.User.ID) {
		// taken over only from an open terminal: gone already, nobody would
		// hand it back when leaving
		rt.mu.Unlock()
		return ErrNotAttached
	}
	if foreign {
		rt.taker, rt.takerName = id.User.ID, id.User.Username
	} else {
		rt.taker, rt.takerName = 0, ""
	}
	rt.mu.Unlock()
	rt.tellDriver(nil)
	action := "session_take"
	if !foreign {
		action = "session_take_back"
	}
	h.web.S.Audit(&id.User, id.IP, action, sidText, "ok", "")
	return nil
}

// typist: the user who types into a session now (the owner unless taken over).
func (h *Hub) typist(row SessionRow) int64 {
	if sid, err := proto.ParseSID(row.SID); err == nil {
		if rt := h.route(sid); rt != nil {
			rt.mu.Lock()
			defer rt.mu.Unlock()
			if rt.taker != 0 {
				return rt.taker
			}
		}
	}
	return row.OwnerID
}

// Queue admission only, not an acknowledgement that the program consumed input.
// Hold the driver lock through enqueue so a takeover cannot split that decision.
func (a *attachment) queueInput(data []byte) string {
	rt := a.rt
	rt.mu.Lock()
	driver := rt.owner
	if rt.taker != 0 {
		driver = rt.taker
	}
	if a.id.User.ID != driver {
		rt.mu.Unlock()
		return proto.ErrReadOnly
	}
	n := rt.hub.nodeConn(rt.nodeID)
	if n == nil {
		rt.mu.Unlock()
		return proto.ErrNodeOffline
	}
	f, err := proto.AppendFrame(nil, proto.Frame{Type: proto.TypeInput, SID: rt.sid, Data: data})
	if err != nil {
		rt.mu.Unlock()
		return proto.ErrInputOverflow
	}
	ok := n.ws.send(websocket.BinaryMessage, f)
	rt.mu.Unlock()
	if !ok {
		return proto.ErrInputOverflow
	}
	rt.lead(a)
	return ""
}
