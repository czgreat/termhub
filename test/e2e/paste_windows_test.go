//go:build windows

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"termhub/internal/hub/route"
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

// Image paste end to end (docs/M4 第 7 节): the image lives in the browser's
// clipboard, so it is uploaded to the node and its path is typed into the CLI.
func TestPasteIntoTerminal(t *testing.T) {
	w := newWorld(t)
	_, out := w.call("POST", "/api/admin/nodes", map[string]string{"name": "this-pc"})
	w.startAgent(out["token"].(string))
	_, out = w.call("POST", "/api/admin/profiles", route.Profile{NodeID: 1, Name: "cli", Mode: "direct", Command: testcli, QuoteStyle: "double"})
	pid := out["profile"].(map[string]any)["id"]
	w.waitNode(true)
	_, out = w.call("POST", "/api/sessions", map[string]any{"profile_id": pid, "cwd": t.TempDir()})
	sid := out["session"].(map[string]any)["sid"].(string)
	v := w.view(sid)
	v.attach(false)
	v.waitText("READY")

	var img bytes.Buffer
	png.Encode(&img, image.NewRGBA(image.Rect(0, 0, 8, 8)))

	// A real PNG under a lying name: the content decides, and the extension follows it.
	code, out := w.paste(sid, "screenshot.bmp", img.Bytes(), true)
	if code != 200 {
		t.Fatalf("paste image: %d %v", code, out)
	}
	path, insert := out["path"].(string), out["insert"].(string)
	wantDir := filepath.Join(w.agentData, "paste", sid)
	if filepath.Dir(path) != wantDir || filepath.Ext(path) != ".png" {
		t.Fatalf("pasted file at %q, want a .png inside %q", path, wantDir)
	}
	if b, _ := os.ReadFile(path); !bytes.Equal(b, img.Bytes()) {
		t.Fatal("the pasted image differs from what was sent")
	}
	if insert != `"`+path+`" ` {
		t.Fatalf("text to type must follow the profile's quote style and end with a space, no Enter: %q", insert)
	}
	// The browser types that text; the program receives the path intact.
	v.typeIn("echo " + insert + "\r")
	v.waitText(strings.ReplaceAll(path, `\`, `\\`))

	// Not an image, whatever it calls itself.
	if code, out := w.paste(sid, "photo.png", []byte("MZ this is an executable"), true); code == 200 || out["code"] != "type_not_allowed" {
		t.Fatalf("a non-image pasted as an image: %d %v", code, out)
	}
	// Other files may be pasted as files and keep their extension.
	code, out = w.paste(sid, "notes 笔记.txt", []byte("hello"), false)
	if code != 200 || filepath.Ext(out["path"].(string)) != ".txt" {
		t.Fatalf("paste a file: %d %v", code, out)
	}
	if code, _ := w.call("POST", "/api/sessions/"+sid+"/paste-file", map[string]any{"name": "big.png", "size": 21 << 20, "image": true}); code == 200 {
		t.Fatal("an image over 20 MB was accepted")
	}
	if code, _ := w.call("POST", "/api/sessions/00000000-0000-7000-8000-000000000000/paste-file", map[string]any{"name": "x.png", "size": 10, "image": true}); code != 404 {
		t.Fatalf("paste into an unknown session: %d", code)
	}
	files, _ := os.ReadDir(wantDir)
	if len(files) != 2 {
		var names []string
		for _, f := range files {
			names = append(names, f.Name())
		}
		t.Fatalf("expected exactly the two successful pastes on disk, got %v", names)
	}

	// The session ends: its pasted files go with it.
	w.call("DELETE", "/api/sessions/"+sid+"?mode=force", nil)
	for end := time.Now().Add(15 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if _, err := os.Stat(wantDir); err != nil {
			return
		}
	}
	t.Fatal("the paste folder outlived its session")
}
