//go:build windows

package e2e

import (
	"archive/zip"
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	agentfs "termhub/internal/agent/fs"
	"termhub/internal/hub/route"
)

// A whole folder as one download (docs/M4 验收 7).
func TestFolderDownload(t *testing.T) {
	w := newWorld(t)
	_, out := w.call("POST", "/api/admin/nodes", map[string]string{"name": "this-pc"})
	w.startAgent(out["token"].(string))
	w.call("POST", "/api/admin/profiles", route.Profile{NodeID: 1, Name: "p", Mode: "direct", Command: testcli})
	w.waitNode(true)

	root := filepath.Join(t.TempDir(), "我的项目")
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("must never be in the archive"), 0o644)
	want := map[string]string{
		"README.md":        "# 项目\n",
		"src/main.go":      "package main\n",
		"src/深层/数据 文件.txt": strings.Repeat("数据", 50000),
	}
	for rel, content := range want {
		full := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(content), 0o644)
	}
	os.MkdirAll(filepath.Join(root, "empty-dir"), 0o755)
	// A junction pointing out of the folder: must be skipped and reported, never followed.
	if b, err := exec.Command("cmd", "/c", "mklink", "/J", filepath.Join(root, "escape"), outside).CombinedOutput(); err != nil {
		t.Fatalf("mklink: %v %s", err, b)
	}

	code, out := w.call("POST", "/api/nodes/1/fs/downloads", map[string]any{"path": root, "folder": true})
	if code != 200 {
		t.Fatalf("request folder download: %d %v", code, out)
	}
	code, body, hdr := w.raw("GET", out["url"].(string), nil, nil)
	if code != 200 || !strings.Contains(hdr.Get("Content-Disposition"), "%E6%88%91%E7%9A%84%E9%A1%B9%E7%9B%AE.zip") {
		t.Fatalf("folder download: %d %q", code, hdr.Get("Content-Disposition"))
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("not a valid zip: %v", err)
	}
	got := map[string]string{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		got[f.Name] = string(b)
	}
	for rel, content := range want {
		if got[rel] != content {
			t.Errorf("%s: content differs (%d vs %d bytes)", rel, len(got[rel]), len(content))
		}
	}
	if _, ok := got["empty-dir/"]; !ok {
		t.Error("an empty folder must survive the round trip")
	}
	for name, content := range got {
		if strings.Contains(name, "escape") || strings.Contains(content, "must never be in the archive") {
			t.Fatalf("the archive reached outside the folder through a link: %s", name)
		}
	}
	if !strings.Contains(got["_termhub_skipped.txt"], "escape: link, not followed") {
		t.Errorf("the skipped link must be reported inside the archive: %q", got["_termhub_skipped.txt"])
	}

	// Over a limit: refused before anything is sent, with the node's code.
	oldEntries := agentfs.MaxZipEntries
	agentfs.MaxZipEntries = 2
	defer func() { agentfs.MaxZipEntries = oldEntries }()
	_, out = w.call("POST", "/api/nodes/1/fs/downloads", map[string]any{"path": root, "folder": true})
	if code, _, _ := w.raw("GET", out["url"].(string), nil, nil); code != 409 && code != 413 {
		t.Errorf("a folder over the entry limit: %d", code)
	}
	var refused int
	w.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE event='fs_download' AND result='too_many_entries'`).Scan(&refused)
	if refused != 1 {
		t.Errorf("the refusal must be audited with its reason, got %d", refused)
	}
	// A file is not a folder.
	_, out = w.call("POST", "/api/nodes/1/fs/downloads", map[string]any{"path": filepath.Join(root, "README.md"), "folder": true})
	if code, _, _ := w.raw("GET", out["url"].(string), nil, nil); code != 400 {
		t.Errorf("zip of a file: %d", code)
	}
}
