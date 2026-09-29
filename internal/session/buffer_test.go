package session

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"
)

func feed(s string) *Tracker {
	t := &Tracker{}
	t.Feed([]byte(s))
	return t
}

func TestTrackerDECModes(t *testing.T) {
	tr := feed("\x1b[?2004h\x1b[?1004h\x1b[?1000;1006h\x1b[?25l\x1b[?1h\x1b[?1007h")
	m := tr.Modes
	if !m.BracketedPaste || !m.FocusEvents || !m.MouseVT200 || !m.MouseSGR || !m.CursorHidden || !m.CursorKeys || !m.AltScroll {
		t.Fatalf("modes not set: %+v", m)
	}
	tr.Feed([]byte("\x1b[?2004l\x1b[?25h\x1b[?1000l"))
	m = tr.Modes
	if m.BracketedPaste || m.CursorHidden || m.MouseVT200 || !m.FocusEvents {
		t.Fatalf("modes not reset: %+v", m)
	}
	// Untracked and malformed sequences must not disturb anything.
	tr.Feed([]byte("\x1b[?9999h\x1b[31;1m\x1b[2J\x1b[?h\x1b[?;;h"))
	if tr.Modes != m || !tr.Safe() {
		t.Fatalf("state disturbed: %+v", tr.Modes)
	}
}

func TestTrackerAltScreen(t *testing.T) {
	tr := feed("\x1b[?1049h")
	if tr.Modes.AltScreen != 1049 {
		t.Fatal("alt screen not entered")
	}
	tr.Feed([]byte("\x1b[?1049l"))
	if tr.Modes.AltScreen != 0 {
		t.Fatal("alt screen not left")
	}
}

func TestTrackerKeyboard(t *testing.T) {
	tr := feed("\x1b=\x1b[>4;2m\x1b[>1u\x1b[>7u")
	m := tr.Modes
	if !m.KeypadApp || m.ModifyOther != 2 || !bytes.Equal(m.KittyFlags(), []uint8{1, 7}) {
		t.Fatalf("%+v %v", m, m.KittyFlags())
	}
	tr.Feed([]byte("\x1b[=5;1u")) // set top of stack
	if got := tr.Modes.KittyFlags(); !bytes.Equal(got, []uint8{1, 5}) {
		t.Fatalf("kitty set: %v", got)
	}
	tr.Feed([]byte("\x1b[<u\x1b>\x1b[>4m"))
	m = tr.Modes
	if m.KeypadApp || m.ModifyOther != 0 || !bytes.Equal(m.KittyFlags(), []uint8{1}) {
		t.Fatalf("after pop: %+v %v", m, m.KittyFlags())
	}
	tr.Feed([]byte("\x1b[<9u")) // popping more than exist empties the stack
	if len(tr.Modes.KittyFlags()) != 0 {
		t.Fatal("stack should be empty")
	}
	for i := 0; i < kittyDepth+3; i++ { // overflow keeps the newest entries
		tr.Feed([]byte("\x1b[>" + string(rune('0'+i%10)) + "u"))
	}
	if n := len(tr.Modes.KittyFlags()); n != kittyDepth {
		t.Fatalf("stack depth %d", n)
	}
}

func TestTrackerTitleAndStrings(t *testing.T) {
	var got []string
	tr := &Tracker{}
	tr.OnTitle(func(s string) { got = append(got, s) })
	tr.Feed([]byte("\x1b]0;first\x07\x1b]2;第二个\x1b\\\x1b]2;第二个\x07\x1b]8;;http://x\x07"))
	if len(got) != 2 || got[0] != "first" || got[1] != "第二个" || tr.Modes.Title != "第二个" {
		t.Fatalf("titles %q, current %q", got, tr.Modes.Title)
	}
	// A title must never be able to smuggle a terminator or controls into the prelude.
	tr.Feed([]byte("\x1b]2;a\x01b\x7fc\x07"))
	if tr.Modes.Title != "abc" {
		t.Fatalf("title not sanitised: %q", tr.Modes.Title)
	}
	tr.Feed([]byte("\x1b]2;" + strings.Repeat("长", 500) + "\x07"))
	if len(tr.Modes.Title) > maxTitle || !utf8.ValidString(tr.Modes.Title) {
		t.Fatalf("title length %d valid=%v", len(tr.Modes.Title), utf8.ValidString(tr.Modes.Title))
	}
	// DCS / APC bodies are skipped, including mode-looking text inside them.
	tr2 := feed("\x1bP[?2004h-not-real\x1b\\\x1b_[?1004h\x1b\\")
	if tr2.Modes.BracketedPaste || tr2.Modes.FocusEvents || !tr2.Safe() {
		t.Fatalf("string body was interpreted: %+v", tr2.Modes)
	}
	// But an ESC inside a string aborts it and starts a new sequence, exactly
	// as real terminals (and xterm.js) do; the tracker must agree with them.
	tr3 := feed("\x1bPbody\x1b[?2004h")
	if !tr3.Modes.BracketedPaste || !tr3.Safe() {
		t.Fatalf("ESC must abort a string: %+v", tr3.Modes)
	}
}

func TestTrackerResets(t *testing.T) {
	tr := feed("\x1b]2;keep\x07\x1b[?2004h\x1b[?1049h\x1b[>1u\x1b[!p")
	if tr.Modes.BracketedPaste || tr.Modes.AltScreen != 1049 || tr.Modes.Title != "keep" {
		t.Fatalf("soft reset: %+v", tr.Modes)
	}
	tr.Feed([]byte("\x1b[?1004h\x1bc"))
	if tr.Modes.FocusEvents || tr.Modes.AltScreen != 0 || tr.Modes.Title != "keep" {
		t.Fatalf("hard reset: %+v", tr.Modes)
	}
}

func TestTrackerSafe(t *testing.T) {
	steps := []struct {
		in   string
		safe bool
	}{
		{"abc", true},
		{"\x1b", false},
		{"[", false},
		{"?2004", false},
		{"h", true},
		{"\xe4", false}, // first byte of 中
		{"\xb8", false},
		{"\xad", true},
		{"\xf0\x9f\x98", false}, // emoji, 3 of 4 bytes
		{"\x80", true},
		{"\x1b]0;title", false},
		{"\x07", true},
		{"\x1b[31", false},
		{"\x18", true}, // CAN aborts
		{"\x1bP1;2|data\x1b", false},
		{"\\", true},
	}
	tr := &Tracker{}
	for i, s := range steps {
		tr.Feed([]byte(s.in))
		if tr.Safe() != s.safe {
			t.Fatalf("step %d %q: Safe=%v want %v", i, s.in, tr.Safe(), s.safe)
		}
	}
}

// A prelude, fed to a fresh tracker, must reproduce exactly the modes it was built from.
func TestPreludeRoundTrip(t *testing.T) {
	src := feed("\x1b[?1049h\x1b[?2004h\x1b[?1004h\x1b[?1002;1006h\x1b[?25l\x1b[?1h\x1b[?1007h" +
		"\x1b=\x1b[>4;2m\x1b[>1u\x1b[>15u\x1b[5 q\x1b[2;20r\x1b]2;my 标题\x07")
	again := feed(string(src.Modes.Prelude()))
	if again.Modes != src.Modes {
		t.Fatalf("prelude does not reproduce modes:\n got %+v\nwant %+v", again.Modes, src.Modes)
	}
	if !again.Safe() {
		t.Fatal("prelude must end in ground state")
	}
	var zero Modes
	if len(zero.Prelude()) != 0 {
		t.Fatalf("default modes need no prelude, got %q", zero.Prelude())
	}
}

func TestBufferOffsetsAndResume(t *testing.T) {
	b := NewBuffer(1 << 20)
	if off := b.Write([]byte("hello ")); off != 0 {
		t.Fatalf("first write at %d", off)
	}
	if off := b.Write([]byte("world")); off != 6 {
		t.Fatalf("second write at %d", off)
	}
	have := uint64(6)
	r := b.Replay(&have)
	if !r.Resume || r.From != 6 || r.End != 11 || string(r.Data) != "world" || r.Prelude != nil {
		t.Fatalf("resume: %+v", r)
	}
	have = 11 // fully caught up
	if r = b.Replay(&have); !r.Resume || len(r.Data) != 0 {
		t.Fatalf("caught up: %+v", r)
	}
	have = 12 // claims more than exists: cannot resume
	if r = b.Replay(&have); r.Resume {
		t.Fatal("resumed from the future")
	}
	if r = b.Replay(nil); r.Resume || r.From != 0 || string(r.Data) != "hello world" {
		t.Fatalf("full replay: %+v", r)
	}
	if d, ok := b.Slice(6); !ok || string(d) != "world" {
		t.Fatalf("slice: %q %v", d, ok)
	}
	if _, ok := b.Slice(99); ok {
		t.Fatal("slice beyond end")
	}
}

// The core promise of docs/M2 第 7 节: modes set long ago, whose sequences have
// left the ring, still reach a late joiner, and the replay starts on a clean boundary.
func TestReplayAfterWrapCarriesModes(t *testing.T) {
	const size = 256 << 10
	b := NewBuffer(size)
	b.Write([]byte("\x1b[?2004h\x1b[?1004h\x1b]2;claude\x07"))
	line := []byte("中文输出 with \x1b[32mcolour\x1b[0m and emoji 😀 padding\r\n")
	for written := 0; written < 3*size; written += len(line) {
		b.Write(line)
	}
	base, end := b.Range()
	if end-base != size {
		t.Fatalf("ring should be full: %d", end-base)
	}
	have := uint64(10) // long gone
	r := b.Replay(&have)
	if r.Resume {
		t.Fatal("must not resume from an evicted offset")
	}
	if r.From < base || r.From > end || r.End != end {
		t.Fatalf("replay range [%d,%d) outside ring [%d,%d)", r.From, r.End, base, end)
	}
	if r.From-base > 2*safeInterval {
		t.Fatalf("replay discards too much: starts %d bytes after base", r.From-base)
	}
	p := feed(string(r.Prelude)).Modes
	if !p.BracketedPaste || !p.FocusEvents || p.Title != "claude" {
		t.Fatalf("prelude lost modes: %q", r.Prelude)
	}
	if !utf8.Valid(r.Data) {
		t.Fatal("replay starts or ends inside a UTF-8 character")
	}
	// Replaying prelude + data into a fresh tracker must leave it exactly where the live one is.
	fresh := feed(string(r.Prelude) + string(r.Data))
	if fresh.Modes != b.Modes() || !fresh.Safe() {
		t.Fatalf("replayed state differs:\n got %+v\nwant %+v", fresh.Modes, b.Modes())
	}
}

// A mode switched off inside the ring must end up off, even though the prelude turned it on.
func TestReplayModeChangesInsideRing(t *testing.T) {
	const size = 256 << 10
	b := NewBuffer(size)
	b.Write([]byte("\x1b[?1049h\x1b[?2004h"))
	filler := bytes.Repeat([]byte("x"), 1000)
	for i := 0; i < 2*size/1000; i++ {
		b.Write(filler)
	}
	b.Write([]byte("\x1b[?1049l"))
	r := b.Replay(nil)
	if r.AltScreen {
		t.Fatal("session left the alternate screen")
	}
	fresh := feed(string(r.Prelude) + string(r.Data))
	if fresh.Modes.AltScreen != 0 || !fresh.Modes.BracketedPaste {
		t.Fatalf("got %+v", fresh.Modes)
	}
	b.Write([]byte("\x1b[?1049h"))
	if !b.Replay(nil).AltScreen {
		t.Fatal("AltScreen flag must follow the live state")
	}
}

// One endless escape sequence fills the ring: there is no place to cut.
func TestReplayWithNoSafePoint(t *testing.T) {
	const size = 128 << 10
	b := NewBuffer(size)
	b.Write([]byte("\x1b[?2004h\x1bP"))
	b.Write(bytes.Repeat([]byte("d"), 2*size)) // DCS body, never terminated
	r := b.Replay(nil)
	if len(r.Data) != 0 || r.From != r.End {
		t.Fatalf("expected a modes-only replay, got %d bytes", len(r.Data))
	}
	if !feed(string(r.Prelude)).Modes.BracketedPaste {
		t.Fatal("modes-only replay must still carry modes")
	}
	b.Write([]byte("\x1b\\ok")) // sequence ends: cutting is possible again
	if r = b.Replay(nil); !strings.HasSuffix(string(r.Data), "ok") && len(r.Data) == 0 {
		t.Fatalf("safe point not re-established: %+v", r)
	}
}

func TestBufferWrapContent(t *testing.T) {
	b := NewBuffer(10)
	b.Write([]byte("0123456789"))
	b.Write([]byte("abcd"))
	if d, ok := b.Slice(4); !ok || string(d) != "456789abcd" {
		t.Fatalf("got %q %v", d, ok)
	}
	if _, ok := b.Slice(3); ok {
		t.Fatal("offset 3 was evicted")
	}
	b.Write(bytes.Repeat([]byte("z"), 35)) // larger than the ring itself
	base, end := b.Range()
	if d, _ := b.Slice(base); end-base != 10 || string(d) != "zzzzzzzzzz" {
		t.Fatalf("got %q", d)
	}
}

func FuzzTrackerNeverPanics(f *testing.F) {
	f.Add([]byte("\x1b[?2004h\x1b]2;t\x07\x1b[>1u\x1bP\x1b\\"))
	f.Add([]byte("\x1b[;;;;;;;;;;;;;;;;;;;;;;;;99999999999999h"))
	f.Fuzz(func(t *testing.T, data []byte) {
		tr := &Tracker{}
		tr.Feed(data)
		// Whatever state we reached, its prelude must reproduce it and be self-contained.
		again := feed(string(tr.Modes.Prelude()))
		if again.Modes != tr.Modes || !again.Safe() {
			t.Fatalf("prelude not faithful for input %q", data)
		}
	})
}

func FuzzBufferReplay(f *testing.F) {
	f.Add([]byte("中\x1b[?1049h文\x1b]0;x\x07"), uint16(7))
	f.Fuzz(func(t *testing.T, chunk []byte, reps uint16) {
		if len(chunk) == 0 {
			return
		}
		b := NewBuffer(64 << 10)
		live := &Tracker{}
		for i := 0; i < int(reps%400)+1; i++ {
			b.Write(chunk)
			live.Feed(chunk)
		}
		r := b.Replay(nil)
		base, end := b.Range()
		if r.From < base || r.From > end || r.From+uint64(len(r.Data)) != end {
			t.Fatalf("replay range broken: from=%d len=%d ring=[%d,%d)", r.From, len(r.Data), base, end)
		}
		if got := feed(string(r.Prelude) + string(r.Data)); len(r.Data) > 0 && got.Modes != live.Modes {
			t.Fatalf("replayed modes differ")
		}
	})
}
