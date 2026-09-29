// Package session implements docs/M2-终端会话层.md. The files without a
// _windows suffix are platform independent: the output buffer, the terminal
// mode tracker and replay construction.
package session

import (
	"strconv"
	"strings"
)

// Modes is the part of the terminal state that a late-joining browser must be
// told about, because the sequences that set it may long have left the output
// buffer (docs/M2 第 7 节). It is a plain value: snapshots are copies.
type Modes struct {
	CursorKeys     bool // ?1   DECCKM
	CursorHidden   bool // ?25  (inverted: default is visible)
	AltScreen      int  // 0, or the mode number used to enter it: 47, 1047, 1049
	MouseX10       bool // ?9
	MouseVT200     bool // ?1000
	MouseBtnEvent  bool // ?1002
	MouseAnyEvent  bool // ?1003
	MouseUTF8      bool // ?1005
	MouseSGR       bool // ?1006
	MouseURXVT     bool // ?1015
	FocusEvents    bool // ?1004
	AltScroll      bool // ?1007 wheel-to-arrows, Codex turns this on
	BracketedPaste bool // ?2004
	KeypadApp      bool // ESC = / ESC >
	ModifyOther    int  // CSI > 4 ; n m
	CursorStyle    int  // CSI n SP q, 0 = default
	ScrollTop      int  // DECSTBM, 0 = unset
	ScrollBottom   int
	Title          string
	kitty          [kittyDepth]uint8 // kitty keyboard flag stack
	kittyLen       int
}

const (
	kittyDepth = 8
	maxTitle   = 256
)

// decMode returns a pointer to the bool tracking DEC private mode n, or nil.
func (m *Modes) decMode(n int) *bool {
	switch n {
	case 1:
		return &m.CursorKeys
	case 9:
		return &m.MouseX10
	case 1000:
		return &m.MouseVT200
	case 1002:
		return &m.MouseBtnEvent
	case 1003:
		return &m.MouseAnyEvent
	case 1004:
		return &m.FocusEvents
	case 1005:
		return &m.MouseUTF8
	case 1006:
		return &m.MouseSGR
	case 1007:
		return &m.AltScroll
	case 1015:
		return &m.MouseURXVT
	case 2004:
		return &m.BracketedPaste
	}
	return nil
}

func (m *Modes) setDEC(n int, on bool) {
	switch n {
	case 25:
		m.CursorHidden = !on
	case 47, 1047, 1049:
		if on {
			m.AltScreen = n
		} else {
			m.AltScreen = 0
		}
	default:
		if p := m.decMode(n); p != nil {
			*p = on
		}
	}
}

// KittyFlags returns the current kitty keyboard flag stack, bottom first.
func (m *Modes) KittyFlags() []uint8 { return m.kitty[:m.kittyLen] }

// Prelude renders the sequences that bring a freshly reset terminal into
// state m. The alternate screen comes first so later state lands on it.
func (m *Modes) Prelude() []byte {
	var b strings.Builder
	dec := func(n int, on bool) {
		if on {
			b.WriteString("\x1b[?" + strconv.Itoa(n) + "h")
		}
	}
	dec(m.AltScreen, m.AltScreen != 0)
	dec(1, m.CursorKeys)
	dec(9, m.MouseX10)
	dec(1000, m.MouseVT200)
	dec(1002, m.MouseBtnEvent)
	dec(1003, m.MouseAnyEvent)
	dec(1005, m.MouseUTF8)
	dec(1006, m.MouseSGR)
	dec(1015, m.MouseURXVT)
	dec(1004, m.FocusEvents)
	dec(1007, m.AltScroll)
	dec(2004, m.BracketedPaste)
	if m.CursorHidden {
		b.WriteString("\x1b[?25l")
	}
	if m.KeypadApp {
		b.WriteString("\x1b=")
	}
	if m.ModifyOther != 0 {
		b.WriteString("\x1b[>4;" + strconv.Itoa(m.ModifyOther) + "m")
	}
	for _, f := range m.KittyFlags() {
		b.WriteString("\x1b[>" + strconv.Itoa(int(f)) + "u")
	}
	if m.CursorStyle != 0 {
		b.WriteString("\x1b[" + strconv.Itoa(m.CursorStyle) + " q")
	}
	if m.ScrollTop != 0 || m.ScrollBottom != 0 {
		b.WriteString("\x1b[" + strconv.Itoa(m.ScrollTop) + ";" + strconv.Itoa(m.ScrollBottom) + "r")
	}
	if m.Title != "" {
		b.WriteString("\x1b]2;" + m.Title + "\x1b\\")
	}
	return []byte(b.String())
}

// Tracker is a parse-only VT state machine. It renders nothing; it follows the
// escape sequences in a program's output far enough to keep Modes current and
// to know when the stream is at a safe cut point.
type Tracker struct {
	Modes Modes

	state    vtState
	utf8Left int // continuation bytes still expected, ground state only

	private byte  // CSI private marker: ? > < =
	inter   byte  // last CSI intermediate, e.g. SP in "CSI n SP q"
	params  []int // CSI parameters; -1 = omitted
	cur     int
	hasCur  bool
	tooLong bool

	osc     []byte
	oscEsc  bool // saw ESC inside a string, expecting '\'
	onTitle func(string)
}

type vtState uint8

const (
	stGround vtState = iota
	stEsc
	stEscInter
	stCSI
	stOSC
	stString // DCS, SOS, PM, APC: skipped up to ST
)

const (
	maxParams = 16
	maxOSC    = 4096
)

// Safe reports whether the stream could be cut right here: not inside an
// escape sequence and not inside a multi-byte UTF-8 character.
func (t *Tracker) Safe() bool { return t.state == stGround && t.utf8Left == 0 }

// OnTitle registers a callback for title changes.
func (t *Tracker) OnTitle(f func(string)) { t.onTitle = f }

// Feed advances the tracker over p.
func (t *Tracker) Feed(p []byte) {
	for _, c := range p {
		t.Step(c)
	}
}

// Step advances the tracker by one byte.
func (t *Tracker) Step(c byte) {
	// CAN and SUB abort any sequence; ESC restarts one, except inside a string
	// where ESC \ is the terminator.
	if c == 0x18 || c == 0x1a {
		t.state, t.utf8Left = stGround, 0
		return
	}
	switch t.state {
	case stGround:
		t.ground(c)
	case stEsc:
		t.esc(c)
	case stEscInter:
		if c >= 0x30 && c <= 0x7e {
			t.state = stGround
		} else if c == 0x1b {
			t.state = stEsc
		}
	case stCSI:
		t.csi(c)
	case stOSC, stString:
		t.str(c)
	}
}

func (t *Tracker) ground(c byte) {
	switch {
	case c == 0x1b:
		t.state, t.utf8Left = stEsc, 0
	case c < 0x80:
		t.utf8Left = 0
	case c >= 0xc0 && c < 0xe0:
		t.utf8Left = 1
	case c >= 0xe0 && c < 0xf0:
		t.utf8Left = 2
	case c >= 0xf0 && c < 0xf8:
		t.utf8Left = 3
	case c >= 0x80 && c < 0xc0:
		if t.utf8Left > 0 {
			t.utf8Left--
		}
	default:
		t.utf8Left = 0
	}
}

func (t *Tracker) esc(c byte) {
	switch {
	case c == '[':
		t.state = stCSI
		t.private, t.inter, t.params, t.cur, t.hasCur, t.tooLong = 0, 0, t.params[:0], 0, false, false
	case c == ']':
		t.state, t.osc, t.oscEsc = stOSC, t.osc[:0], false
	case c == 'P' || c == 'X' || c == '^' || c == '_':
		t.state, t.oscEsc = stString, false
	case c == '=':
		t.Modes.KeypadApp, t.state = true, stGround
	case c == '>':
		t.Modes.KeypadApp, t.state = false, stGround
	case c == 'c': // RIS: full reset
		title := t.Modes.Title
		t.Modes = Modes{Title: title}
		t.state = stGround
	case c >= 0x20 && c <= 0x2f:
		t.state = stEscInter
	case c == 0x1b:
		// stay
	default:
		t.state = stGround
	}
}

func (t *Tracker) csi(c byte) {
	switch {
	case c >= '0' && c <= '9':
		if t.cur < 100000 {
			t.cur = t.cur*10 + int(c-'0')
		}
		t.hasCur = true
	case c == ';' || c == ':':
		t.pushParam()
	case c >= 0x3c && c <= 0x3f: // < = > ?
		if len(t.params) == 0 && !t.hasCur {
			t.private = c
		}
	case c >= 0x20 && c <= 0x2f:
		t.inter = c
	case c >= 0x40 && c <= 0x7e:
		t.pushParam()
		if !t.tooLong {
			t.dispatchCSI(c)
		}
		t.state = stGround
	case c == 0x1b:
		t.state = stEsc
	}
}

func (t *Tracker) pushParam() {
	v := -1
	if t.hasCur {
		v = t.cur
	}
	if len(t.params) < maxParams {
		t.params = append(t.params, v)
	} else {
		t.tooLong = true
	}
	t.cur, t.hasCur = 0, false
}

func (t *Tracker) param(i, def int) int {
	if i < len(t.params) && t.params[i] >= 0 {
		return t.params[i]
	}
	return def
}

func (t *Tracker) dispatchCSI(final byte) {
	m := &t.Modes
	switch {
	case t.private == '?' && t.inter == 0 && (final == 'h' || final == 'l'):
		for _, p := range t.params {
			if p >= 0 {
				m.setDEC(p, final == 'h')
			}
		}
	case t.private == '>' && final == 'm': // modifyOtherKeys
		if t.param(0, -1) == 4 {
			m.ModifyOther = t.param(1, 0)
		}
	case t.private == '>' && final == 'u': // kitty push
		if m.kittyLen == kittyDepth {
			copy(m.kitty[:], m.kitty[1:])
			m.kittyLen--
		}
		m.kitty[m.kittyLen] = uint8(t.param(0, 0))
		m.kittyLen++
	case t.private == '<' && final == 'u': // kitty pop
		m.kittyLen = max(0, m.kittyLen-t.param(0, 1))
	case t.private == '=' && final == 'u': // kitty set on top of stack
		if m.kittyLen == 0 {
			m.kittyLen = 1
			m.kitty[0] = 0
		}
		top, flags := &m.kitty[m.kittyLen-1], uint8(t.param(0, 0))
		switch t.param(1, 1) {
		case 1:
			*top = flags
		case 2:
			*top |= flags
		case 3:
			*top &^= flags
		}
	case t.private == 0 && t.inter == ' ' && final == 'q':
		m.CursorStyle = t.param(0, 0)
	case t.private == 0 && t.inter == 0 && final == 'r':
		m.ScrollTop, m.ScrollBottom = t.param(0, 0), t.param(1, 0)
	case t.private == 0 && t.inter == '!' && final == 'p': // DECSTR soft reset
		title, alt := m.Title, m.AltScreen
		*m = Modes{Title: title, AltScreen: alt}
	}
}

// str consumes OSC and the skipped string types up to BEL or ST.
func (t *Tracker) str(c byte) {
	if t.oscEsc {
		t.oscEsc = false
		if c == '\\' {
			t.endString()
			return
		}
		// ESC followed by something else: the string is aborted and a new
		// escape sequence begins.
		t.state = stEsc
		t.esc(c)
		return
	}
	switch {
	case c == 0x1b:
		t.oscEsc = true
	case c == 0x07:
		t.endString()
	case t.state == stOSC && len(t.osc) < maxOSC:
		t.osc = append(t.osc, c)
	}
}

func (t *Tracker) endString() {
	if t.state == stOSC {
		if num, text, ok := strings.Cut(string(t.osc), ";"); ok && (num == "0" || num == "2") {
			text = sanitizeTitle(text)
			if text != t.Modes.Title {
				t.Modes.Title = text
				if t.onTitle != nil {
					t.onTitle(text)
				}
			}
		}
	}
	t.state = stGround
}

// sanitizeTitle drops control characters, so a title can be replayed inside an
// OSC without being able to terminate it early, and bounds its length.
func sanitizeTitle(s string) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return -1
		}
		return r
	}, s)
	if len(s) > maxTitle {
		s = strings.ToValidUTF8(s[:maxTitle], "")
	}
	return s
}
