//go:build linux

package e2e

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"termhub/internal/hub/route"
)

// A Linux node end to end (2026-10-11): a profile shaped like the
// "Claude Code（Linux）" template (a login bash running the CLI), POSIX
// folders through the Hub's folder rules, the folder picker's calls, and a
// pasted image whose path reaches the program. The CLI is the test CLI;
// nothing here talks to a real CLI or account.
func TestLinuxNodeThroughTheWholeStack(t *testing.T) {
	w := newWorld(t)
	_, out := w.call("POST", "/api/admin/nodes", map[string]string{"name": "linux-pc"})
	w.startAgent(out["token"].(string))
	w.waitNode(true)
	_, out = w.call("GET", "/api/nodes", nil)
	if osb, _ := out["nodes"].([]any)[0].(map[string]any)["os_build"].(string); osb == "" {
		t.Fatalf("a Linux node names its system: %v", out)
	}

	root := t.TempDir()
	project := filepath.Join(root, "Project A")
	other := filepath.Join(root, "project a") // another folder on Linux: case matters
	os.Mkdir(project, 0o755)
	os.Mkdir(other, 0o755)

	code, out := w.call("POST", "/api/admin/profiles", route.Profile{NodeID: 1, Name: "cli", Kind: "claude", Mode: "shell",
		ShellPath: "/bin/bash", Command: testcli, Args: []string{"-title", "linux"}, DefaultCwd: project})
	if code != 200 {
		t.Fatalf("profile: %d %v", code, out)
	}
	pid := out["profile"].(map[string]any)["id"]

	// A user kept to one POSIX folder: accepted, and normalised.
	w.call("POST", "/api/admin/users", map[string]string{"username": "bob", "role": "user"})
	_, out = w.call("GET", "/api/admin/users", nil)
	var uid any
	for _, u := range out["users"].([]any) {
		if u.(map[string]any)["username"] == "bob" {
			uid = u.(map[string]any)["id"]
		}
	}
	code, out = w.call("PUT", fmt.Sprintf("/api/admin/profiles/%v/bindings", pid), map[string]any{
		"user_ids": []any{uid}, "folders": map[string][]string{fmt.Sprint(uid): {project + "/"}}})
	if code != 200 {
		t.Fatalf("POSIX folder range refused: %d %v", code, out)
	}
	_, out = w.call("GET", fmt.Sprintf("/api/admin/profiles/%v/bindings", pid), nil)
	if got := fmt.Sprint(out["folders"]); !strings.Contains(got, project+"]") {
		t.Fatalf("folder range not normalised: %v", got)
	}

	// The folder picker: one root, the home folder, a listing, a new folder.
	code, out = w.call("GET", "/api/nodes/1/fs/drives", nil)
	if d := out["drives"].([]any); code != 200 || len(d) != 1 || d[0].(map[string]any)["letter"] != "/" || out["desktop"] == "" {
		t.Fatalf("drives: %d %v", code, out)
	}
	code, out = w.call("POST", "/api/nodes/1/fs/mkdir", map[string]string{"parent": project, "name": "新 文件夹"})
	if code != 200 || out["path"] != project+"/新 文件夹" {
		t.Fatalf("mkdir: %d %v", code, out)
	}
	code, out = w.call("GET", "/api/nodes/1/fs/list?path="+url.QueryEscape(project), nil)
	if e, _ := out["entries"].([]any); code != 200 || len(e) != 1 || out["parent"] != root {
		t.Fatalf("list: %d %v", code, out)
	}

	// A session in the folder: through bash's -c, the CLI gets its arguments.
	code, out = w.call("POST", "/api/sessions", map[string]any{"profile_id": pid, "cwd": project, "cols": 100, "rows": 30})
	if code != 200 {
		t.Fatalf("create session: %d %v", code, out)
	}
	sid := out["session"].(map[string]any)["sid"].(string)
	v := w.view(sid)
	v.attach(false)
	v.waitText("READY")
	v.typeIn("echo 你好 linux\r")
	v.waitText(`ECHO "你好 linux"`)

	// One CLI of a kind per folder: the same folder written differently is
	// the same folder, the other-case folder is another one.
	if code, out := w.call("POST", "/api/sessions", map[string]any{"profile_id": pid, "cwd": project + "/./"}); code != 409 {
		t.Fatalf("second CLI in the same folder: %d %v", code, out)
	}
	if code, out := w.call("POST", "/api/sessions", map[string]any{"profile_id": pid, "cwd": other}); code != 200 {
		t.Fatalf("CLI in the other-case folder: %d %v", code, out)
	}

	// A pasted image: a POSIX path, typed in as one shell word.
	var img bytes.Buffer
	png.Encode(&img, image.NewRGBA(image.Rect(0, 0, 4, 4)))
	code, out = w.paste(sid, "shot.png", img.Bytes(), true)
	if code != 200 {
		t.Fatalf("paste: %d %v", code, out)
	}
	path, insert := out["path"].(string), out["insert"].(string)
	if !strings.HasPrefix(path, w.agentData+"/paste/"+sid+"/") || insert != path+" " || out["keys"] != nil {
		t.Fatalf("paste result: %v", out)
	}
	v.typeIn("echo " + insert + "\r")
	v.waitText(path)

	if code, _ := w.call("DELETE", "/api/sessions/"+sid, nil); code != 200 {
		t.Fatalf("close: %d", code)
	}
	if ex := v.wait("exited"); ex.Reason != "user" {
		t.Fatalf("exit: %+v", ex)
	}
}
