//go:build windows

package e2e

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"termhub/internal/hub/route"
)

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

// upload drives the chunked upload API the way the browser will.
func (w *world) upload(dir, name string, data []byte, conflict, sha string) (int, map[string]any) {
	w.t.Helper()
	code, out := w.call("POST", "/api/nodes/1/fs/uploads", map[string]any{"dir": dir, "name": name, "size": len(data), "conflict": conflict})
	if code != 200 {
		return code, out
	}
	uid := out["id"].(string)
	chunk := int(out["chunk"].(float64))
	for off := 0; off < len(data); off += chunk {
		end := min(off+chunk, len(data))
		c, body, _ := w.raw("PUT", fmt.Sprintf("/api/nodes/1/fs/uploads/%s?offset=%d", uid, off), data[off:end], nil)
		if c != 200 {
			w.t.Fatalf("chunk at %d: %d %s", off, c, body)
		}
	}
	return w.call("POST", "/api/nodes/1/fs/uploads/"+uid+"/finish", map[string]string{"sha256": sha})
}

func leftovers(t *testing.T, dir string) []string {
	var out []string
	es, _ := os.ReadDir(dir)
	for _, e := range es {
		if strings.HasSuffix(e.Name(), ".thpart") {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestUploadAndDownload(t *testing.T) {
	w := newWorld(t)
	_, out := w.call("POST", "/api/admin/nodes", map[string]string{"name": "this-pc"})
	w.startAgent(out["token"].(string))
	w.call("POST", "/api/admin/profiles", route.Profile{NodeID: 1, Name: "p", Mode: "direct", Command: testcli})
	w.waitNode(true)

	dir := t.TempDir()
	data := make([]byte, 3<<20+12345) // several chunks plus a ragged tail
	rand.Read(data)
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])

	code, out := w.upload(dir, "报告 final.bin", data, "fail", sha)
	if code != 200 {
		t.Fatalf("upload: %d %v", code, out)
	}
	got, err := os.ReadFile(out["path"].(string))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("the uploaded file differs from what was sent: %v", err)
	}

	// Conflict policies.
	if code, out := w.upload(dir, "报告 final.bin", data, "fail", sha); code != 409 || out["code"] != "exists" {
		t.Fatalf("existing name with policy fail: %d %v", code, out)
	}
	if code, out := w.upload(dir, "报告 final.bin", []byte("v2"), "rename", hashOf("v2")); code != 200 || filepath.Base(out["path"].(string)) != "报告 final (1).bin" {
		t.Fatalf("policy rename: %d %v", code, out)
	}
	if code, _ := w.upload(dir, "报告 final.bin", []byte("replaced"), "overwrite", hashOf("replaced")); code != 200 {
		t.Fatalf("policy overwrite: %d", code)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "报告 final.bin")); string(b) != "replaced" {
		t.Fatalf("overwrite did not replace: %q", b)
	}

	// A wrong hash: refused, the original stays intact, no temporary file remains.
	if code, out := w.upload(dir, "报告 final.bin", []byte("corrupted"), "overwrite", hashOf("something else")); code == 200 || out["code"] != "hash_mismatch" {
		t.Fatalf("hash mismatch: %d %v", code, out)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "报告 final.bin")); string(b) != "replaced" {
		t.Fatalf("a failed overwrite damaged the original: %q", b)
	}

	// A chunk at the wrong offset tells the browser where to resume; cancelling removes the partial file.
	_, out = w.call("POST", "/api/nodes/1/fs/uploads", map[string]any{"dir": dir, "name": "partial.bin", "size": 100})
	uid := out["id"].(string)
	w.raw("PUT", "/api/nodes/1/fs/uploads/"+uid+"?offset=0", make([]byte, 40), nil)
	c, body, _ := w.raw("PUT", "/api/nodes/1/fs/uploads/"+uid+"?offset=10", make([]byte, 10), nil)
	var resume map[string]any
	json.Unmarshal(body, &resume)
	if c != 409 || resume["next"] != float64(40) {
		t.Fatalf("wrong offset: %d %s", c, body)
	}
	if len(leftovers(t, dir)) != 1 {
		t.Fatalf("expected one partial file while uploading: %v", leftovers(t, dir))
	}
	w.call("DELETE", "/api/nodes/1/fs/uploads/"+uid, nil)
	for i := 0; i < 50 && len(leftovers(t, dir)) > 0; i++ {
		waitABit()
	}
	if l := leftovers(t, dir); len(l) != 0 {
		t.Fatalf("temporary files left behind: %v", l)
	}
	if _, err := os.Stat(filepath.Join(dir, "partial.bin")); err == nil {
		t.Fatal("a cancelled upload appeared under its real name")
	}

	// Rules are the node's: bad names and paths never get as far as a file.
	for _, c := range []struct{ dir, name, want string }{{dir, "CON", "name_invalid"}, {dir, `a\b`, "name_invalid"}, {`\\.\C:\`, "x", "path_invalid"}, {filepath.Join(dir, "nope"), "x", "not_found"}} {
		if code, out := w.call("POST", "/api/nodes/1/fs/uploads", map[string]any{"dir": c.dir, "name": c.name, "size": 1}); code == 200 || out["code"] != c.want {
			t.Errorf("upload to %q / %q: %d %v", c.dir, c.name, code, out)
		}
	}
	if code, out := w.call("POST", "/api/nodes/1/fs/uploads", map[string]any{"dir": dir, "name": "huge", "size": int64(3) << 30}); code != 413 {
		t.Errorf("over the size limit: %d %v", code, out)
	}

	// Download: whole, then resumed with Range.
	big := filepath.Join(dir, "下载 me.bin")
	os.WriteFile(big, data, 0o644)
	_, out = w.call("POST", "/api/nodes/1/fs/downloads", map[string]string{"path": big})
	link := out["url"].(string)
	code, body, hdr := w.raw("GET", link, nil, nil)
	if code != 200 || !bytes.Equal(body, data) {
		t.Fatalf("download: %d, %d bytes", code, len(body))
	}
	if !strings.HasPrefix(hdr.Get("Content-Disposition"), "attachment;") || hdr.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("a download must never be rendered as a page: %v", hdr)
	}
	if code, _, _ := w.raw("GET", link, nil, nil); code != 404 {
		t.Fatalf("a download link must work once: %d", code)
	}
	_, out = w.call("POST", "/api/nodes/1/fs/downloads", map[string]string{"path": big})
	code, body, hdr = w.raw("GET", out["url"].(string), nil, map[string]string{"Range": "bytes=1000000-"})
	if code != 206 || !bytes.Equal(body, data[1000000:]) || !strings.HasPrefix(hdr.Get("Content-Range"), "bytes 1000000-") {
		t.Fatalf("ranged download: %d, %d bytes, %q", code, len(body), hdr.Get("Content-Range"))
	}
	if code, out := w.call("POST", "/api/nodes/1/fs/downloads", map[string]string{"path": filepath.Join(dir, "missing")}); code != 404 {
		t.Fatalf("download of a missing file: %d %v", code, out)
	}

	// Everything is in the audit log, with size and hash for uploads.
	var ups, downs int
	w.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE event='fs_upload' AND result='ok' AND detail LIKE '%sha256=%'`).Scan(&ups)
	w.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE event='fs_download' AND result='ok'`).Scan(&downs)
	if ups != 3 || downs != 2 {
		t.Fatalf("audit: %d uploads, %d downloads", ups, downs)
	}
}

func waitABit() { time.Sleep(100 * time.Millisecond) }

func hashOf(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
