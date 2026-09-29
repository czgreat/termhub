//go:build windows

package e2e

import (
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"termhub/internal/hub/route"
)

// "New project" as the browser does it (docs/M10 第 2 节): look at the drives,
// browse to a folder, create a folder in it, start a CLI inside. All of it
// through Hub -> agent -> this machine's real file system.
func TestNewProjectFlow(t *testing.T) {
	w := newWorld(t)
	_, out := w.call("POST", "/api/admin/nodes", map[string]string{"name": "this-pc"})
	token := out["token"].(string)
	_, out = w.call("POST", "/api/admin/profiles", route.Profile{NodeID: 1, Name: "testcli", Mode: "direct", Command: testcli})
	pid := out["profile"].(map[string]any)["id"]
	w.startAgent(token)
	w.waitNode(true)

	code, out := w.call("GET", "/api/nodes/1/fs/drives", nil)
	drives, _ := out["drives"].([]any)
	if code != 200 || len(drives) == 0 {
		t.Fatalf("drives: %d %v", code, out)
	}
	if d, _ := out["desktop"].(string); d == "" { // the picker's 桌面 button
		t.Fatalf("drives without the desktop: %v", out)
	}

	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "notes.txt"), []byte("x"), 0o644)
	q := func(p string) string { return url.QueryEscape(p) }
	code, out = w.call("GET", "/api/nodes/1/fs/list?path="+q(root), nil)
	if entries, _ := out["entries"].([]any); code != 200 || len(entries) != 1 || entries[0].(map[string]any)["name"] != "notes.txt" {
		t.Fatalf("list: %d %v", code, out)
	}

	code, out = w.call("POST", "/api/nodes/1/fs/mkdir", map[string]string{"parent": root, "name": "我的新项目"})
	if code != 200 {
		t.Fatalf("mkdir: %d %v", code, out)
	}
	project := out["path"].(string)
	if fi, err := os.Stat(project); err != nil || !fi.IsDir() {
		t.Fatalf("the folder was not really created: %v", err)
	}

	// The node's rules and codes arrive unchanged, with a fitting HTTP status.
	for _, c := range []struct {
		name, parent, folder, wantCode string
		wantStatus                     int
	}{
		{"again", root, "我的新项目", "exists", 409},
		{"reserved name", root, "CON", "name_invalid", 400},
		{"name that is a path", root, `..\escape`, "name_invalid", 400},
		{"device path", `\\.\C:\`, "x", "path_invalid", 400},
		{"relative parent", `some\where`, "x", "path_invalid", 400},
		{"network path", `\\server\share`, "x", "unc_disabled", 403},
		{"missing parent", filepath.Join(root, "nope"), "x", "not_found", 404},
	} {
		code, out := w.call("POST", "/api/nodes/1/fs/mkdir", map[string]string{"parent": c.parent, "name": c.folder})
		if code != c.wantStatus || out["code"] != c.wantCode {
			t.Errorf("%s: got %d %v, want %d %s", c.name, code, out["code"], c.wantStatus, c.wantCode)
		}
	}
	if code, _ := w.call("GET", "/api/nodes/99/fs/drives", nil); code != 404 {
		t.Errorf("unknown node: %d", code)
	}

	// Start the CLI inside the new project folder.
	code, out = w.call("POST", "/api/sessions", map[string]any{"profile_id": pid, "cwd": project})
	if code != 200 {
		t.Fatalf("session in the new folder: %d %v", code, out)
	}
	sid := out["session"].(map[string]any)["sid"].(string)
	v := w.view(sid)
	v.attach(false)
	v.waitText("READY")
	w.call("DELETE", "/api/sessions/"+sid+"?mode=force", nil)

	// A folder that does not exist is refused by the node, and the reason reaches the browser.
	code, out = w.call("POST", "/api/sessions", map[string]any{"profile_id": pid, "cwd": filepath.Join(root, "missing")})
	if code == 200 || out["code"] != "cwd_not_found" {
		t.Fatalf("missing cwd: %d %v", code, out)
	}

	// Every change made through the file functions is in the audit log, failures included.
	var ok, failed int
	w.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE event='fs_mkdir' AND result='ok'`).Scan(&ok)
	w.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE event='fs_mkdir' AND result<>'ok'`).Scan(&failed)
	if ok != 1 || failed != 7 {
		t.Fatalf("audit: %d ok, %d failed", ok, failed)
	}
}
