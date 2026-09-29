//go:build windows

package agent

import (
	"encoding/json"
	"errors"

	"termhub/internal/agent/fs"
	"termhub/internal/agent/history"
	"termhub/internal/proto"
	"termhub/internal/session"
)

// FileParams is the payload of fs_list, fs_stat and fs_mkdir requests.
type FileParams struct {
	Path   string `json:"path"`
	Page   int    `json:"page,omitempty"`
	Hidden bool   `json:"hidden,omitempty"`
	Name   string `json:"name,omitempty"` // fs_mkdir: the new folder's name
}

// A bad disk request must not take down every session's network link (#7 F01).
func safeFileRequest(m *proto.Msg, serve func(*proto.Msg) *proto.Msg) (reply *proto.Msg) {
	defer func() {
		if recover() != nil {
			reply = m.Fail(proto.ErrInternal, "file request failed unexpectedly")
		}
	}()
	return serve(m)
}

// fileRequest serves the browsing part of docs/M4. Every rule about paths and
// names is enforced by package fs, here on the node.
func (a *Agent) fileRequest(m *proto.Msg) *proto.Msg {
	if m.T == proto.MsgHistoryList || m.T == proto.MsgHistoryRead {
		return historyRequest(m)
	}
	if m.T == proto.MsgUsage {
		return usageRequest(m)
	}
	var p FileParams
	if len(m.Payload) > 0 && json.Unmarshal(m.Payload, &p) != nil {
		return m.Fail(proto.ErrBadRequest, "malformed parameters")
	}
	var result any
	var err error
	switch m.T {
	case proto.MsgFsList:
		if p.Path == "" {
			drives := map[string]any{"drives": fs.Drives()}
			if d := fs.Desktop(); d != "" { // older nodes leave it out and the picker shows no 桌面 button
				drives["desktop"] = d
			}
			result = drives
		} else {
			result, err = fs.List(p.Path, p.Page, p.Hidden, a.cfg.Files)
		}
	case proto.MsgFsStat:
		result, err = fs.Stat(p.Path, a.cfg.Files)
	case proto.MsgFsMkdir:
		var full string
		if full, err = fs.Mkdir(p.Path, p.Name, a.cfg.Files); err == nil {
			result = map[string]string{"path": full}
		}
	case proto.MsgClipboard: // an uploaded image onto this session's clipboard (docs/M4 第 7 节)
		if err = fs.ClipboardImage(a.cfg.DataDir, p.Path); err == nil {
			result = map[string]bool{"ok": true}
		}
	}
	if err != nil {
		var fe *fs.Error
		if errors.As(err, &fe) {
			return m.Fail(fe.Code, fe.Msg)
		}
		return m.Fail(proto.ErrInternal, err.Error())
	}
	r := m.Reply()
	r.Payload, _ = json.Marshal(result)
	return r
}

// historyBudget keeps an answer inside one control message (docs/M1 第 10 节)
// with room for the envelope.
const historyBudget = proto.MaxJSON - 8<<10

// historyRequest serves docs/M10 第 3.2 节: conversations of a CLI, read-only
// from its own files. Where they are is derived here from the CLI kind and the
// profile's environment; the Hub never names a path.
func historyRequest(m *proto.Msg) *proto.Msg {
	var p history.Params
	if json.Unmarshal(m.Payload, &p) != nil {
		return m.Fail(proto.ErrBadRequest, "malformed parameters")
	}
	// the folders are found the way a session finds them (问题单 1 第 5 条)
	p.Base = session.UserEnvironment()
	var payload []byte
	var err error
	if m.T == proto.MsgHistoryList {
		var l *history.List
		if l, err = history.Scan(p); err == nil {
			items := l.Items[:0]
			for _, it := range l.Items {
				if len(it.Cwd) <= maxField { // a folder longer than that is no folder a CLI ran in
					items = append(items, it)
				}
			}
			l.Items = items
			// too many to send at once: the oldest go, and the list says it is not complete
			for payload, _ = json.Marshal(l); len(payload) > historyBudget && len(l.Items) > 0; payload, _ = json.Marshal(l) {
				l.Items = l.Items[:len(l.Items)*3/4]
				l.Incomplete = true
			}
		}
	} else {
		// A transcript can hold megabytes of a file in memory: two at a time.
		historyReads <- struct{}{}
		defer func() { <-historyReads }()
		// Read once; a page too large to send loses its oldest messages
		// (they start the next page) rather than being read again smaller.
		var t *history.Transcript
		if t, err = history.Read(p); err == nil {
			trimTranscript(t)
			for payload, _ = json.Marshal(t); len(payload) > historyBudget && len(t.Messages) > 1; payload, _ = json.Marshal(t) {
				t.DropOldest((len(t.Messages) + 3) / 4)
			}
			if len(payload) > historyBudget && len(t.Messages) == 1 {
				t.Messages[0].Text = cut(t.Messages[0].Text, 4000) // one enormous message
				payload, _ = json.Marshal(t)
			}
		}
	}
	if err == nil && len(payload) > historyBudget {
		// say so, rather than an answer the link would refuse and the Hub wait for in vain
		return m.Fail(proto.ErrInternal, "the answer is too large to send")
	}
	if err != nil {
		var he *history.Error
		if errors.As(err, &he) {
			return m.Fail(he.Code, he.Msg)
		}
		return m.Fail(proto.ErrInternal, err.Error())
	}
	r := m.Reply()
	r.Payload = payload
	return r
}

// usageRequest totals what a running session's conversation used, from the
// CLI's own files, read-only (history.UsageOf). Like history, the folder is
// found from the CLI kind and the environment, never from a path sent.
func usageRequest(m *proto.Msg) *proto.Msg {
	var p history.UsageParams
	if json.Unmarshal(m.Payload, &p) != nil {
		return m.Fail(proto.ErrBadRequest, "malformed parameters")
	}
	p.Base = session.UserEnvironment()
	usageReads <- struct{}{} // its own slots: reading history is never held up by it (复核 N1)
	u, err := history.UsageOf(p)
	<-usageReads
	if err != nil {
		var he *history.Error
		if errors.As(err, &he) {
			return m.Fail(he.Code, he.Msg)
		}
		return m.Fail(proto.ErrInternal, err.Error())
	}
	if len(u.Conv) > maxField || len(u.Model) > maxField || len(u.Buckets) > 64 {
		return m.Fail(proto.ErrInternal, "usage answer out of bounds")
	}
	for _, b := range u.Buckets {
		if len(b.Model) > 256 {
			return m.Fail(proto.ErrInternal, "usage answer out of bounds")
		}
	}
	r := m.Reply()
	r.Payload, _ = json.Marshal(u)
	return r
}

// maxField bounds the fields of history answers that are not message text:
// they come from files anyone on the node can write (复核：单字段无上限).
const maxField = 4096

var historyReads = make(chan struct{}, 2)
var usageReads = make(chan struct{}, 2)

func trimTranscript(t *history.Transcript) {
	if len(t.Cwd) > maxField {
		t.Cwd = ""
	}
	t.Title = cut(t.Title, 300)
	for i := range t.Messages {
		m := &t.Messages[i]
		if len(m.Tools) > 50 {
			m.Tools = m.Tools[:50]
		}
		for j := range m.Tools {
			m.Tools[j] = cut(m.Tools[j], 100)
		}
	}
}

func cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
