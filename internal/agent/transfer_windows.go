//go:build windows

package agent

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"termhub/internal/agent/fs"
	"termhub/internal/proto"
)

// chunkWriter turns a stream of writes into binary messages. Each write blocks
// until the message is on the wire, so a slow browser paces the zip writer.
type chunkWriter struct{ ws *websocket.Conn }

func (c *chunkWriter) Write(p []byte) (int, error) {
	for off := 0; off < len(p); off += 256 << 10 {
		end := min(off+256<<10, len(p))
		c.ws.SetWriteDeadline(time.Now().Add(2 * time.Minute))
		if err := c.ws.WriteMessage(websocket.BinaryMessage, p[off:end]); err != nil {
			return off, err
		}
	}
	return len(p), nil
}

// transfer serves one file transfer on its own connection, so file data never
// competes with terminal traffic on the main link (docs/M1 2.2, 第 8 节).
func (a *Agent) transfer(m *proto.Msg) {
	url := "wss" + strings.TrimPrefix(a.cfg.HubURL, "https") + "/node/transfer/" + m.Ticket
	d := websocket.Dialer{TLSClientConfig: a.cfg.TLS, HandshakeTimeout: 15 * time.Second, Proxy: nil}
	ws, _, err := d.Dial(url, http.Header{"X-TH-Node-Token": {a.cfg.Token}})
	if err != nil {
		a.cfg.Log.Warn("cannot open the transfer connection", "err", err)
		return
	}
	defer ws.Close()
	ws.SetReadLimit(proto.MaxChunk + 1024)
	send := func(t proto.Transfer) error {
		b, _ := json.Marshal(t)
		ws.SetWriteDeadline(time.Now().Add(30 * time.Second))
		return ws.WriteMessage(websocket.TextMessage, b)
	}
	refuse := func(err error) {
		var fe *fs.Error
		if errors.As(err, &fe) {
			send(proto.Transfer{T: proto.XferErr, Code: fe.Code, Msg: fe.Msg})
		} else {
			send(proto.Transfer{T: proto.XferErr, Code: proto.ErrInternal, Msg: err.Error()})
		}
		time.Sleep(100 * time.Millisecond) // let the message leave before the close
	}
	switch m.Dir {
	case proto.DirUp, proto.DirPaste:
		var up *fs.Upload
		var err error
		if m.Dir == proto.DirPaste { // into the session's own paste folder (docs/M4 第 7 节)
			if m.SID == nil {
				refuse(&fs.Error{Code: fs.CodePathInvalid, Msg: "paste needs a session"})
				return
			}
			up, err = fs.BeginPaste(a.cfg.DataDir, m.SID.String(), m.Name, m.Size, m.Mode == "image")
		} else {
			up, err = fs.BeginUpload(m.Path, m.Name, m.Size, m.Mode, a.cfg.Files)
		}
		if err != nil {
			refuse(err)
			return
		}
		defer up.Abort() // anything but a clean finish removes the temporary file
		if send(proto.Transfer{T: proto.XferReady, Path: up.Final}) != nil {
			return
		}
		for {
			ws.SetReadDeadline(time.Now().Add(10 * time.Minute))
			kind, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if kind == websocket.BinaryMessage {
				if err := up.Write(up.Received(), data); err != nil {
					up.Abort() // the partial file goes first, the explanation second
					refuse(err)
					return
				}
				if send(proto.Transfer{T: proto.XferAck, Next: up.Received()}) != nil {
					return
				}
				continue
			}
			var t proto.Transfer
			if json.Unmarshal(data, &t) != nil || t.T == proto.XferCancel {
				return
			}
			if t.T == proto.XferFinish {
				path, err := up.Finish(t.SHA256)
				if err != nil {
					refuse(err)
					return
				}
				send(proto.Transfer{T: proto.XferDone, Path: path})
				time.Sleep(100 * time.Millisecond)
				return
			}
		}
	case proto.DirZip: // a whole folder, zipped on the fly; no temporary archive on disk
		plan, err := fs.PlanZip(m.Path, a.cfg.Files)
		if err != nil {
			refuse(err)
			return
		}
		if send(proto.Transfer{T: proto.XferMeta, Name: plan.Name, Size: -1}) != nil {
			return
		}
		if err := plan.Write(&chunkWriter{ws: ws}); err != nil {
			refuse(err)
			return
		}
		send(proto.Transfer{T: proto.XferDone})
		time.Sleep(100 * time.Millisecond)
	case proto.DirDown:
		dl, err := fs.OpenDownload(m.Path, a.cfg.Files)
		if err != nil {
			refuse(err)
			return
		}
		defer dl.Close()
		offset := int64(0)
		if m.From != nil {
			offset = int64(*m.From)
		}
		if offset < 0 || offset > dl.Size {
			refuse(&fs.Error{Code: fs.CodePathInvalid, Msg: "offset beyond the end of the file"})
			return
		}
		if send(proto.Transfer{T: proto.XferMeta, Name: dl.Name, Size: dl.Size}) != nil {
			return
		}
		buf := make([]byte, 256<<10)
		for offset < dl.Size {
			n, err := dl.ReadAt(buf[:min(int64(len(buf)), dl.Size-offset)], offset)
			if n > 0 {
				ws.SetWriteDeadline(time.Now().Add(2 * time.Minute))
				if ws.WriteMessage(websocket.BinaryMessage, buf[:n]) != nil {
					return // the browser went away; TCP backpressure paced everything up to here
				}
				offset += int64(n)
			}
			if err != nil && err != io.EOF {
				refuse(err)
				return
			}
			if n == 0 {
				break
			}
		}
		if !dl.Unchanged() || offset != dl.Size {
			refuse(&fs.Error{Code: fs.CodeChanged, Msg: "the file changed while it was being sent"})
			return
		}
		send(proto.Transfer{T: proto.XferDone})
		time.Sleep(100 * time.Millisecond)
	}
}
