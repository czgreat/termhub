package route

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"termhub/internal/hub/auth"
	"termhub/internal/proto"
)

// Transfer limits (docs/M4 第 5 节, 总体设计 原 Q5).
const (
	MaxFileSize      = 2 << 30
	maxUserTransfers = 3
	maxNodeTransfers = 6
	uploadIdle       = time.Hour
)

var (
	ErrTooLarge      = &auth.Error{Code: "too_large", Msg: "文件超过大小上限", Status: 413}
	ErrTransferLimit = &auth.Error{Code: "transfer_limit", Msg: "同时进行的传输已达上限", Status: 429}
	ErrOffset        = &auth.Error{Code: "bad_offset", Msg: "分片偏移不对", Status: 409}
)

type transferTicket struct {
	nodeID int64
	conn   chan *websocket.Conn
}

type upload struct {
	mu     sync.Mutex
	ws     *websocket.Conn
	userID int64
	nodeID int64
	next   int64
	size   int64
	path   string
	timer  *time.Timer
	quote  string // set for "paste into terminal": how to quote the path when typing it
	kind   string // the profile kind of that session ("claude": images go through the clipboard)
	sid    string // nonempty for a paste: its permission and typist can change
}

// isImageFile: the node named the file by its sniffed content (fs.BeginPaste);
// only the formats the agent can decode for the clipboard count (no bmp/webp).
func isImageFile(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg", ".gif":
		return true
	}
	return false
}

// pasteText renders the path the way it is typed into the terminal: quoted per
// the CLI profile's style, followed by a space, never by Enter (docs/M4 第 7 节).
func pasteText(path, style string) string {
	switch style {
	case "none":
	case "double":
		path = `"` + path + `"`
	case "single":
		path = `'` + path + `'`
	default: // auto
		if strings.ContainsAny(path, " \t") {
			path = `"` + path + `"`
		}
	}
	return path + " "
}

type downloadToken struct {
	nodeID  int64
	userID  int64
	csrf    string // ties the link to the login session that asked for it
	path    string
	zip     bool // a folder, delivered as a zip stream
	expires time.Time
}

type transfers struct {
	mu        sync.Mutex
	tickets   map[string]*transferTicket
	uploads   map[string]*upload
	downloads map[string]*downloadToken
	active    map[int64]int // node id -> running transfers
	perUser   map[int64]int
}

func newTransfers() *transfers {
	return &transfers{tickets: map[string]*transferTicket{}, uploads: map[string]*upload{}, downloads: map[string]*downloadToken{},
		active: map[int64]int{}, perUser: map[int64]int{}}
}

func randomID() string {
	b := make([]byte, 24)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (t *transfers) acquire(userID, nodeID int64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.perUser[userID] >= maxUserTransfers || t.active[nodeID] >= maxNodeTransfers {
		return false
	}
	t.perUser[userID]++
	t.active[nodeID]++
	return true
}

func (t *transfers) release(userID, nodeID int64) {
	t.mu.Lock()
	t.perUser[userID]--
	t.active[nodeID]--
	t.mu.Unlock()
}

// openTransfer has the node open a dedicated connection and returns it after
// the node's first message, which is either its go-ahead or its precise refusal.
func (h *Hub) openTransfer(nodeID int64, m *proto.Msg) (*websocket.Conn, *proto.Transfer, error) {
	n := h.nodeConn(nodeID)
	if n == nil {
		return nil, nil, ErrNodeOffline
	}
	tk := &transferTicket{nodeID: nodeID, conn: make(chan *websocket.Conn, 1)}
	m.T, m.Ticket = proto.MsgTransferOpen, randomID()
	h.xfer.mu.Lock()
	h.xfer.tickets[m.Ticket] = tk
	h.xfer.mu.Unlock()
	defer func() { h.xfer.mu.Lock(); delete(h.xfer.tickets, m.Ticket); h.xfer.mu.Unlock() }()
	if !n.sendMsg(m) {
		return nil, nil, ErrNodeOffline
	}
	var ws *websocket.Conn
	select {
	case ws = <-tk.conn:
	case <-time.After(20 * time.Second):
		return nil, nil, &auth.Error{Code: "node_timeout", Msg: "节点没有建立传输连接", Status: 504}
	}
	first, err := readTransfer(ws, 30*time.Second)
	if err != nil {
		ws.Close()
		return nil, nil, err
	}
	return ws, first, nil
}

// readTransfer reads one control message; a node-side refusal becomes an error
// carrying the node's own code.
func readTransfer(ws *websocket.Conn, wait time.Duration) (*proto.Transfer, error) {
	ws.SetReadDeadline(time.Now().Add(wait))
	kind, data, err := ws.ReadMessage()
	if err != nil || kind != websocket.TextMessage {
		return nil, &auth.Error{Code: "transfer_broken", Msg: "传输连接中断", Status: 502}
	}
	var t proto.Transfer
	if json.Unmarshal(data, &t) != nil {
		return nil, &auth.Error{Code: "transfer_broken", Msg: "传输连接中断", Status: 502}
	}
	if t.T == proto.XferErr {
		st, ok := fileStatus[t.Code]
		if !ok {
			st = 409
		}
		return nil, &auth.Error{Code: t.Code, Msg: t.Msg, Status: st}
	}
	return &t, nil
}

// ServeTransfer handles /node/transfer/{ticket}: the node's side of a transfer.
func (h *Hub) ServeTransfer(w http.ResponseWriter, r *http.Request) {
	node, err := h.reg.AuthenticateNode(r.Header.Get("X-TH-Node-Token"))
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	h.xfer.mu.Lock()
	tk := h.xfer.tickets[r.PathValue("ticket")]
	delete(h.xfer.tickets, r.PathValue("ticket")) // single use
	h.xfer.mu.Unlock()
	if tk == nil || tk.nodeID != node.ID {
		http.Error(w, "unknown ticket", http.StatusNotFound)
		return
	}
	ws, err := h.up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(proto.MaxChunk + 1024)
	tk.conn <- ws
}

func (h *Hub) registerTransfers(mux *http.ServeMux) {
	mux.HandleFunc("GET /node/transfer/{ticket}", h.ServeTransfer)
	handle := func(pattern string, fn func(w http.ResponseWriter, r *http.Request, id *auth.Identity) error) {
		mux.Handle(pattern, h.web.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := fn(w, r, auth.IdentityFrom(r.Context())); err != nil {
				h.web.Fail(w, r, err)
			}
		})))
	}
	allowed := func(id *auth.Identity, r *http.Request) (int64, error) {
		nodeID, err := pathID(r, "id")
		if err != nil {
			return 0, err
		}
		if _, err := h.reg.node(nodeID); err != nil || !h.reg.CanUseNode(id, nodeID) {
			return 0, ErrNotFound
		}
		return nodeID, nil
	}

	handle("POST /api/nodes/{id}/fs/uploads", func(w http.ResponseWriter, r *http.Request, id *auth.Identity) error {
		nodeID, err := allowed(id, r)
		if err != nil {
			return err
		}
		var in struct {
			Dir, Name, Conflict string
			Size                int64
		}
		if err := readBody(w, r, &in); err != nil {
			return err
		}
		if in.Size < 0 || in.Size > MaxFileSize {
			return ErrTooLarge
		}
		if !h.xfer.acquire(id.User.ID, nodeID) {
			return ErrTransferLimit
		}
		ws, first, err := h.openTransfer(nodeID, &proto.Msg{Dir: proto.DirUp, Path: in.Dir, Name: in.Name, Size: in.Size, Mode: in.Conflict})
		if err != nil {
			h.xfer.release(id.User.ID, nodeID)
			h.web.S.Audit(&id.User, id.IP, "fs_upload", in.Dir+`\`+in.Name, errCode(err), "")
			return err
		}
		uid := randomID()
		u := &upload{ws: ws, userID: id.User.ID, nodeID: nodeID, size: in.Size, path: first.Path}
		u.timer = time.AfterFunc(uploadIdle, func() { h.endUpload(uid, true) })
		h.xfer.mu.Lock()
		h.xfer.uploads[uid] = u
		h.xfer.mu.Unlock()
		writeJSON(w, map[string]any{"id": uid, "path": first.Path, "next": 0, "chunk": proto.MaxChunk})
		return nil
	})

	find := func(id *auth.Identity, r *http.Request) (*upload, error) {
		h.xfer.mu.Lock()
		u := h.xfer.uploads[r.PathValue("uid")]
		h.xfer.mu.Unlock()
		if u == nil || u.userID != id.User.ID {
			return nil, ErrNotFound
		}
		nodeID, err := pathID(r, "id")
		if err != nil || nodeID != u.nodeID {
			return nil, ErrNotFound
		}
		if err := h.uploadAllowed(id, u); err != nil {
			h.endUpload(r.PathValue("uid"), true)
			return nil, err
		}
		return u, nil
	}

	handle("PUT /api/nodes/{id}/fs/uploads/{uid}", func(w http.ResponseWriter, r *http.Request, id *auth.Identity) error {
		u, err := find(id, r)
		if err != nil {
			return err
		}
		offset, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
		chunk, err := io.ReadAll(io.LimitReader(r.Body, proto.MaxChunk+1))
		if err != nil || len(chunk) == 0 || len(chunk) > proto.MaxChunk {
			return ErrBadRequest
		}
		u.mu.Lock()
		defer u.mu.Unlock()
		if err := h.uploadAllowed(id, u); err != nil {
			go h.endUpload(r.PathValue("uid"), true)
			return err
		}
		if offset != u.next { // the browser resumes from here (docs/M4 第 5 节)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]any{"code": ErrOffset.Code, "msg": ErrOffset.Msg, "next": u.next})
			return nil
		}
		u.timer.Reset(uploadIdle)
		u.ws.SetWriteDeadline(time.Now().Add(2 * time.Minute))
		if err := u.ws.WriteMessage(websocket.BinaryMessage, chunk); err != nil {
			go h.endUpload(r.PathValue("uid"), false)
			return &auth.Error{Code: "transfer_broken", Msg: "传输连接中断", Status: 502}
		}
		ack, err := readTransfer(u.ws, 2*time.Minute)
		if err != nil {
			go h.endUpload(r.PathValue("uid"), false)
			return err
		}
		u.next = ack.Next
		writeJSON(w, map[string]any{"next": u.next})
		return nil
	})

	handle("POST /api/nodes/{id}/fs/uploads/{uid}/finish", func(w http.ResponseWriter, r *http.Request, id *auth.Identity) error {
		u, err := find(id, r)
		if err != nil {
			return err
		}
		var in struct{ SHA256 string }
		if err := readBody(w, r, &in); err != nil {
			return err
		}
		u.mu.Lock()
		if err := h.uploadAllowed(id, u); err != nil {
			u.mu.Unlock()
			h.endUpload(r.PathValue("uid"), true)
			return err
		}
		b, _ := json.Marshal(proto.Transfer{T: proto.XferFinish, SHA256: in.SHA256})
		u.ws.SetWriteDeadline(time.Now().Add(2 * time.Minute))
		u.ws.WriteMessage(websocket.TextMessage, b)
		done, err := readTransfer(u.ws, 2*time.Minute)
		u.mu.Unlock()
		h.endUpload(r.PathValue("uid"), false)
		if err != nil {
			h.web.S.Audit(&id.User, id.IP, "fs_upload", u.path, errCode(err), "")
			return err
		}
		h.web.S.Audit(&id.User, id.IP, "fs_upload", done.Path, "ok", "size="+strconv.FormatInt(u.size, 10)+" sha256="+in.SHA256)
		out := map[string]any{"path": done.Path}
		if u.quote != "" {
			if err := h.uploadAllowed(id, u); err != nil {
				return err
			}
			// Claude Code no longer treats a pasted image path as an attachment
			// (verified 2026-09-23, v2.1.186), but it reads its own machine's
			// clipboard on Alt+V. So for images the node puts the file on the
			// clipboard of the session's window station and the browser sends
			// Alt+V; anything else, and any failure, falls back to typing the path.
			if u.kind == "claude" && isImageFile(done.Path) {
				if n := h.nodeConn(u.nodeID); n != nil {
					payload, _ := json.Marshal(map[string]string{"path": done.Path})
					if _, err := n.request(&proto.Msg{T: proto.MsgClipboard, Payload: payload}, 15*time.Second); err == nil {
						out["keys"] = "\x1bv"
						writeJSON(w, out)
						return nil
					}
				}
			}
			out["insert"] = pasteText(done.Path, u.quote)
		}
		writeJSON(w, out)
		return nil
	})

	// Paste into a terminal: the file goes into the session's own folder on the
	// node; the answer to finish carries the text to type into the terminal.
	handle("POST /api/sessions/{sid}/paste-file", func(w http.ResponseWriter, r *http.Request, id *auth.Identity) error {
		row, err := h.reg.session(r.PathValue("sid"))
		if err != nil || !h.reg.CanAccessSession(id, row) || row.EndedAt != 0 {
			return ErrNotFound
		}
		if row.OwnerID != id.User.ID && !id.Reverified { // as for looking into or ending someone else's session (D4)
			return auth.ErrReverifyRequired
		}
		if h.typist(row) != id.User.ID { // only the one at the keyboard pastes
			return ErrReadOnly
		}
		sid, _ := proto.ParseSID(row.SID)
		var in struct {
			Name  string
			Size  int64
			Image bool
		}
		if err := readBody(w, r, &in); err != nil {
			return err
		}
		if in.Size <= 0 || in.Size > MaxFileSize {
			return ErrTooLarge
		}
		style, kind := "auto", ""
		h.reg.db.QueryRow(`SELECT quote_style, kind FROM cli_profiles WHERE id=?`, row.ProfileID).Scan(&style, &kind)
		if !h.xfer.acquire(id.User.ID, row.NodeID) {
			return ErrTransferLimit
		}
		mode := "file"
		if in.Image {
			mode = "image"
		}
		ws, _, err := h.openTransfer(row.NodeID, &proto.Msg{Dir: proto.DirPaste, SID: &sid, Name: in.Name, Size: in.Size, Mode: mode})
		if err != nil {
			h.xfer.release(id.User.ID, row.NodeID)
			return err
		}
		uid := randomID()
		u := &upload{ws: ws, userID: id.User.ID, nodeID: row.NodeID, size: in.Size, path: "paste:" + row.SID, quote: style, kind: kind, sid: row.SID}
		u.timer = time.AfterFunc(uploadIdle, func() { h.endUpload(uid, true) })
		h.xfer.mu.Lock()
		h.xfer.uploads[uid] = u
		h.xfer.mu.Unlock()
		writeJSON(w, map[string]any{"id": uid, "node_id": row.NodeID, "next": 0, "chunk": proto.MaxChunk})
		return nil
	})

	handle("DELETE /api/nodes/{id}/fs/uploads/{uid}", func(w http.ResponseWriter, r *http.Request, id *auth.Identity) error {
		if _, err := find(id, r); err != nil {
			return err
		}
		h.endUpload(r.PathValue("uid"), true)
		writeJSON(w, map[string]bool{"ok": true})
		return nil
	})

	handle("POST /api/nodes/{id}/fs/downloads", func(w http.ResponseWriter, r *http.Request, id *auth.Identity) error {
		nodeID, err := allowed(id, r)
		if err != nil {
			return err
		}
		var in struct {
			Path   string
			Folder bool
		}
		if err := readBody(w, r, &in); err != nil {
			return err
		}
		// Ask the node now, so a bad path fails here and not inside the download.
		if _, err := h.nodeFile(id, nodeID, proto.MsgFsStat, map[string]any{"path": in.Path}); err != nil {
			return err
		}
		tok := randomID()
		h.xfer.mu.Lock()
		for k, d := range h.xfer.downloads {
			if time.Now().After(d.expires) {
				delete(h.xfer.downloads, k)
			}
		}
		h.xfer.downloads[tok] = &downloadToken{nodeID: nodeID, userID: id.User.ID, csrf: id.CSRF, path: in.Path, zip: in.Folder, expires: time.Now().Add(60 * time.Second)}
		h.xfer.mu.Unlock()
		writeJSON(w, map[string]string{"url": "/api/dl/" + tok})
		return nil
	})

	handle("GET /api/dl/{token}", func(w http.ResponseWriter, r *http.Request, id *auth.Identity) error {
		h.xfer.mu.Lock()
		d := h.xfer.downloads[r.PathValue("token")]
		delete(h.xfer.downloads, r.PathValue("token")) // one use
		h.xfer.mu.Unlock()
		// Bound to the login session that created it: a forwarded link is worthless.
		if d == nil || time.Now().After(d.expires) || d.userID != id.User.ID || d.csrf != id.CSRF {
			return ErrNotFound
		}
		if !h.reg.CanUseNode(id, d.nodeID) {
			return ErrNotFound
		}
		var from *uint64
		if rg := r.Header.Get("Range"); strings.HasPrefix(rg, "bytes=") && strings.HasSuffix(rg, "-") {
			if n, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(rg, "bytes="), "-"), 10, 64); err == nil {
				from = &n
			}
		}
		if !h.xfer.acquire(id.User.ID, d.nodeID) {
			return ErrTransferLimit
		}
		defer h.xfer.release(id.User.ID, d.nodeID)
		dir := proto.DirDown
		if d.zip { // a stream of unknown length cannot be resumed
			dir, from = proto.DirZip, nil
		}
		ws, meta, err := h.openTransfer(d.nodeID, &proto.Msg{Dir: dir, Path: d.path, From: from})
		if err != nil {
			h.web.S.Audit(&id.User, id.IP, "fs_download", d.path, errCode(err), "")
			return err
		}
		defer ws.Close()
		start := int64(0)
		if from != nil {
			start = int64(*from)
		}
		hd := w.Header()
		hd.Set("Content-Type", "application/octet-stream")
		hd.Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(meta.Name)) // never rendered as a page
		hd.Set("X-Content-Type-Options", "nosniff")
		if meta.Size >= 0 {
			hd.Set("Accept-Ranges", "bytes")
			hd.Set("Content-Length", strconv.FormatInt(meta.Size-start, 10))
		}
		if from != nil {
			hd.Set("Content-Range", "bytes "+strconv.FormatInt(start, 10)+"-"+strconv.FormatInt(meta.Size-1, 10)+"/"+strconv.FormatInt(meta.Size, 10))
			w.WriteHeader(http.StatusPartialContent)
		}
		flusher, _ := w.(http.Flusher)
		sent, result := int64(0), "aborted"
		for {
			ws.SetReadDeadline(time.Now().Add(2 * time.Minute))
			kind, data, err := ws.ReadMessage()
			if err != nil {
				break
			}
			if kind == websocket.BinaryMessage {
				// Nothing is buffered here: a slow browser slows this write, which
				// slows the read from the node. The Hub's disk is never touched.
				if _, err := w.Write(data); err != nil {
					break
				}
				sent += int64(len(data))
				if flusher != nil {
					flusher.Flush()
				}
				continue
			}
			var t proto.Transfer
			if json.Unmarshal(data, &t) == nil && t.T == proto.XferDone {
				result = "ok"
			} else if t.T == proto.XferErr {
				result = t.Code
			}
			break
		}
		h.web.S.Audit(&id.User, id.IP, "fs_download", d.path, result, "bytes="+strconv.FormatInt(sent, 10))
		if result != "ok" {
			// Headers are gone already; cutting the connection makes the browser
			// see a failed download instead of a short file that looks complete.
			panic(http.ErrAbortHandler)
		}
		return nil
	})
}

// Every chunk/commit uses current authorization, not just the upload ticket
// created earlier. In particular a paste belongs to the current typist (#7 F06).
func (h *Hub) uploadAllowed(id *auth.Identity, u *upload) error {
	if !h.reg.CanUseNode(id, u.nodeID) {
		return ErrNotFound
	}
	if u.sid != "" {
		row, err := h.reg.session(u.sid)
		if err != nil || row.NodeID != u.nodeID || row.EndedAt != 0 || !h.reg.CanAccessSession(id, row) {
			return ErrNotFound
		}
		if row.OwnerID != id.User.ID && !id.Reverified {
			return auth.ErrReverifyRequired
		}
		if h.typist(row) != id.User.ID {
			return ErrReadOnly
		}
	}
	return nil
}

// endUpload closes an upload's connection; with cancel the node is told to
// discard what it has. Either way the node removes its temporary file unless
// the transfer finished.
func (h *Hub) endUpload(uid string, cancel bool) {
	h.xfer.mu.Lock()
	u := h.xfer.uploads[uid]
	delete(h.xfer.uploads, uid)
	h.xfer.mu.Unlock()
	if u == nil {
		return
	}
	u.timer.Stop()
	u.mu.Lock() // never write the connection concurrently with a PUT or finish
	if cancel {
		u.ws.SetWriteDeadline(time.Now().Add(wsWriteWait))
		b, _ := json.Marshal(proto.Transfer{T: proto.XferCancel})
		u.ws.WriteMessage(websocket.TextMessage, b)
	}
	u.ws.Close()
	u.mu.Unlock()
	h.xfer.release(u.userID, u.nodeID)
}
