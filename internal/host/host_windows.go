//go:build windows

// Package host implements docs/M3-会话宿主.md: the long-lived process that
// owns every terminal session of one Windows user and serves the agent over a
// named pipe, so that agent upgrades and crashes never end a session.
package host

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"

	"termhub/internal/proto"
	"termhub/internal/session"
)

// Version of this host build, reported in welcome.
var Version = "dev"

// Config holds what differs between production and tests.
type Config struct {
	PipeName    string        // default: \\.\pipe\termhub-host-<user SID>
	StateDir    string        // default: %LOCALAPPDATA%\termhub\host
	MaxSessions int           // default 64
	IdleExit    time.Duration // exit when no sessions and no agent for this long; default 10m
	Tick        time.Duration // idle accounting granularity; default 1s
	Log         *slog.Logger
}

// DefaultPipeName returns the per-user pipe name.
func DefaultPipeName() (string, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return `\\.\pipe\termhub-host-` + u.User.Sid.String(), nil
}

type entry struct {
	s        *session.Session
	sid      proto.SID
	profile  string
	kind     string
	owner    string
	created  time.Time
	timeout  time.Duration // 0 = never
	idle     time.Duration // accumulated while unwatched and the link is up
	stopFwd  func()        // non-nil while forwarding
	deadline time.Time
}

// Host owns the sessions.
type Host struct {
	cfg      Config
	ln       net.Listener
	mu       sync.Mutex
	sessions map[proto.SID]*entry
	client   *client
	linkUp   bool
	lastBusy time.Time
	exits    []*proto.Msg // recent session_exited notices for an agent that was away
	stop     chan struct{}
	stopOnce sync.Once
	stopped  chan struct{}
}

// New creates the pipe (failing if another host already serves it), cleans up
// what a crashed predecessor left behind, and starts serving.
func New(cfg Config) (*Host, error) {
	if cfg.PipeName == "" {
		name, err := DefaultPipeName()
		if err != nil {
			return nil, err
		}
		cfg.PipeName = name
	}
	if cfg.StateDir == "" {
		cfg.StateDir = filepath.Join(os.Getenv("LOCALAPPDATA"), "termhub", "host")
	}
	if cfg.MaxSessions == 0 {
		cfg.MaxSessions = 64
	}
	if cfg.IdleExit == 0 {
		cfg.IdleExit = 10 * time.Minute
	}
	if cfg.Tick == 0 {
		cfg.Tick = time.Second
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	// Only this user. go-winio creates the first instance with
	// FILE_FLAG_FIRST_PIPE_INSTANCE and rejects remote clients.
	ln, err := winio.ListenPipe(cfg.PipeName, &winio.PipeConfig{
		SecurityDescriptor: "D:P(A;;GA;;;" + u.User.Sid.String() + ")",
		InputBufferSize:    1 << 20,
		OutputBufferSize:   1 << 20,
	})
	if err != nil {
		return nil, fmt.Errorf("host: pipe %s: %w", cfg.PipeName, err)
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		ln.Close()
		return nil, err
	}
	h := &Host{cfg: cfg, ln: ln, sessions: map[proto.SID]*entry{}, lastBusy: time.Now(),
		stop: make(chan struct{}), stopped: make(chan struct{})}
	h.reapLeftovers()
	go h.acceptLoop()
	go h.tickLoop()
	return h, nil
}

// Wait blocks until the host has shut down.
func (h *Host) Wait() { <-h.stopped }

// Shutdown ends every session and stops serving.
func (h *Host) Shutdown() {
	h.stopOnce.Do(func() {
		close(h.stop)
		h.ln.Close()
		h.mu.Lock()
		list := make([]*entry, 0, len(h.sessions))
		for _, e := range h.sessions {
			list = append(list, e)
		}
		c := h.client
		h.mu.Unlock()
		for _, e := range list {
			e.s.Close(session.Force, session.ReasonHostShutdown)
		}
		deadline := time.After(15 * time.Second)
		for _, e := range list {
			select {
			case <-e.s.Done():
			case <-deadline:
			}
		}
		if c != nil {
			c.close()
		}
		h.writeState()
		close(h.stopped)
	})
}

func (h *Host) acceptLoop() {
	for {
		conn, err := h.ln.Accept()
		if err != nil {
			return
		}
		c := newClient(h, conn)
		h.mu.Lock()
		old := h.client
		h.client, h.linkUp = c, false
		h.mu.Unlock()
		if old != nil { // a newer agent takes over (docs/M1 2.3)
			old.close()
		}
		go c.run()
	}
}

// tickLoop does idle accounting, state persistence and self-exit.
func (h *Host) tickLoop() {
	t := time.NewTicker(h.cfg.Tick)
	defer t.Stop()
	lastState := time.Now()
	for {
		select {
		case <-h.stop:
			return
		case <-t.C:
		}
		h.mu.Lock()
		counting := h.client != nil && h.linkUp
		var expire []*entry
		for _, e := range h.sessions {
			// Idle time only runs while the agent is connected and reports a
			// healthy Hub link, so outages never kill sessions (docs/M3 第 6 节).
			if e.timeout == 0 || e.stopFwd != nil || !counting {
				e.deadline = time.Time{}
				continue
			}
			e.idle += h.cfg.Tick
			e.deadline = time.Now().Add(e.timeout - e.idle)
			if e.idle >= e.timeout {
				expire = append(expire, e)
			}
		}
		if len(h.sessions) > 0 || h.client != nil {
			h.lastBusy = time.Now()
		}
		quit := time.Since(h.lastBusy) > h.cfg.IdleExit
		h.mu.Unlock()
		for _, e := range expire {
			e.s.Close(session.Graceful, session.ReasonIdle)
		}
		if time.Since(lastState) > 5*h.cfg.Tick {
			h.writeState()
			lastState = time.Now()
		}
		if quit {
			h.cfg.Log.Info("no sessions and no agent, exiting")
			go h.Shutdown()
			return
		}
	}
}

func (h *Host) list() []proto.SessionInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]proto.SessionInfo, 0, len(h.sessions))
	for _, e := range h.sessions {
		i := e.s.Info()
		si := proto.SessionInfo{SID: e.sid, Profile: e.profile, Kind: e.kind, Owner: e.owner, Cwd: i.Cwd, State: i.State,
			Cols: i.Cols, Rows: i.Rows, Base: i.Base, End: i.End, Created: e.created.Unix(),
			Title: i.Title, JobEscapeRisk: i.JobEscapeRisk}
		if !e.deadline.IsZero() {
			si.IdleDeadline = e.deadline.Unix()
		}
		if i.CodePageUnset {
			si.Flags = append(si.Flags, "codepage_not_set")
		}
		out = append(out, si)
	}
	return out
}

func (h *Host) create(m *proto.Msg) error {
	if m.SID == nil || m.Profile == nil {
		return &session.Error{Code: proto.ErrBadRequest, Msg: "create needs sid and profile"}
	}
	h.mu.Lock()
	_, dup := h.sessions[*m.SID]
	full := len(h.sessions) >= h.cfg.MaxSessions
	h.mu.Unlock()
	if dup {
		return &session.Error{Code: proto.ErrBadRequest, Msg: "session id already exists"}
	}
	if full {
		return &session.Error{Code: proto.ErrBusy, Msg: "session limit reached"}
	}
	p := m.Profile
	s, err := session.Start(session.Spec{ID: *m.SID, Mode: session.LaunchMode(p.Mode), ShellPath: p.ShellPath,
		ShellArgs: p.ShellArgs, Command: p.Command, Args: p.Args, Env: p.Env, Cwd: m.Cwd,
		Cols: m.Cols, Rows: m.Rows, BufferBytes: p.BufferBytes})
	if err != nil {
		return err
	}
	e := &entry{s: s, sid: *m.SID, profile: p.Name, kind: p.Kind, owner: m.Owner, created: time.Now(),
		timeout: time.Duration(p.IdleTimeout) * time.Second}
	h.mu.Lock()
	h.sessions[e.sid] = e
	h.mu.Unlock()
	s.OnTitle(func(t string) { h.notify(&proto.Msg{T: proto.MsgTitle, SID: &e.sid, Title: t}) })
	go h.reap(e)
	return nil
}

// reap waits for a session to end. A panic inside one session's goroutines is
// that session's problem only; nothing here can take the host down.
func (h *Host) reap(e *entry) {
	info := <-e.s.Done()
	h.mu.Lock()
	if e.stopFwd != nil {
		e.stopFwd()
		e.stopFwd = nil
	}
	delete(h.sessions, e.sid)
	code := info.ExitCode
	msg := &proto.Msg{T: proto.MsgSessionExited, SID: &e.sid, Reason: info.Reason, ExitCode: &code, Leftover: info.Leftover}
	h.exits = append(h.exits, msg)
	if len(h.exits) > 256 {
		h.exits = h.exits[len(h.exits)-256:]
	}
	c := h.client
	h.mu.Unlock()
	if c != nil {
		c.mux.Drop(e.sid)
		c.send(msg)
		c.send(&proto.Msg{T: proto.MsgSessions, List: h.list()})
	}
	h.writeState()
}

func (h *Host) notify(m *proto.Msg) {
	h.mu.Lock()
	c := h.client
	h.mu.Unlock()
	if c != nil {
		c.send(m)
	}
}

func (h *Host) get(sid *proto.SID) (*entry, error) {
	if sid == nil {
		return nil, &session.Error{Code: proto.ErrBadRequest, Msg: "missing sid"}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if e := h.sessions[*sid]; e != nil {
		return e, nil
	}
	return nil, &session.Error{Code: proto.ErrNotFound, Msg: "no such session"}
}

// ---- state on disk (docs/M3 第 7 节) ----

type stateFile struct {
	Sessions []stateSession `json:"sessions"`
}

type stateSession struct {
	SID   string           `json:"sid"`
	Procs []session.ProcID `json:"procs"`
}

func (h *Host) statePath() string { return filepath.Join(h.cfg.StateDir, "state.json") }

// writeState records, per session, the processes that would have to be killed
// if this host died. Nothing else: no terminal content, no environment, no
// command lines.
func (h *Host) writeState() {
	h.mu.Lock()
	var st stateFile
	for _, e := range h.sessions {
		st.Sessions = append(st.Sessions, stateSession{SID: e.sid.String(), Procs: e.s.Descendants()})
	}
	h.mu.Unlock()
	b, _ := json.Marshal(st)
	tmp := h.statePath() + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, h.statePath())
	}
}

// reapLeftovers kills what a previous host that died left running. Processes
// inside that host's Jobs died with it; this catches the ones that had
// escaped a Job. Identity is pid plus creation time, never pid alone.
func (h *Host) reapLeftovers() {
	b, err := os.ReadFile(h.statePath())
	if err != nil {
		return
	}
	var st stateFile
	if json.Unmarshal(b, &st) != nil {
		return
	}
	for _, s := range st.Sessions {
		for _, p := range s.Procs {
			if session.KillIfSame(p) {
				h.cfg.Log.Warn("killed leftover process of a previous host", "pid", p.PID, "name", p.Name, "session", s.SID)
			}
		}
	}
	os.Remove(h.statePath())
}

var errClosed = errors.New("host: client closed")
