package route

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"termhub/internal/hub/auth"
	"termhub/internal/proto"
)

// Limits of docs/M7 第 6 节 (总体设计 D5).
type Limits struct {
	PerUser int // default 20
	PerNode int // default 64
}

// Hub routes terminal sessions between browsers and nodes.
type Hub struct {
	reg    *Registry
	web    *auth.HTTP
	log    *slog.Logger
	limits Limits
	up     websocket.Upgrader

	mu     sync.Mutex
	nodes  map[int64]*nodeConn
	routes map[proto.SID]*route
	events map[*eventConn]struct{}
	xfer   *transfers

	starting sync.Mutex // the folder check and the insert of CreateSession, as one step
	titles   coalescer  // sessions_changed for title changes, at most every titleEvery
	usage    usageCache // each session's dollar figure and context share, briefly
}

func NewHub(reg *Registry, web *auth.HTTP, limits Limits, log *slog.Logger) *Hub {
	if limits.PerUser == 0 {
		limits.PerUser = 20
	}
	if limits.PerNode == 0 {
		limits.PerNode = 64
	}
	if log == nil {
		log = slog.Default()
	}
	h := &Hub{reg: reg, web: web, log: log, limits: limits,
		// Origin is checked by auth.AuthenticateWS for browsers; nodes send none.
		up:    websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }, ReadBufferSize: 32 << 10, WriteBufferSize: 32 << 10},
		nodes: map[int64]*nodeConn{}, routes: map[proto.SID]*route{}, events: map[*eventConn]struct{}{}, xfer: newTransfers()}
	h.titles = coalescer{every: titleEvery, fire: h.broadcastSessions}
	return h
}

// ---- node link (docs/M1 链路 B, M7 第 3、8 节) ----

type nodeConn struct {
	hub      *Hub
	node     Node
	ws       *wsConn
	mu       sync.Mutex
	nextID   uint64
	pending  map[uint64]chan *proto.Msg
	creates  map[uint64]proto.SID // retained until a late reply or this connection ends
	creating map[proto.SID]bool
	replays  map[uint32]*replayWait
	nextReq  uint32
}

type replayWait struct {
	att   *attachment
	begin *proto.Msg
}

// ServeNode handles /node/link. Authentication happens before the upgrade, so
// a wrong token never gets a WebSocket.
func (h *Hub) ServeNode(w http.ResponseWriter, r *http.Request) {
	node, err := h.reg.AuthenticateNode(r.Header.Get("X-TH-Node-Token"))
	if err != nil {
		h.web.S.AuditNoisy(h.web.ClientIP(r), "node_auth", "failed") // one line per address and 15 minutes
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	c, err := h.up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	n := &nodeConn{hub: h, node: node, ws: newWSConn(c, 8<<20, proto.MaxFrame+64),
		pending: map[uint64]chan *proto.Msg{}, replays: map[uint32]*replayWait{}}
	n.run(h.web.ClientIP(r))
}

func (n *nodeConn) sendMsg(m *proto.Msg) bool {
	b, err := proto.EncodeMsg(m)
	if err != nil {
		return false
	}
	if !n.ws.send(websocket.TextMessage, b) {
		n.ws.close() // a node that cannot take control traffic is broken
		return false
	}
	return true
}

// request sends m and waits for the node's answer.
func (n *nodeConn) request(m *proto.Msg, timeout time.Duration) (*proto.Msg, error) {
	ch := make(chan *proto.Msg, 1)
	n.mu.Lock()
	n.nextID++
	m.ID = n.nextID
	n.pending[m.ID] = ch
	if m.T == proto.MsgCreate && m.SID != nil {
		if n.creates == nil {
			n.creates = map[uint64]proto.SID{}
		}
		if n.creating == nil {
			n.creating = map[proto.SID]bool{}
		}
		n.creates[m.ID] = *m.SID
		n.creating[*m.SID] = true
	}
	n.mu.Unlock()
	defer func() { n.mu.Lock(); delete(n.pending, m.ID); n.mu.Unlock() }()
	if !n.sendMsg(m) {
		return nil, ErrNodeOffline
	}
	select {
	case r := <-ch:
		if r.T == proto.MsgErr {
			return r, &auth.Error{Code: r.Code, Msg: r.Text, Status: 409}
		}
		return r, nil
	case <-n.ws.done:
		return nil, ErrNodeOffline
	case <-time.After(timeout):
		return nil, &auth.Error{Code: "node_timeout", Msg: "节点没有在规定时间内应答", Status: 504}
	}
}

func (n *nodeConn) run(ip string) {
	h := n.hub
	defer n.ws.close()
	// hello must come first.
	_, raw, err := n.ws.c.ReadMessage()
	if err != nil {
		return
	}
	hello, err := proto.DecodeMsg(raw)
	if err != nil || hello.T != proto.MsgHello {
		return
	}
	ver, err := proto.ParseVersion(hello.Proto)
	if err == nil {
		ver, err = proto.Negotiate(ver)
	}
	if err != nil {
		n.sendMsg(hello.Fail(proto.ErrProtoMismatch, "hub speaks "+proto.Current.String()))
		time.Sleep(200 * time.Millisecond)
		return
	}
	if !h.reg.CheckFingerprint(n.node.ID, hello.Fingerprint) {
		h.web.S.Audit(nil, ip, "node_fingerprint", n.node.Name, "mismatch", "")
		n.sendMsg(hello.Fail(proto.ErrForbidden, "machine fingerprint does not match the registered one"))
		time.Sleep(200 * time.Millisecond)
		return
	}
	h.reg.noteHello(n.node.ID, hello)
	w := hello.Reply()
	w.T, w.Proto, w.NodeID = proto.MsgWelcome, ver.String(), n.node.Name
	n.sendMsg(w)

	h.mu.Lock()
	old := h.nodes[n.node.ID]
	h.nodes[n.node.ID] = n
	h.mu.Unlock()
	if old != nil { // a newer connection replaces the old one
		old.ws.close()
	}
	h.web.S.Audit(nil, ip, "node_online", n.node.Name, "ok", hello.AgentVer)
	h.broadcastNode(n.node.ID, true)

	defer func() {
		h.mu.Lock()
		current := h.nodes[n.node.ID] == n
		if current {
			delete(h.nodes, n.node.ID)
		}
		var affected []*route
		for _, rt := range h.routes {
			if rt.nodeID == n.node.ID {
				affected = append(affected, rt)
			}
		}
		h.mu.Unlock()
		if current {
			for _, rt := range affected {
				rt.nodeOffline()
			}
			h.broadcastNode(n.node.ID, false)
		}
	}()

	for {
		kind, data, err := n.ws.c.ReadMessage()
		if err != nil {
			return
		}
		n.ws.touch()
		if kind == websocket.BinaryMessage {
			f, err := proto.ParseFrame(data)
			if err != nil {
				return
			}
			n.handleFrame(f)
			continue
		}
		m, err := proto.DecodeMsg(data)
		if err != nil {
			return
		}
		n.handleMsg(m)
	}
}

func (n *nodeConn) handleFrame(f proto.Frame) {
	switch f.Type {
	case proto.TypeOutput:
		if rt := n.hub.route(f.SID); rt != nil && rt.nodeID == n.node.ID {
			rt.output(f.Offset, f.Data)
		}
	case proto.TypeReplay:
		n.mu.Lock()
		w := n.replays[f.Req]
		n.mu.Unlock()
		if w != nil {
			w.att.replayData(f.Offset, f.Data)
		}
	}
}

func (n *nodeConn) handleMsg(m *proto.Msg) {
	if m.Re != 0 && m.T != proto.MsgReplayBegin {
		n.mu.Lock()
		ch := n.pending[m.Re]
		sid, creating := n.creates[m.Re]
		if creating {
			delete(n.creates, m.Re)
		}
		n.mu.Unlock()
		if creating && n.ownsSession(&sid) {
			if m.T == proto.MsgErr {
				n.hub.reg.db.Exec(`UPDATE sessions SET ended_at=?,end_reason='start_failed' WHERE sid=? AND node_id=? AND end_reason IN ('starting','start_unknown')`, time.Now().Unix(), sid.String(), n.node.ID)
			} else if m.T == proto.MsgOK {
				n.hub.reg.db.Exec(`UPDATE sessions SET ended_at=NULL,end_reason='' WHERE sid=? AND node_id=? AND end_reason IN ('starting','start_unknown','lost')`, sid.String(), n.node.ID)
			}
			n.hub.broadcastSessions()
		}
		if creating {
			n.mu.Lock()
			delete(n.creating, sid)
			n.mu.Unlock()
		}
		if ch != nil {
			select {
			case ch <- m:
			default:
			}
		}
		return
	}
	switch m.T {
	case proto.MsgSessions:
		n.hub.reconcile(n, m.List)
	case proto.MsgSessionExited:
		if n.ownsSession(m.SID) {
			n.hub.sessionEnded(n.node.ID, *m.SID, m.Reason, m.ExitCode)
		}
	case proto.MsgGap:
		if n.ownsSession(m.SID) {
			if rt := n.hub.route(*m.SID); rt != nil {
				rt.gap(m.Next)
			}
		}
	case proto.MsgTitle:
		if n.ownsSession(m.SID) {
			n.hub.reg.db.Exec(`UPDATE sessions SET title=? WHERE sid=? AND node_id=?`, m.Title, m.SID.String(), n.node.ID)
			n.hub.titles.poke()
		}
	case proto.MsgReplayBegin, proto.MsgReplayEnd:
		n.mu.Lock()
		w := n.replays[m.Req]
		if m.T == proto.MsgReplayEnd {
			delete(n.replays, m.Req)
		}
		n.mu.Unlock()
		if w == nil {
			return
		}
		if m.T == proto.MsgReplayBegin {
			w.att.replayBegin(m)
		} else {
			w.att.replayEnd(m.End)
		}
	case proto.MsgPing:
		n.sendMsg(&proto.Msg{T: proto.MsgPong, Re: m.ID})
	case proto.MsgErr:
		if n.ownsSession(m.SID) && (m.Code == proto.ErrInputOverflow || m.Code == "input_stalled" || m.Code == "not_running") {
			if rt := n.hub.route(*m.SID); rt != nil {
				rt.each(func(a *attachment) {
					if rt.drives(a) {
						a.sendMsg(m)
					}
				})
			}
		}
	}
}

// All session notifications belong to this node, including ones sent before
// a browser has attached and created a route (#7 F03).
func (n *nodeConn) ownsSession(sid *proto.SID) bool {
	if sid == nil || n.hub.nodeConn(n.node.ID) != n {
		return false
	}
	if rt := n.hub.route(*sid); rt != nil {
		return rt.nodeID == n.node.ID
	}
	var nodeID int64
	return n.hub.reg.db.QueryRow(`SELECT node_id FROM sessions WHERE sid=?`, sid.String()).Scan(&nodeID) == nil && nodeID == n.node.ID
}

// startReplay asks the node for catch-up data on behalf of an attachment.
func (n *nodeConn) startReplay(att *attachment, have *uint64) {
	n.mu.Lock()
	n.nextReq++
	req := n.nextReq
	n.replays[req] = &replayWait{att: att}
	n.mu.Unlock()
	sid := att.rt.sid
	n.sendMsg(&proto.Msg{T: proto.MsgReplay, SID: &sid, Req: req, From: have})
}

func (h *Hub) nodeConn(id int64) *nodeConn {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.nodes[id]
}

// Online reports whether a node is connected.
func (h *Hub) Online(nodeID int64) bool { return h.nodeConn(nodeID) != nil }

func (h *Hub) route(sid proto.SID) *route {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.routes[sid]
}

// reconcile brings the register in line with what the node really runs
// (docs/M7 第 8 节).
func (h *Hub) reconcile(n *nodeConn, list []proto.SessionInfo) {
	onNode := map[string]proto.SessionInfo{}
	for _, s := range list {
		if s.State != "exited" {
			onNode[s.SID.String()] = s
		}
	}
	h.starting.Lock()
	rows, err := h.reg.db.Query(`SELECT sid, node_id, ended_at IS NULL FROM sessions WHERE sid IN (SELECT sid FROM sessions WHERE node_id=? AND ended_at IS NULL)`, n.node.ID)
	known := map[string]bool{}
	if err == nil {
		for rows.Next() {
			var sid string
			var nodeID int64
			var running bool
			rows.Scan(&sid, &nodeID, &running)
			known[sid] = true
		}
		rows.Close()
	}
	now := time.Now().Unix()
	for sid := range known { // the Hub believes it runs, the node does not have it
		if _, ok := onNode[sid]; !ok {
			parsed, _ := proto.ParseSID(sid)
			n.mu.Lock()
			creating := n.creating[parsed]
			n.mu.Unlock()
			if creating {
				continue
			}
			h.reg.db.Exec(`UPDATE sessions SET ended_at=?, end_reason='lost' WHERE sid=? AND ended_at IS NULL`, now, sid)
			if id, err := proto.ParseSID(sid); err == nil {
				h.dropRoute(id, "lost", nil)
			}
		}
	}
	h.starting.Unlock()
	for sid, s := range onNode {
		var otherNode int64
		err := h.reg.db.QueryRow(`SELECT node_id FROM sessions WHERE sid=?`, sid).Scan(&otherNode)
		switch {
		case err == nil && otherNode != n.node.ID:
			h.log.Warn("node reports a session that belongs to another node", "node", n.node.Name, "sid", sid)
			continue
		case errors.Is(err, sql.ErrNoRows): // unknown to the Hub: adopt it. Without a known owner it is an orphan, visible to admins only.
			var ownerID any
			var uid int64
			if h.reg.db.QueryRow(`SELECT id FROM users WHERE username=?`, s.Owner).Scan(&uid) == nil {
				ownerID = uid
			}
			kind := s.Kind
			if kind == "" { // older hosts did not report their creation snapshot
				h.reg.db.QueryRow(`SELECT kind FROM cli_profiles WHERE node_id=? AND name=?`, n.node.ID, s.Profile).Scan(&kind)
			}
			if _, err := h.reg.db.Exec(`INSERT INTO sessions(sid,node_id,profile_name,owner_id,cwd,title,created_at,kind) VALUES (?,?,?,?,?,?,?,?)`,
				sid, n.node.ID, s.Profile, ownerID, s.Cwd, s.Title, s.Created, kind); err != nil {
				continue
			}
		case err != nil:
			continue // a database error is not proof that this is an unknown SID
		default:
			// A missing acknowledgment is not a failed start (#7 F04). Do not
			// resurrect an explicitly ended session or replace its snapshots.
			h.reg.db.Exec(`UPDATE sessions SET ended_at=NULL,end_reason='' WHERE sid=? AND node_id=? AND end_reason IN ('starting','start_unknown','lost')`, sid, n.node.ID)
			var ended bool
			if err := h.reg.db.QueryRow(`SELECT ended_at IS NOT NULL FROM sessions WHERE sid=?`, sid).Scan(&ended); err != nil || ended {
				continue
			}
		}
		h.ensureRoute(s.SID, n.node.ID, s.Cols, s.Rows)
	}
	h.mu.Lock()
	var mine []*route
	for _, rt := range h.routes {
		if rt.nodeID == n.node.ID {
			mine = append(mine, rt)
		}
	}
	h.mu.Unlock()
	for _, rt := range mine {
		rt.nodeOnline()
	}
	h.broadcastSessions()
}

func (h *Hub) ensureRoute(sid proto.SID, nodeID int64, cols, rows int) *route {
	h.mu.Lock()
	defer h.mu.Unlock()
	rt := h.routes[sid]
	if rt == nil {
		rt = &route{hub: h, sid: sid, nodeID: nodeID, cols: cols, rows: rows, atts: map[*attachment]struct{}{}}
		h.routes[sid] = rt
	}
	return rt
}

func (h *Hub) dropRoute(sid proto.SID, reason string, code *int) {
	h.mu.Lock()
	rt := h.routes[sid]
	delete(h.routes, sid)
	h.mu.Unlock()
	if rt != nil {
		rt.ended(reason, code)
	}
}

func (h *Hub) sessionEnded(nodeID int64, sid proto.SID, reason string, code *int) {
	var ec any
	if code != nil {
		ec = *code
	}
	result, err := h.reg.db.Exec(`UPDATE sessions SET ended_at=?, end_reason=?, exit_code=? WHERE sid=? AND node_id=? AND ended_at IS NULL`,
		time.Now().Unix(), reason, ec, sid.String(), nodeID)
	if err != nil {
		return
	}
	if changed, _ := result.RowsAffected(); changed == 0 {
		return
	}
	h.dropRoute(sid, reason, code)
	h.web.S.Audit(nil, "", "session_ended", sid.String(), "ok", reason)
	h.broadcastSessions()
}

// ---- creating and closing sessions (docs/M7 第 6 节) ----

// NewSession is what starting a session asks for.
type NewSession struct {
	ProfileID  int64  `json:"profile_id"`
	Cwd        string `json:"cwd"`
	Cols, Rows int
	Mode       string `json:"mode"`           // new (default), continue, pick, resume: see startCommand
	Conv       string `json:"resume_session"` // resume: the conversation's id
	Resume     bool   `json:"resume"`         // older pages: pick, or resume when an id is given
}

// CreateSession starts a session on the profile's node for the acting user.
// Besides a fresh start, it can continue the folder's last conversation, show
// the CLI's own picker, or reopen one conversation (docs/M10 第 3 节).
func (h *Hub) CreateSession(id *auth.Identity, in NewSession) (SessionRow, error) {
	p, err := h.reg.profileFor(id, in.ProfileID)
	if err != nil {
		return SessionRow{}, err
	}
	mode, conv := in.Mode, in.Conv
	if mode == "" && in.Resume {
		mode = StartPick
		if conv != "" {
			mode = StartResume
		}
	}
	if mode != StartResume {
		conv = ""
	}
	if p.Command, p.Args, err = startCommand(p, mode, conv); err != nil {
		return SessionRow{}, err
	}
	cwd, cols, rows := in.Cwd, in.Cols, in.Rows
	n := h.nodeConn(p.NodeID)
	if n == nil {
		return SessionRow{}, ErrNodeOffline
	}
	if cwd == "" {
		cwd = p.DefaultCwd
	}
	if cols == 0 || rows == 0 {
		cols, rows = 120, 32
	}
	if proto.CheckSize(cols, rows) != nil {
		return SessionRow{}, ErrBadRequest
	}
	sid := proto.NewSID()
	now := time.Now().Unix()
	h.starting.Lock() // two presses at once: only one gets the folder
	if h.reg.countRunning("owner_id", id.User.ID) >= h.limits.PerUser || h.reg.countRunning("node_id", p.NodeID) >= h.limits.PerNode {
		h.starting.Unlock()
		return SessionRow{}, ErrBusy
	}
	if err := h.folderFree(id, p, cwd); err != nil {
		h.starting.Unlock()
		return SessionRow{}, err
	}
	_, err = h.reg.db.Exec(`INSERT INTO sessions(sid,node_id,profile_id,profile_name,owner_id,cwd,created_at,conv_id,kind,end_reason) VALUES (?,?,?,?,?,?,?,?,?,'starting')`,
		sid.String(), p.NodeID, p.ID, p.Name, id.User.ID, cwd, now, conv, p.Kind)
	if err == nil {
		n.mu.Lock()
		if n.creating == nil {
			n.creating = map[proto.SID]bool{}
		}
		n.creating[sid] = true
		n.mu.Unlock()
	}
	h.starting.Unlock()
	if err != nil {
		return SessionRow{}, err
	}
	// The node gets a snapshot, never a reference into this database.
	snap := &proto.Profile{Name: p.Name, Kind: p.Kind, Mode: p.Mode, ShellPath: p.ShellPath, ShellArgs: p.ShellArgs,
		Command: p.Command, Args: p.Args, Env: p.Env, IdleTimeout: p.IdleTimeout}
	reply, err := n.request(&proto.Msg{T: proto.MsgCreate, SID: &sid, Owner: id.User.Username, Cwd: cwd, Cols: cols, Rows: rows, Profile: snap}, 15*time.Second)
	if err != nil {
		if reply != nil && reply.T == proto.MsgErr {
			h.reg.db.Exec(`UPDATE sessions SET ended_at=?, end_reason='start_failed' WHERE sid=? AND end_reason IN ('starting','start_unknown')`, time.Now().Unix(), sid.String())
		} else {
			// Keep the quota/folder reservation until the node resolves it.
			h.reg.db.Exec(`UPDATE sessions SET end_reason='start_unknown' WHERE sid=? AND end_reason='starting'`, sid.String())
		}
		h.web.S.Audit(&id.User, id.IP, "session_create", p.Name, "failed", errCode(err))
		return SessionRow{}, err // the node's own code and text reach the browser unchanged
	}
	h.reg.db.Exec(`UPDATE sessions SET end_reason='' WHERE sid=? AND end_reason IN ('starting','start_unknown') AND ended_at IS NULL`, sid.String())
	h.ensureRoute(sid, p.NodeID, cols, rows)
	h.web.S.Audit(&id.User, id.IP, "session_create", sid.String(), "ok", p.Name)
	h.broadcastSessions()
	return h.reg.session(sid.String())
}

// folderFree: one AI CLI of a kind per folder and machine, whoever started it
// and under whichever profile: two would edit the same files, and carry on
// the same conversations. Claude Code and Codex do not count against each
// other; a plain shell is never limited.
func (h *Hub) folderFree(id *auth.Identity, p Profile, cwd string) error {
	name := map[string]string{"claude": "Claude Code", "codex": "Codex"}[p.Kind]
	if name == "" {
		return nil
	}
	// the kind each was started as, not its profile's now (deleted or changed since)
	rows, err := h.reg.db.Query(`SELECT s.cwd, s.owner_id, COALESCE(u.username,'') FROM sessions s
		LEFT JOIN users u ON u.id=s.owner_id
		WHERE s.ended_at IS NULL AND s.node_id=? AND s.kind=?`, p.NodeID, p.Kind)
	if err != nil {
		return err
	}
	defer rows.Close()
	k := normFolder(cwd)
	for rows.Next() {
		var c, who string
		var owner int64
		rows.Scan(&c, &owner, &who)
		if normFolder(c) == k {
			// whose: yours, or the name for an administrator; who works where
			// is not told to everyone else
			switch {
			case owner == id.User.ID:
				who = "你"
			case !id.User.IsAdmin() || who == "":
				who = "另一位用户"
			}
			return &auth.Error{Code: proto.ErrFolderBusy, Status: 409,
				Msg: fmt.Sprintf("%s 正在这个文件夹里使用 %s：同一个文件夹同时只能开一个 %s 会话", who, name, name)}
		}
	}
	return rows.Err()
}

func errCode(err error) string {
	var e *auth.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return "internal"
}

// CloseSession asks the node to end a session.
func (h *Hub) CloseSession(id *auth.Identity, sidText string, force bool) error {
	row, err := h.reg.session(sidText)
	if err != nil {
		return err
	}
	if !h.reg.CanAccessSession(id, row) {
		return ErrNotFound // do not reveal that it exists
	}
	// ending someone else's shell is a sensitive act, like mounting it (复核第四轮 11)
	if row.OwnerID != id.User.ID && !id.Reverified {
		return auth.ErrReverifyRequired
	}
	sid, _ := proto.ParseSID(sidText)
	n := h.nodeConn(row.NodeID)
	if n == nil {
		return ErrNodeOffline
	}
	mode := proto.CloseGraceful
	if force {
		mode = proto.CloseForce
	}
	_, err = n.request(&proto.Msg{T: proto.MsgClose, SID: &sid, Mode: mode, Reason: "user"}, 10*time.Second)
	h.web.S.Audit(&id.User, id.IP, "session_close", sidText, "ok", mode)
	return err
}

// ---- page events (docs/M1 5.3) ----

type eventConn struct {
	ws *wsConn
	id *auth.Identity
}

func (h *Hub) broadcast(m *proto.Msg, allow func(*auth.Identity) bool) {
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for e := range h.events {
		if allow == nil || allow(e.id) {
			e.ws.send(websocket.TextMessage, b)
		}
	}
}

func (h *Hub) broadcastSessions() { h.broadcast(&proto.Msg{T: proto.MsgSessionsChanged}, nil) }

// A working Claude Code changes its title several times a second, and every
// sessions_changed makes each browser read three lists again. With answers
// slower than the frames the page threw each one away as stale and never saw
// the session working, so its status dot stayed off (状态点复核 3). Title
// changes are told at most every titleEvery; the newest title is in the
// database before the one that is told, so the last change is never missed.
const titleEvery = 300 * time.Millisecond

// coalescer calls fire once, a period of every after the first poke, for
// all the pokes that came in the meantime.
type coalescer struct {
	every time.Duration
	fire  func()
	mu    sync.Mutex
	timer *time.Timer
}

func (c *coalescer) poke() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.timer != nil {
		return
	}
	c.timer = time.AfterFunc(c.every, func() {
		c.mu.Lock()
		c.timer = nil
		c.mu.Unlock()
		c.fire()
	})
}

func (h *Hub) broadcastNode(nodeID int64, online bool) {
	var name string
	h.reg.db.QueryRow(`SELECT name FROM nodes WHERE id=?`, nodeID).Scan(&name)
	h.broadcast(&proto.Msg{T: proto.MsgNodeStatus, Node: name, Online: &online},
		func(id *auth.Identity) bool { return h.reg.CanUseNode(id, nodeID) })
}

// ServeEvents handles /ws/events.
func (h *Hub) ServeEvents(w http.ResponseWriter, r *http.Request) {
	id, err := h.web.AuthenticateWS(r)
	if err != nil {
		h.web.Fail(w, r, err)
		return
	}
	c, err := h.up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	e := &eventConn{ws: newWSConn(c, 1<<20, 64<<10), id: id}
	h.mu.Lock()
	h.events[e] = struct{}{}
	h.mu.Unlock()
	stop := h.watchLogin(r, e.ws, func(now *auth.Identity) bool { // keep the identity fresh for the filters
		h.mu.Lock()
		e.id = now
		h.mu.Unlock()
		return true
	})
	defer func() {
		close(stop)
		h.mu.Lock()
		delete(h.events, e)
		h.mu.Unlock()
		e.ws.close()
	}()
	for {
		_, data, err := c.ReadMessage()
		if err != nil {
			return
		}
		e.ws.touch()
		if m, err := proto.DecodeMsg(data); err == nil && m.T == proto.MsgPing {
			b, _ := json.Marshal(&proto.Msg{T: proto.MsgPong})
			e.ws.send(websocket.TextMessage, b)
		}
	}
}

// watchLogin re-checks the login session behind an open WebSocket, so that a
// revoked or expired login closes its terminals within seconds (docs/M6 第 5 节).
// still, when given, re-evaluates the connection's authorization against the
// fresh identity (a demoted administrator loses someone else's terminal at
// once, 安全复核 M6).
func (h *Hub) watchLogin(r *http.Request, ws *wsConn, still func(*auth.Identity) bool) chan struct{} {
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ws.done:
				return
			case <-t.C:
				id, err := h.web.AuthenticateWS(r)
				if err == nil && still != nil && !still(id) {
					err = auth.ErrForbidden
				}
				if err != nil {
					b, _ := json.Marshal(&proto.Msg{T: proto.MsgAuthExpired})
					ws.send(websocket.TextMessage, b)
					time.Sleep(100 * time.Millisecond)
					ws.close()
					return
				}
			}
		}
	}()
	return stop
}
