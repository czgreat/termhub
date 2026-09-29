//go:build windows

// Package agent is the networked half of termhub-agent (docs/M5): it connects
// outwards to the Hub, connects to the local session host, and carries
// messages and terminal data between the two. It holds no session state of its
// own, so it can be killed and replaced at any time (docs/M3).
package agent

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/gorilla/websocket"
	"golang.org/x/sys/windows/registry"

	"termhub/internal/agent/fs"
	"termhub/internal/proto"
)

// Version of this agent build.
var Version = "dev"

// Config is what an enrolled agent knows (docs/M5 第 3 节).
type Config struct {
	HubURL   string      // https://host:port
	Token    string      // node token
	NodeName string      // informational; the Hub knows the node by its token
	PipeName string      // session host pipe
	TLS      *tls.Config // nil: system roots. Pinning is configured by the caller.
	Log      *slog.Logger
	// StartHost is called when the host pipe cannot be reached. nil: just retry.
	StartHost func() error
	Files     fs.Options // per-node file settings (docs/M4 第 3 节)
	DataDir   string     // pasted files live under here; default: <install dir>\agent
}

// Agent bridges one Hub link and one host link.
type Agent struct {
	cfg Config

	mu        sync.Mutex
	host      net.Conn
	hostOut   chan []byte // encoded pipe frames to the host
	hub       *hubLink
	nextHost  uint64
	pending   map[uint64]uint64 // host request id -> Hub request id
	forwarded map[proto.SID]bool
	hostVer   string
}

type hubLink struct {
	ws     *websocket.Conn
	ctrl   chan []byte // text messages
	replay chan []byte // replay frames and replay_end, in order
	mux    *proto.Mux  // live output, fair across sessions
	done   chan struct{}
	once   sync.Once
}

func (l *hubLink) close() { l.once.Do(func() { close(l.done); l.ws.Close() }) }

func New(cfg Config) *Agent {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.DataDir == "" {
		cfg.DataDir = filepath.Join(Dir(), "agent")
	}
	return &Agent{cfg: cfg, hostOut: make(chan []byte, 4096), pending: map[uint64]uint64{}, forwarded: map[proto.SID]bool{}}
}

// Fingerprint identifies this Windows installation without revealing the
// underlying identifier (docs/总体设计 8.3).
func Fingerprint() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Cryptography`, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return ""
	}
	defer k.Close()
	guid, _, err := k.GetStringValue("MachineGuid")
	if err != nil {
		return ""
	}
	sum := sha256.Sum256([]byte("termhub:" + guid))
	return hex.EncodeToString(sum[:])
}

// Run keeps both links up until ctx ends. It never gives up on the Hub.
func (a *Agent) Run(ctx context.Context) {
	go a.hostLoop(ctx)
	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := a.hubSession(ctx)
		if ctx.Err() != nil {
			return
		}
		wait := backoff + time.Duration(rand.Int64N(int64(backoff/2)+1))
		var unauthorized *unauthorizedError
		if errors.As(err, &unauthorized) {
			wait = 5 * time.Minute // token invalid or node disabled: do not hammer the Hub
		}
		a.cfg.Log.Warn("hub link down", "err", err, "retry_in", wait.Round(time.Second))
		if time.Since(started) > time.Minute {
			backoff = time.Second
		} else if backoff < 30*time.Second {
			backoff *= 2
		}
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
	}
}

// listRequest marks the agent's own "list your sessions" request to the host.
const listRequest = ^uint64(0)

type unauthorizedError struct{}

func (*unauthorizedError) Error() string { return "hub refused the node token" }

// ---- host link (docs/M1 链路 C) ----

func (a *Agent) hostLoop(ctx context.Context) {
	for ctx.Err() == nil {
		d := 2 * time.Second
		conn, err := winio.DialPipe(a.cfg.PipeName, &d)
		if err != nil {
			if a.cfg.StartHost != nil {
				if e := a.cfg.StartHost(); e != nil {
					a.cfg.Log.Error("cannot start the session host", "err", e)
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		a.serveHost(ctx, conn)
	}
}

func (a *Agent) toHost(frame []byte) {
	select {
	case a.hostOut <- frame:
	default: // the host is local and fast; a full queue means it is gone
	}
}

func (a *Agent) hostMsg(m *proto.Msg) {
	if b, err := proto.EncodeMsg(m); err == nil {
		if f, err := proto.AppendFrame(nil, proto.Frame{Type: proto.TypeControl, Data: b}); err == nil {
			a.toHost(f)
		}
	}
}

func (a *Agent) serveHost(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	for len(a.hostOut) > 0 { // frames meant for a previous host connection
		<-a.hostOut
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case f := <-a.hostOut:
				if proto.WritePipeFrame(conn, f) != nil {
					conn.Close()
					return
				}
			case <-stop:
				return
			case <-ctx.Done():
				conn.Close()
				return
			}
		}
	}()
	a.hostMsg(&proto.Msg{T: proto.MsgHello, ID: a.newHostID(0), Proto: proto.Current.String(), AgentVer: Version})
	a.mu.Lock()
	a.host = conn
	up := a.hub != nil
	a.mu.Unlock()
	a.hostMsg(&proto.Msg{T: proto.MsgLink, Up: &up})
	defer func() {
		a.mu.Lock()
		a.host = nil
		a.forwarded = map[proto.SID]bool{}
		a.mu.Unlock()
	}()
	for {
		raw, err := proto.ReadPipeFrame(conn)
		if err != nil {
			return
		}
		f, err := proto.ParseFrame(raw)
		if err != nil {
			return
		}
		a.fromHost(f, raw)
	}
}

func (a *Agent) newHostID(hubID uint64) uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.nextHost++
	if hubID != 0 {
		a.pending[a.nextHost] = hubID
	}
	return a.nextHost
}

// fromHost relays what the host says to the Hub. Frames of links B and C share
// one format, so terminal data passes through without re-encoding.
func (a *Agent) fromHost(f proto.Frame, raw []byte) {
	a.mu.Lock()
	hub := a.hub
	a.mu.Unlock()
	switch f.Type {
	case proto.TypeOutput:
		if hub != nil {
			hub.mux.Push(f.SID, f.Offset, f.Data)
		}
	case proto.TypeReplay:
		if hub != nil {
			select {
			case hub.replay <- append([]byte{websocket.BinaryMessage}, raw...):
			case <-hub.done:
			}
		}
	case proto.TypeControl:
		m, err := proto.DecodeMsg(f.Data)
		if err != nil {
			return
		}
		if m.T == proto.MsgWelcome {
			a.mu.Lock()
			a.hostVer = m.HostVer
			a.mu.Unlock()
			return
		}
		// Pasted files belong to a session and go when it goes (docs/M4 第 7 节).
		if m.T == proto.MsgSessionExited && m.SID != nil {
			go fs.RemovePaste(a.cfg.DataDir, m.SID.String())
		}
		if m.List != nil || m.T == proto.MsgSessions {
			alive := map[string]bool{}
			for _, s := range m.List {
				alive[s.SID.String()] = true
			}
			go fs.SweepPaste(a.cfg.DataDir, func(sid string) bool { return alive[sid] })
		}
		if m.Re != 0 && m.T != proto.MsgReplayBegin {
			a.mu.Lock()
			hubID, ok := a.pending[m.Re]
			delete(a.pending, m.Re)
			a.mu.Unlock()
			if !ok {
				return // an answer to one of the agent's own requests
			}
			if hubID == listRequest {
				// The agent asked for the list on the Hub's behalf after a
				// reconnect; the Hub expects it as a sessions notification.
				m = &proto.Msg{T: proto.MsgSessions, List: m.List}
			} else {
				m.Re = hubID
			}
		} else if m.T == proto.MsgReplayBegin {
			m.Re = 0
		}
		if hub == nil {
			return
		}
		b, err := proto.EncodeMsg(m)
		if err != nil {
			return
		}
		if m.T == proto.MsgReplayEnd || m.T == proto.MsgReplayBegin {
			// Must stay in order with the replay frames around it.
			select {
			case hub.replay <- append([]byte{websocket.TextMessage}, b...):
			case <-hub.done:
			}
			return
		}
		select {
		case hub.ctrl <- b:
		case <-hub.done:
		default:
			hub.close() // the Hub link cannot even carry control traffic
		}
	}
}

// ---- Hub link (docs/M1 链路 B) ----

func (a *Agent) hubSession(ctx context.Context) error {
	// Being online must mean being usable: give the session host a moment to
	// come up (it may just have been started) before announcing this node.
	for end := time.Now().Add(15 * time.Second); time.Now().Before(end) && ctx.Err() == nil; time.Sleep(100 * time.Millisecond) {
		a.mu.Lock()
		ready := a.host != nil
		a.mu.Unlock()
		if ready {
			break
		}
	}
	url := "wss" + strings.TrimPrefix(a.cfg.HubURL, "https") + "/node/link"
	d := websocket.Dialer{TLSClientConfig: a.cfg.TLS, HandshakeTimeout: 15 * time.Second,
		Proxy: nil} // never through a proxy: not the environment's, not the system's (docs/M5 第 5 节)
	ws, resp, err := d.DialContext(ctx, url, http.Header{"X-TH-Node-Token": {a.cfg.Token}})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			return &unauthorizedError{}
		}
		return err
	}
	l := &hubLink{ws: ws, ctrl: make(chan []byte, 2048), replay: make(chan []byte, 16), mux: proto.NewMux(proto.SessionQueue), done: make(chan struct{})}
	defer l.close()
	go func() { // stopping the agent must drop the link at once, not at the next read timeout
		select {
		case <-ctx.Done():
			l.close()
		case <-l.done:
		}
	}()
	ws.SetReadLimit(proto.MaxFrame + 64)

	host, _ := os.Hostname()
	if a.cfg.NodeName != "" {
		host = a.cfg.NodeName
	}
	a.mu.Lock()
	hostVer := a.hostVer
	a.mu.Unlock()
	hello, _ := proto.EncodeMsg(&proto.Msg{T: proto.MsgHello, ID: 1, Proto: proto.Current.String(), AgentVer: Version,
		HostVer: hostVer, Name: host, Fingerprint: Fingerprint()})
	if err := ws.WriteMessage(websocket.TextMessage, hello); err != nil {
		return err
	}
	ws.SetReadDeadline(time.Now().Add(20 * time.Second))
	_, raw, err := ws.ReadMessage()
	if err != nil {
		return err
	}
	if w, err := proto.DecodeMsg(raw); err != nil || w.T != proto.MsgWelcome {
		return errors.New("hub did not welcome us: " + string(raw))
	}
	go a.hubWriter(l)

	a.mu.Lock()
	a.hub = l
	a.mu.Unlock()
	up := true
	a.hostMsg(&proto.Msg{T: proto.MsgLink, Up: &up})
	a.hostMsg(&proto.Msg{T: proto.MsgSessions, ID: a.newHostID(listRequest)}) // have the host list its sessions for the Hub
	defer func() {
		a.mu.Lock()
		a.hub = nil
		sids := make([]proto.SID, 0, len(a.forwarded))
		for sid := range a.forwarded {
			sids = append(sids, sid)
		}
		a.forwarded = map[proto.SID]bool{}
		a.mu.Unlock()
		down, off := false, false
		a.hostMsg(&proto.Msg{T: proto.MsgLink, Up: &down})
		for i := range sids { // nobody can watch through a dead link
			a.hostMsg(&proto.Msg{T: proto.MsgForward, SID: &sids[i], On: &off})
		}
	}()

	ws.SetPongHandler(func(string) error { return ws.SetReadDeadline(time.Now().Add(60 * time.Second)) })
	for {
		ws.SetReadDeadline(time.Now().Add(60 * time.Second))
		kind, data, err := ws.ReadMessage()
		if err != nil {
			return err
		}
		if kind == websocket.BinaryMessage { // terminal input: same frame format, pass through
			if f, err := proto.ParseFrame(data); err == nil && f.Type == proto.TypeInput {
				a.toHost(append([]byte(nil), data...))
			}
			continue
		}
		m, err := proto.DecodeMsg(data)
		if err != nil {
			return err
		}
		a.fromHub(l, m)
	}
}

func (a *Agent) fromHub(l *hubLink, m *proto.Msg) {
	switch m.T {
	case proto.MsgCreate, proto.MsgForward, proto.MsgReplay, proto.MsgResize, proto.MsgRedraw, proto.MsgClose:
		if m.T == proto.MsgForward && m.SID != nil && m.On != nil {
			a.mu.Lock()
			if *m.On {
				a.forwarded[*m.SID] = true
			} else {
				delete(a.forwarded, *m.SID)
				l.mux.Drop(*m.SID)
			}
			a.mu.Unlock()
		}
		a.mu.Lock()
		connected := a.host != nil
		a.mu.Unlock()
		if !connected {
			if m.IsRequest() {
				if b, err := proto.EncodeMsg(m.Fail(proto.ErrInternal, "the session host is not running on this node")); err == nil {
					select { // a writer gone with the link must not hold up this reader
					case l.ctrl <- b:
					case <-l.done:
					}
				}
			}
			return
		}
		if m.IsRequest() {
			m.ID = a.newHostID(m.ID)
		}
		a.hostMsg(m)
	case proto.MsgPing:
		if b, err := proto.EncodeMsg(&proto.Msg{T: proto.MsgPong, Re: m.ID}); err == nil {
			select { // a writer gone with the link must not hold up this reader
			case l.ctrl <- b:
			case <-l.done:
			}
		}
	case proto.MsgTransferOpen:
		go a.transfer(m)
	case proto.MsgFsList, proto.MsgFsStat, proto.MsgFsMkdir, proto.MsgClipboard, proto.MsgHistoryList, proto.MsgHistoryRead, proto.MsgUsage:
		go func() { // disk access must never hold up the link's reader
			reply := safeFileRequest(m, a.fileRequest)
			if b, err := proto.EncodeMsg(reply); err == nil {
				select {
				case l.ctrl <- b:
				case <-l.done:
				}
			}
		}()
	default:
		if m.IsRequest() { // history_list, upgrade, transfers: later modules
			if b, err := proto.EncodeMsg(m.Fail(proto.ErrUnsupported, m.T)); err == nil {
				select { // a writer gone with the link must not hold up this reader
				case l.ctrl <- b:
				case <-l.done:
				}
			}
		}
	}
}

// hubWriter is the link's only writer: control first, then replays in order,
// then live output round-robin across sessions (docs/M1 第 6 节).
func (a *Agent) hubWriter(l *hubLink) {
	write := func(kind int, data []byte) bool {
		l.ws.SetWriteDeadline(time.Now().Add(15 * time.Second))
		if l.ws.WriteMessage(kind, data) != nil {
			l.close()
			return false
		}
		return true
	}
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case b := <-l.ctrl:
			if !write(websocket.TextMessage, b) {
				return
			}
			continue
		default:
		}
		select {
		case b := <-l.replay:
			if !write(int(b[0]), b[1:]) {
				return
			}
			continue
		default:
		}
		if it, ok := l.mux.Next(); ok {
			if it.Gap {
				sid := it.SID
				if b, err := proto.EncodeMsg(&proto.Msg{T: proto.MsgGap, SID: &sid, Next: it.Offset}); err == nil && !write(websocket.TextMessage, b) {
					return
				}
				continue
			}
			f, err := proto.AppendFrame(nil, proto.Frame{Type: proto.TypeOutput, SID: it.SID, Offset: it.Offset, Data: it.Data})
			if err == nil && !write(websocket.BinaryMessage, f) {
				return
			}
			continue
		}
		select {
		case b := <-l.ctrl:
			if !write(websocket.TextMessage, b) {
				return
			}
		case b := <-l.replay:
			if !write(int(b[0]), b[1:]) {
				return
			}
		case <-l.mux.Wake():
		case <-ping.C:
			if !write(websocket.PingMessage, nil) {
				return
			}
		case <-l.done:
			return
		}
	}
}
