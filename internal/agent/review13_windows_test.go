//go:build windows

package agent

import (
	"termhub/internal/proto"
	"testing"
)

func TestReview13FilePanicContained(t *testing.T) {
	m := &proto.Msg{T: proto.MsgFsList, ID: 42}
	r := safeFileRequest(m, func(*proto.Msg) *proto.Msg { panic("bad page") })
	if r == nil || r.T != proto.MsgErr || r.Re != 42 || r.Code != proto.ErrInternal {
		t.Fatalf("bad response: %+v", r)
	}
	r = safeFileRequest(m, func(m *proto.Msg) *proto.Msg { return m.Reply() })
	if r.T != proto.MsgOK {
		t.Fatal("agent no longer serves requests")
	}
}
