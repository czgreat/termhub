package e2e

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

func jsonUnmarshal(b []byte, v any) { json.Unmarshal(b, v) }

// paste drives "paste into terminal" the way the browser will: announce, send
// chunks, finish, then type the returned text into the terminal.
func (w *world) paste(sid, name string, data []byte, isImage bool) (int, map[string]any) {
	w.t.Helper()
	code, out := w.call("POST", "/api/sessions/"+sid+"/paste-file", map[string]any{"name": name, "size": len(data), "image": isImage})
	if code != 200 {
		return code, out
	}
	uid, node := out["id"].(string), int(out["node_id"].(float64))
	c, body, _ := w.raw("PUT", fmt.Sprintf("/api/nodes/%d/fs/uploads/%s?offset=0", node, uid), data, nil)
	if c != 200 {
		var e map[string]any
		jsonUnmarshal(body, &e)
		return c, e
	}
	return w.call("POST", fmt.Sprintf("/api/nodes/%d/fs/uploads/%s/finish", node, uid), map[string]string{"sha256": hashOf(string(data))})
}

func (w *world) raw(method, path string, body []byte, hdr map[string]string) (int, []byte, http.Header) {
	w.t.Helper()
	req, _ := http.NewRequest(method, w.base+path, bytes.NewReader(body))
	req.Header.Set("Origin", w.base)
	req.Header.Set("X-TH-CSRF", w.csrf)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return 0, nil, nil // an aborted download surfaces here
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, b, resp.Header
	}
	return resp.StatusCode, b, resp.Header
}

func hashOf(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
