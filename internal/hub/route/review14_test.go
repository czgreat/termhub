package route

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/gorilla/websocket"
	"termhub/internal/proto"
)

func TestReview14OutputDropPreservesControls(t *testing.T) {
	for _, limit := range []int64{16, 120} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			w := &wsConn{out: make(chan wsMsg, 16), done: make(chan struct{}), limit: limit}
			a := &attachment{ws: w}
			a.sendMsg(&proto.Msg{T: proto.MsgInputResult, Re: 7})
			a.deliver(0, bytes.Repeat([]byte("x"), 64))
			a.sendMsg(&proto.Msg{T: proto.MsgDriver})
			a.deliver(64, bytes.Repeat([]byte("y"), 64))
			var kinds []string
			for len(w.out) > 0 {
				m := <-w.out
				if m.kind != websocket.TextMessage {
					t.Fatal("output was not dropped")
				}
				var p proto.Msg
				if err := json.Unmarshal(m.data, &p); err != nil {
					t.Fatal(err)
				}
				kinds = append(kinds, p.T)
				if p.T == proto.MsgInputResult && p.Re != 7 {
					t.Fatal("wrong batch")
				}
			}
			want := []string{proto.MsgInputResult, proto.MsgDriver, proto.MsgGap}
			if limit == 16 {
				want = []string{proto.MsgInputResult, proto.MsgGap, proto.MsgDriver, proto.MsgGap}
			}
			if !reflect.DeepEqual(kinds, want) {
				t.Fatalf("controls lost or reordered: %v", kinds)
			}
		})
	}
}

func TestReview14ControlBudgetAndFIFO(t *testing.T) {
	w := &wsConn{out: make(chan wsMsg, 4), done: make(chan struct{}), limit: 16}
	if !w.send(websocket.TextMessage, []byte("attached")) || !w.send(websocket.BinaryMessage, []byte("out")) || !w.send(websocket.TextMessage, []byte("replay_end")) {
		t.Fatal("enqueue")
	}
	for _, want := range []string{"attached", "out", "replay_end"} {
		m, ok := w.take()
		if !ok || string(m.data) != want {
			t.Fatalf("FIFO %q %v", m.data, ok)
		}
	}
	if w.queued.Load() != 0 || w.controlBytes != 0 {
		t.Fatal("accounting not drained")
	}
	for i := 0; i < cap(w.out); i++ {
		if !w.send(websocket.TextMessage, []byte("control")) {
			t.Fatal("control slot")
		}
	}
	a := &attachment{ws: w}
	a.sendMsg(&proto.Msg{T: proto.MsgInputResult, Re: 9})
	select {
	case <-w.done:
	default:
		t.Fatal("control saturation must close connection")
	}
	w = &wsConn{out: make(chan wsMsg, 4), done: make(chan struct{}), limit: 16}
	if !w.send(websocket.TextMessage, make([]byte, wsControlLimit)) {
		t.Fatal("reserved bytes unavailable")
	}
	if w.send(websocket.TextMessage, []byte("x")) {
		t.Fatal("control byte budget exceeded")
	}
}
