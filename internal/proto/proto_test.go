package proto

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"testing"
)

func TestSID(t *testing.T) {
	a, b := NewSID(), NewSID()
	if a == b || a.IsZero() {
		t.Fatal("ids must be unique and non-zero")
	}
	if a[6]>>4 != 7 || a[8]>>6 != 2 {
		t.Fatalf("not a UUIDv7: % x", a)
	}
	got, err := ParseSID(a.String())
	if err != nil || got != a {
		t.Fatalf("round trip: %v %v", got, err)
	}
	for _, bad := range []string{"", "xyz", a.String()[:35], a.String() + "0", "g" + a.String()[1:]} {
		if _, err := ParseSID(bad); err == nil {
			t.Errorf("ParseSID(%q) accepted", bad)
		}
	}
	// JSON goes through MarshalText.
	js, _ := json.Marshal(struct{ S SID }{a})
	var back struct{ S SID }
	if err := json.Unmarshal(js, &back); err != nil || back.S != a {
		t.Fatalf("json round trip: %s %v", js, err)
	}
}

func TestFrameRoundTrip(t *testing.T) {
	sid := NewSID()
	cases := []Frame{
		{Type: TypeOutput, SID: sid, Offset: 1 << 40, Data: []byte("hello\x1b[0m")},
		{Type: TypeOutput, SID: sid, Offset: 0, Data: nil},
		{Type: TypeInput, SID: sid, Data: []byte("ls\r")},
		{Type: TypeReplay, SID: sid, Req: 7, Offset: 99, Data: bytes.Repeat([]byte{'x'}, 100000)},
		{Type: TypeControl, Data: []byte(`{"t":"ping"}`)},
	}
	for _, want := range cases {
		b, err := AppendFrame(nil, want)
		if err != nil {
			t.Fatalf("encode %#x: %v", want.Type, err)
		}
		got, err := ParseFrame(b)
		if err != nil {
			t.Fatalf("decode %#x: %v", want.Type, err)
		}
		if got.Type != want.Type || got.SID != want.SID || got.Offset != want.Offset ||
			got.Req != want.Req || !bytes.Equal(got.Data, want.Data) {
			t.Fatalf("mismatch for type %#x", want.Type)
		}
	}
}

func TestFrameLimits(t *testing.T) {
	sid := NewSID()
	if _, err := AppendFrame(nil, Frame{Type: TypeOutput, SID: sid, Data: make([]byte, MaxOutputData+1)}); !errors.Is(err, ErrFrameSize) {
		t.Errorf("oversized output: %v", err)
	}
	if _, err := AppendFrame(nil, Frame{Type: TypeInput, SID: sid, Data: make([]byte, MaxInputData+1)}); !errors.Is(err, ErrFrameSize) {
		t.Errorf("oversized input: %v", err)
	}
	if _, err := AppendFrame(nil, Frame{Type: 0x7f}); !errors.Is(err, ErrFrameType) {
		t.Errorf("unknown type: %v", err)
	}
	// Hand-built oversized input frame must be refused on decode too.
	raw := append(append([]byte{TypeInput}, sid[:]...), make([]byte, MaxInputData+1)...)
	if _, err := ParseFrame(raw); !errors.Is(err, ErrFrameSize) {
		t.Errorf("decode oversized input: %v", err)
	}
	if _, err := ParseFrame(make([]byte, MaxFrame+1)); !errors.Is(err, ErrFrameSize) {
		t.Errorf("decode oversized frame: %v", err)
	}
	for _, short := range [][]byte{nil, {TypeOutput}, append([]byte{TypeOutput}, sid[:]...), append([]byte{TypeReplay}, sid[:]...)} {
		if _, err := ParseFrame(short); !errors.Is(err, ErrFrameShort) {
			t.Errorf("short frame % x: %v", short, err)
		}
	}
	// Offset + length overflowing uint64 is corrupt.
	raw = append(append([]byte{TypeOutput}, sid[:]...), 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 'a', 'b')
	if _, err := ParseFrame(raw); err == nil {
		t.Error("offset overflow accepted")
	}
}

func TestBrowserFrames(t *testing.T) {
	b, err := AppendBrowserOutput(nil, 12345, []byte("data"))
	if err != nil {
		t.Fatal(err)
	}
	off, data, err := ParseBrowserOutput(b)
	if err != nil || off != 12345 || string(data) != "data" {
		t.Fatalf("got %d %q %v", off, data, err)
	}
	if _, _, err := ParseBrowserOutput([]byte{1, 2, 3}); !errors.Is(err, ErrFrameShort) {
		t.Errorf("short: %v", err)
	}
	if err := CheckBrowserInput(make([]byte, MaxInputData+1)); !errors.Is(err, ErrFrameSize) {
		t.Errorf("oversized input: %v", err)
	}
}

func TestPipeFraming(t *testing.T) {
	var buf bytes.Buffer
	frames := [][]byte{[]byte("one"), {}, bytes.Repeat([]byte{'z'}, 70000)}
	for _, f := range frames {
		if err := WritePipeFrame(&buf, f); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range frames {
		got, err := ReadPipeFrame(&buf)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("got %d bytes, err %v", len(got), err)
		}
	}
	if _, err := ReadPipeFrame(&buf); err != io.EOF {
		t.Errorf("clean end: %v", err)
	}
	// A hostile length must be refused before any allocation.
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], MaxFrame+1)
	if _, err := ReadPipeFrame(bytes.NewReader(hdr[:])); !errors.Is(err, ErrFrameSize) {
		t.Errorf("oversized length: %v", err)
	}
	// Truncated body.
	binary.BigEndian.PutUint32(hdr[:], 10)
	if _, err := ReadPipeFrame(bytes.NewReader(append(hdr[:], 'a', 'b'))); err != io.ErrUnexpectedEOF {
		t.Errorf("truncated: %v", err)
	}
	if err := WritePipeFrame(io.Discard, make([]byte, MaxFrame+1)); !errors.Is(err, ErrFrameSize) {
		t.Errorf("oversized write: %v", err)
	}
}

func TestTracker(t *testing.T) {
	tr := NewTracker(100)
	step := func(start uint64, data string, wantFresh string, wantGap bool, wantHave uint64) {
		t.Helper()
		fresh, gap := tr.Accept(start, []byte(data))
		if string(fresh) != wantFresh || gap != wantGap || tr.Have() != wantHave {
			t.Fatalf("Accept(%d,%q) = %q,%v have=%d; want %q,%v have=%d",
				start, data, fresh, gap, tr.Have(), wantFresh, wantGap, wantHave)
		}
	}
	step(100, "abcde", "abcde", false, 105) // in order
	step(100, "abcde", "", false, 105)      // exact repeat
	step(103, "defgh", "fgh", false, 108)   // overlap: only the tail is new
	step(90, "old", "", false, 108)         // entirely old
	step(110, "later", "", true, 108)       // bytes 108..110 missing
	step(108, "", "", false, 108)           // empty frame
	step(^uint64(0), "xy", "", true, 108)   // overflow is corrupt
	tr.Reset(5000)
	step(5000, "z", "z", false, 5001)
}

func TestMuxOverflowBecomesGap(t *testing.T) {
	m := NewMux(100)
	sid := NewSID()
	m.Push(sid, 0, bytes.Repeat([]byte{'a'}, 60))
	m.Push(sid, 60, bytes.Repeat([]byte{'b'}, 60)) // 120 > 100: drop queue, keep this chunk
	it, ok := m.Next()
	if !ok || !it.Gap || it.Offset != 60 {
		t.Fatalf("want gap continuing at 60, got %+v ok=%v", it, ok)
	}
	it, ok = m.Next()
	if !ok || it.Gap || it.Offset != 60 || len(it.Data) != 60 || it.Data[0] != 'b' {
		t.Fatalf("want the surviving chunk, got %+v", it)
	}
	if _, ok := m.Next(); ok {
		t.Fatal("queue should be empty")
	}
	// A single push larger than the whole limit is skipped entirely.
	m.Push(sid, 120, bytes.Repeat([]byte{'c'}, 500))
	it, ok = m.Next()
	if !ok || !it.Gap || it.Offset != 620 {
		t.Fatalf("want gap continuing at 620, got %+v", it)
	}
	if m.Pending(sid) != 0 {
		t.Fatal("nothing should be pending")
	}
}

func TestMuxRoundRobinAndSplit(t *testing.T) {
	m := NewMux(SessionQueue)
	noisy, quiet := NewSID(), NewSID()
	m.Push(noisy, 0, make([]byte, 5*MaxOutputData)) // split into 5 frames
	m.Push(quiet, 0, []byte("x"))
	var order []SID
	var noisyNext uint64
	for {
		it, ok := m.Next()
		if !ok {
			break
		}
		if len(it.Data) > MaxOutputData {
			t.Fatalf("frame of %d bytes exceeds limit", len(it.Data))
		}
		if it.SID == noisy {
			if it.Offset != noisyNext {
				t.Fatalf("noisy offsets not contiguous: %d != %d", it.Offset, noisyNext)
			}
			noisyNext += uint64(len(it.Data))
		}
		order = append(order, it.SID)
	}
	if len(order) != 6 || order[1] != quiet {
		t.Fatalf("quiet session must be served second, not after the flood: %v", order)
	}
	select {
	case <-m.Wake():
	default:
		t.Fatal("Push must signal Wake")
	}
}

func TestMuxDrop(t *testing.T) {
	m := NewMux(1000)
	a, b := NewSID(), NewSID()
	m.Push(a, 0, []byte("aaa"))
	m.Push(b, 0, []byte("bbb"))
	m.Drop(a)
	it, ok := m.Next()
	if !ok || it.SID != b {
		t.Fatalf("got %+v", it)
	}
	if _, ok := m.Next(); ok {
		t.Fatal("dropped session must not reappear")
	}
	m.Drop(a) // dropping twice is harmless
}

func TestMsgRoundTrip(t *testing.T) {
	sid := NewSID()
	have := uint64(0)
	in := &Msg{T: MsgAttach, ID: 3, SID: &sid, Have: &have, Cols: 120, Rows: 32}
	b, err := EncodeMsg(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := DecodeMsg(b)
	if err != nil {
		t.Fatal(err)
	}
	if out.T != MsgAttach || out.ID != 3 || *out.SID != sid || out.Have == nil || *out.Have != 0 || out.Cols != 120 {
		t.Fatalf("got %+v", out)
	}
	if !out.IsRequest() || out.Reply().Re != 3 || out.Fail(ErrBusy, "x").Code != ErrBusy {
		t.Fatal("request/reply helpers")
	}
	// "have": null and an absent have both mean "replay everything"; have 0 does not.
	for _, js := range []string{`{"t":"attach","have":null}`, `{"t":"attach"}`} {
		m, err := DecodeMsg([]byte(js))
		if err != nil || m.Have != nil {
			t.Fatalf("%s: %+v %v", js, m, err)
		}
	}
}

func TestMsgValidation(t *testing.T) {
	bad := []string{``, `{`, `[]`, `{}`, `{"t":""}`, `{"t":"resize","cols":1,"rows":10}`, `{"t":"resize","cols":80,"rows":9999}`, `{"t":"attach","sid":"nope"}`}
	for _, js := range bad {
		if _, err := DecodeMsg([]byte(js)); err == nil {
			t.Errorf("accepted %q", js)
		}
	}
	// Unknown fields and unknown types decode fine: minor versions may add them.
	m, err := DecodeMsg([]byte(`{"t":"from_the_future","new_field":{"a":1}}`))
	if err != nil || m.T != "from_the_future" {
		t.Fatalf("%+v %v", m, err)
	}
	if _, err := DecodeMsg(make([]byte, MaxJSON+1)); !errors.Is(err, ErrFrameSize) {
		t.Errorf("oversized json: %v", err)
	}
	if _, err := EncodeMsg(&Msg{}); !errors.Is(err, ErrMsgInvalid) {
		t.Errorf("empty type: %v", err)
	}
	if _, err := EncodeMsg(&Msg{T: "x", Text: string(make([]byte, MaxJSON))}); !errors.Is(err, ErrFrameSize) {
		t.Errorf("oversized encode: %v", err)
	}
}

func TestVersion(t *testing.T) {
	v, err := ParseVersion("1.4")
	if err != nil || v != (Version{1, 4}) || v.String() != "1.4" {
		t.Fatalf("%v %v", v, err)
	}
	for _, bad := range []string{"", "1", "a.b", "0.1", "1.-1", "1.2.3"} {
		if _, err := ParseVersion(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	got, err := Negotiate(Version{Current.Major, Current.Minor + 5})
	if err != nil || got != Current {
		t.Fatalf("newer minor peer: %v %v", got, err)
	}
	if _, err := Negotiate(Version{Current.Major + 1, 0}); err == nil || err.Error() != ErrProtoMismatch {
		t.Fatalf("other major: %v", err)
	}
}

func TestCheckSize(t *testing.T) {
	for _, ok := range [][2]int{{2, 1}, {500, 200}, {120, 32}} {
		if CheckSize(ok[0], ok[1]) != nil {
			t.Errorf("rejected %v", ok)
		}
	}
	for _, bad := range [][2]int{{1, 10}, {501, 10}, {80, 0}, {80, 201}, {-1, -1}} {
		if CheckSize(bad[0], bad[1]) == nil {
			t.Errorf("accepted %v", bad)
		}
	}
}

func FuzzParseFrame(f *testing.F) {
	sid := NewSID()
	for _, fr := range []Frame{{Type: TypeOutput, SID: sid, Offset: 9, Data: []byte("x")}, {Type: TypeReplay, SID: sid, Req: 1}, {Type: TypeControl, Data: []byte("{}")}} {
		b, _ := AppendFrame(nil, fr)
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		fr, err := ParseFrame(b)
		if err != nil {
			return
		}
		// Whatever parses must re-encode to something that parses identically.
		again, err := AppendFrame(nil, fr)
		if err != nil {
			if fr.Type == TypeReplay { // replay has a larger decode bound than encode bound only at MaxFrame edge
				return
			}
			t.Fatalf("re-encode: %v", err)
		}
		fr2, err := ParseFrame(again)
		if err != nil || fr2.Type != fr.Type || fr2.Offset != fr.Offset || !bytes.Equal(fr2.Data, fr.Data) {
			t.Fatalf("unstable round trip: %v", err)
		}
	})
}

func FuzzTracker(f *testing.F) {
	f.Add(uint64(10), uint64(5), []byte("hello world"))
	f.Fuzz(func(t *testing.T, have, start uint64, data []byte) {
		tr := NewTracker(have)
		fresh, gap := tr.Accept(start, data)
		if tr.Have() < have {
			t.Fatal("have must never move backwards")
		}
		if gap && (len(fresh) != 0 || tr.Have() != have) {
			t.Fatal("a gap must not deliver data or advance")
		}
		if uint64(len(fresh)) != tr.Have()-have {
			t.Fatal("advance must equal delivered bytes")
		}
	})
}

func FuzzDecodeMsg(f *testing.F) {
	f.Add([]byte(`{"t":"attach","have":1,"cols":80,"rows":24}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		m, err := DecodeMsg(b)
		if err == nil && m.T == "" {
			t.Fatal("decoded a message without a type")
		}
	})
}
