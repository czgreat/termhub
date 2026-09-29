//go:build windows

package session

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/windows"
)

// Version is reported to programs as TERM_PROGRAM_VERSION. Set at build time.
var Version = "dev"

const (
	readChunk = 32 << 10
	// A paste arrives as a burst of 64 KiB frames, far faster than a console
	// consumes input. The queue must hold a whole large paste; beyond it input
	// is refused rather than blocking the reader that serves every session.
	inputQueueSize = 8 << 20
	inputPiece     = 4 << 10
	subQueueLen    = 256
	treeInterval   = 2 * time.Second
)

// Info is a snapshot of a session's state.
type Info struct {
	ID            [16]byte
	PID           uint32
	State         string // starting, running, closing, exited
	Cols, Rows    int
	Base, End     uint64
	Title         string
	Cwd           string
	JobEscapeRisk bool
	CodePageUnset bool
	Exe           string
}

type subscriber struct {
	ch     chan Chunk
	gapped bool
}

// Session is one running pseudo console and everything in it (docs/M2).
type Session struct {
	spec Spec
	pty  *conPTY
	sp   *spawned
	exe  string
	buf  *Buffer
	tree *procTree

	mu     sync.Mutex
	state  string
	cols   int
	rows   int
	title  string
	subs   map[int]*subscriber
	nextID int
	cpBad  bool

	inq       chan []byte
	inPending atomic.Int64
	stalled   atomic.Bool

	closing   sync.Once
	readDone  chan struct{}
	procDone  chan struct{}
	exitCode  atomic.Int64
	done      chan ExitInfo
	closeReq  chan closeRequest
	onTitleFn atomic.Pointer[func(string)]
	titleWake chan struct{} // a title change waits for titleLoop
}

type closeRequest struct {
	mode   CloseMode
	reason string
}

// UserEnvironment is the environment a session starts from (see Start; the history list finds the CLI folders the same way):
// the user's own, else this process's.
func UserEnvironment() []string {
	if env := userEnvironment(); env != nil {
		return env
	}
	return os.Environ()
}

// Start launches a session. Every failure leaves nothing behind.
func Start(spec Spec) (*Session, error) {
	if spec.Cols == 0 || spec.Rows == 0 {
		spec.Cols, spec.Rows = 120, 32
	}
	cwd, err := filepath.EvalSymlinks(spec.Cwd)
	if st, e := os.Stat(cwd); spec.Cwd == "" || err != nil || e != nil || !st.IsDir() {
		return nil, errf(CodeCwdNotFound, spec.Cwd)
	}
	env := mergeEnv(UserEnvironment(), spec.Env, Version)
	cl, err := buildCommand(spec, env)
	if err != nil {
		return nil, err
	}
	pty, err := newConPTY(spec.Cols, spec.Rows)
	if err != nil {
		return nil, errf(CodeSpawnFailed, "pseudo console: "+err.Error())
	}
	sp, err := pty.spawn(cl, cwd, env)
	if err != nil {
		pty.releaseChildEnds()
		pty.ClosePTY()
		pty.closePipes()
		return nil, errf(CodeSpawnFailed, cl.exe+": "+err.Error())
	}

	s := &Session{
		spec: spec, pty: pty, sp: sp, exe: cl.exe,
		buf:   NewBuffer(spec.BufferBytes),
		tree:  newProcTree(sp.pid, sp.process),
		state: "running", cols: spec.Cols, rows: spec.Rows,
		subs:      map[int]*subscriber{},
		inq:       make(chan []byte, 1024),
		readDone:  make(chan struct{}),
		procDone:  make(chan struct{}),
		done:      make(chan ExitInfo, 1),
		closeReq:  make(chan closeRequest, 1),
		titleWake: make(chan struct{}, 1),
	}
	s.spec.Cwd = cwd
	s.buf.OnTitle(s.setTitle) // called under the buffer lock: only record, never block
	go s.titleLoop()
	go s.readLoop()
	go s.writeLoop()
	go s.waitProcess()
	go s.supervise()
	if !setUTF8CodePage(sp.pid) {
		s.mu.Lock()
		s.cpBad = true
		s.mu.Unlock()
	}
	return s, nil
}

// OnTitle registers a callback for terminal title changes.
func (s *Session) OnTitle(f func(string)) { s.onTitleFn.Store(&f) }

func (s *Session) setTitle(t string) {
	s.mu.Lock()
	s.title = t
	s.mu.Unlock()
	select {
	case s.titleWake <- struct{}{}:
	default: // titleLoop has a wake-up pending and will read the newest title
	}
}

// titleLoop hands title changes on one at a time, the newest last. One
// goroutine per change could deliver an older spinner frame after Claude
// Code's final ✳ and leave the session looking busy in the browser's status
// dots (状态点复核 2). Frames that come faster than they are sent are skipped.
func (s *Session) titleLoop() {
	for {
		select {
		case <-s.titleWake:
		case <-s.readDone:
			return
		}
		s.mu.Lock()
		t := s.title
		s.mu.Unlock()
		if f := s.onTitleFn.Load(); f != nil {
			(*f)(t)
		}
	}
}

func (s *Session) Info() Info {
	base, end := s.buf.Range()
	s.mu.Lock()
	defer s.mu.Unlock()
	return Info{ID: s.spec.ID, PID: s.sp.pid, State: s.state, Cols: s.cols, Rows: s.rows,
		Base: base, End: end, Title: s.title, Cwd: s.spec.Cwd,
		JobEscapeRisk: s.sp.escapeRisk, CodePageUnset: s.cpBad, Exe: s.exe}
}

// readLoop moves program output into the buffer and on to subscribers. It
// never waits for a subscriber: a full subscriber loses its queue and is told
// so with a Gap chunk (docs/M1 第 6 节).
func (s *Session) readLoop() {
	defer close(s.readDone)
	buf := make([]byte, readChunk)
	for {
		n, err := s.pty.Read(buf)
		if n > 0 {
			s.publish(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// Subscribe delivers live output from now on. Pair it with Replay under the
// caller's own ordering: Replay's End is the first offset Subscribe may carry.
func (s *Session) Subscribe() (<-chan Chunk, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextID
	s.nextID++
	sub := &subscriber{ch: make(chan Chunk, subQueueLen)}
	s.subs[id] = sub
	return sub.ch, func() {
		s.mu.Lock()
		delete(s.subs, id)
		s.mu.Unlock()
	}
}

// Replay returns the catch-up data for a viewer (docs/M2 第 7 节).
func (s *Session) Replay(have *uint64) Replay { return s.buf.Replay(have) }

// Write queues bytes for the program's input. They are passed on unchanged.
func (s *Session) Write(p []byte) error {
	if len(p) == 0 {
		return nil
	}
	s.mu.Lock()
	running := s.state == "running"
	s.mu.Unlock()
	if !running {
		return errf(CodeNotRunning, "session is not running")
	}
	if s.stalled.Load() {
		return errf(CodeInputStalled, "the program is not reading its input")
	}
	if s.inPending.Load()+int64(len(p)) > inputQueueSize {
		return errf(CodeInputOverflow, "input queue full")
	}
	s.inPending.Add(int64(len(p)))
	select {
	case s.inq <- append([]byte(nil), p...):
		return nil
	default:
		s.inPending.Add(-int64(len(p)))
		return errf(CodeInputOverflow, "input queue full")
	}
}

func (s *Session) writeLoop() {
	for {
		select {
		case <-s.readDone:
			return
		case p := <-s.inq:
			for len(p) > 0 {
				n := min(len(p), inputPiece)
				timer := time.AfterFunc(10*time.Second, func() { s.stalled.Store(true) })
				_, err := s.pty.Write(p[:n])
				timer.Stop()
				s.inPending.Add(-int64(n))
				if err != nil {
					s.inPending.Add(-int64(len(p) - n))
					return
				}
				s.stalled.Store(false)
				p = p[n:]
			}
		}
	}
}

func (s *Session) Resize(cols, rows int) error {
	if cols < 2 || cols > 500 || rows < 1 || rows > 200 {
		return errf(CodeBadSpec, "size out of range")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != "running" {
		return errf(CodeNotRunning, "session is not running")
	}
	if cols == s.cols && rows == s.rows {
		return nil
	}
	if err := s.pty.Resize(cols, rows); err != nil {
		return errf(CodeSpawnFailed, err.Error())
	}
	s.cols, s.rows = cols, rows
	return nil
}

// Redraw nudges a full-screen program into repainting by changing the height
// for a moment.
func (s *Session) Redraw() error {
	s.mu.Lock()
	cols, rows := s.cols, s.rows
	s.mu.Unlock()
	alt := rows - 1
	if alt < 1 {
		alt = rows + 1
	}
	if err := s.Resize(cols, alt); err != nil {
		return err
	}
	time.Sleep(50 * time.Millisecond)
	return s.Resize(cols, rows)
}

// Close ends the session. It returns at once; Done reports completion.
func (s *Session) Close(mode CloseMode, reason string) {
	select {
	case s.closeReq <- closeRequest{mode, reason}:
	default:
	}
}

// Done yields exactly one ExitInfo, after cleanup has finished.
func (s *Session) Done() <-chan ExitInfo { return s.done }

func (s *Session) waitProcess() {
	windows.WaitForSingleObject(s.sp.process, windows.INFINITE)
	var code uint32
	windows.GetExitCodeProcess(s.sp.process, &code)
	s.exitCode.Store(int64(code))
	close(s.procDone)
}

// supervise tracks descendants while the session runs and performs cleanup
// when the root exits or Close is called.
func (s *Session) supervise() {
	tick := time.NewTicker(treeInterval)
	defer tick.Stop()
	req := closeRequest{Force, ReasonSelf}
loop:
	for {
		select {
		case <-tick.C:
			s.tree.scan()
		case <-s.procDone:
			break loop
		case req = <-s.closeReq:
			break loop
		}
	}
	s.mu.Lock()
	s.state = "closing"
	s.mu.Unlock()
	s.tree.scan()

	selfExit := req.reason == ReasonSelf
	if selfExit {
		// Let the last output arrive before the console is torn down.
		waitFor(200*time.Millisecond, func() bool { return false })
	}

	// Tier 1: close the pseudo console; programs get a close event.
	go s.pty.ClosePTY()
	grace := 5 * time.Second
	if req.mode == Force || selfExit {
		grace = 300 * time.Millisecond
	}
	waitFor(grace, func() bool { s.tree.scan(); return !s.tree.anyAlive() })

	// Tier 2: the Job.
	windows.TerminateJobObject(s.sp.job, 1)
	windows.CloseHandle(s.sp.job)
	waitFor(time.Second, func() bool { return !s.tree.anyAlive() })

	// Tier 3: whatever escaped the Job but was seen as a descendant.
	s.tree.scan()
	s.tree.killAll()
	waitFor(time.Second, func() bool { return !s.tree.anyAlive() })
	leftover := s.tree.survivors()

	select {
	case <-s.readDone:
	case <-time.After(2 * time.Second):
	}
	select {
	case <-s.procDone:
	case <-time.After(time.Second):
	}
	s.pty.closePipes()
	s.tree.close()
	windows.CloseHandle(s.sp.process)

	s.mu.Lock()
	s.state = "exited"
	for id, sub := range s.subs {
		close(sub.ch)
		delete(s.subs, id)
	}
	s.mu.Unlock()
	s.done <- ExitInfo{Reason: req.reason, ExitCode: int(int32(s.exitCode.Load())), Leftover: leftover}
}

func waitFor(d time.Duration, cond func() bool) {
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if cond() {
			return
		}
	}
}

// publish never waits for a slow subscriber.
func (s *Session) publish(p []byte) {
	data := append([]byte(nil), p...)
	off := s.buf.Write(data)
	s.mu.Lock()
	for _, sub := range s.subs {
		c := Chunk{Offset: off, Data: data, Gap: sub.gapped}
		select {
		case sub.ch <- c:
			sub.gapped = false
		default:
			for len(sub.ch) > 0 {
				<-sub.ch
			}
			sub.gapped = true
		}
	}
	s.mu.Unlock()
}
