package route

import (
	"context"
	"encoding/json"
	"termhub/internal/hub/auth"
	"termhub/internal/proto"
	"time"
)

var ErrEgress = &auth.Error{Code: "egress_blocked", Status: 409, Msg: "出口异常或结果过期，操作已暂停；终端仍保留"}

func (m wsMsg) permitted() bool {
	if m.check != nil && !m.check() {
		if m.rejected != nil {
			m.rejected()
		}
		return false
	}
	return true
}
func (h *Hub) egressEpoch(id int64) (uint64, error) {
	s := h.Egress.Status(id)
	if s == nil {
		return 0, nil
	}
	if !s.Allowed {
		return s.Epoch, ErrEgress
	}
	return s.Epoch, nil
}
func mutates(m *proto.Msg) bool {
	return m.T == proto.MsgCreate || m.T == proto.MsgFsMkdir || m.T == proto.MsgClipboard || (m.T == proto.MsgTransferOpen && (m.Dir == proto.DirUp || m.Dir == proto.DirPaste))
}
func (a *attachment) egressStatus() {
	a.egressMu.Lock()
	defer a.egressMu.Unlock()
	s := a.rt.hub.Egress.Status(a.rt.nodeID)
	b, _ := json.Marshal(s)
	if a.egressLast == string(b) {
		return
	}
	a.egressLast = string(b)
	a.sendMsg(&proto.Msg{T: "egress_status", Payload: b})
}

// Timer notifications are for the UI. Every operation independently checks expiry.
func (h *Hub) WatchEgress(ctx context.Context) {
	if h.Egress == nil {
		return
	}
	done := make(chan struct{})
	go func() { defer close(done); h.Egress.Run(ctx, func(id int64) { h.egressChanged(id) }) }()
	defer func() { <-done }()
	timer := time.NewTicker(time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			h.mu.Lock()
			routes := make([]*route, 0, len(h.routes))
			for _, rt := range h.routes {
				routes = append(routes, rt)
			}
			h.mu.Unlock()
			for _, rt := range routes {
				if s := h.Egress.Status(rt.nodeID); s != nil && !s.Allowed {
					rt.each(func(a *attachment) { a.egressStatus() })
				}
			}
		}
	}
}
func (h *Hub) egressChanged(id int64) {
	payload, _ := json.Marshal(map[string]any{"node_id": id, "status": h.Egress.Status(id)})
	h.broadcast(&proto.Msg{T: "egress_status", Payload: payload}, func(who *auth.Identity) bool { return h.reg.CanUseNode(who, id) })
	h.mu.Lock()
	var routes []*route
	for _, rt := range h.routes {
		if rt.nodeID == id {
			routes = append(routes, rt)
		}
	}
	h.mu.Unlock()
	for _, rt := range routes {
		rt.each(func(a *attachment) { a.egressStatus() })
	}
	// A ticket from a previous healthy generation can never resume on recovery.
	h.xfer.mu.Lock()
	var stale []string
	for uid, u := range h.xfer.uploads {
		if u.nodeID == id && !h.Egress.Allows(id, u.egressEpoch) {
			stale = append(stale, uid)
			u.ws.Close()
		}
	}
	h.xfer.mu.Unlock()
	for _, uid := range stale {
		h.endUpload(uid, false)
	}
}
