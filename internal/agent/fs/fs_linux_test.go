//go:build linux

package fs

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLinuxCleanPath(t *testing.T) {
	ok := map[string]string{"/": "/", "/home/a/../b/./c/": "/home/b/c", "//x": "/x", "/../..": "/", "/中文 目录": "/中文 目录"}
	for in, want := range ok {
		if got, err := CleanPath(in, Options{}); err != nil || got != want {
			t.Errorf("CleanPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "relative/x", `C:\Users`, "/a\x00b", "/a\nb"} {
		if _, err := CleanPath(bad, Options{}); err == nil {
			t.Errorf("CleanPath(%q) accepted", bad)
		}
	}
}

func TestLinuxCheckName(t *testing.T) {
	for _, good := range []string{"a", "a b.", " lead", "CON", "a:b*?", "中文"} {
		if err := CheckName(good); err != nil {
			t.Errorf("CheckName(%q): %v", good, err)
		}
	}
	for _, bad := range []string{"", ".", "..", "a/b", "a\x01", string(make([]byte, 256))} {
		if CheckName(bad) == nil {
			t.Errorf("CheckName(%q) accepted", bad)
		}
	}
}

func TestLinuxListHidesDotFiles(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, ".secret"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("xy"), 0o644)
	os.Symlink("/etc", filepath.Join(dir, "link"))
	l, err := List(dir, 0, false, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range l.Entries {
		names = append(names, e.Name)
	}
	if len(names) != 3 || names[0] != "link" && names[0] != "sub" || l.Parent != filepath.Dir(dir) {
		t.Fatalf("entries %v parent %q", names, l.Parent)
	}
	for _, e := range l.Entries {
		if e.Name == "link" && (!e.Link || !e.Dir) {
			t.Fatalf("link entry %+v", e)
		}
	}
	if l, _ := List(dir, 0, true, Options{}); l.Total != 4 {
		t.Fatalf("with hidden: %d", l.Total)
	}
	if root, err := List("/", 0, false, Options{}); err != nil || root.Parent != "" {
		t.Fatalf("root: %v %q", err, root.Parent)
	}
}

func upload(t *testing.T, dir, name, conflict string, data []byte) (*Upload, error) {
	t.Helper()
	u, err := BeginUpload(dir, name, int64(len(data)), conflict, Options{})
	if err != nil {
		return nil, err
	}
	if err := u.Write(0, data); err != nil {
		t.Fatal(err)
	}
	return u, nil
}

// Only overwrite may replace a target that appeared after the upload began
// (#7 F02): the rename refuses to replace, and rename picks the next number.
func TestLinuxUploadNeverReplacesUnlessAsked(t *testing.T) {
	dir := t.TempDir()
	data := []byte("new content")
	sum := sha256.Sum256(data)
	hexSum := hex.EncodeToString(sum[:])

	u, _ := upload(t, dir, "a.txt", ConflictFail, data)
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("theirs"), 0o644) // appears meanwhile
	if _, err := u.Finish(hexSum); err == nil {
		t.Fatal("replaced a file without the overwrite policy")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(b) != "theirs" {
		t.Fatalf("their file changed: %q", b)
	}

	u, _ = upload(t, dir, "b.txt", ConflictRename, data)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("theirs"), 0o644)
	if got, err := u.Finish(hexSum); err != nil || got != filepath.Join(dir, "b (1).txt") {
		t.Fatalf("rename: %q %v", got, err)
	}

	u, _ = upload(t, dir, "a.txt", ConflictOverwrite, data)
	if got, err := u.Finish(hexSum); err != nil || got != filepath.Join(dir, "a.txt") {
		t.Fatalf("overwrite: %q %v", got, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(b) != "new content" {
		t.Fatalf("not overwritten: %q", b)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".*.thpart")); len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}
}

func TestLinuxDrivesAndDesktop(t *testing.T) {
	d := Drives()
	if len(d) != 1 || d[0].Letter != "/" || d[0].Total == 0 {
		t.Fatalf("drives %+v", d)
	}
	home, _ := os.UserHomeDir()
	if Desktop() != home {
		t.Fatalf("desktop %q", Desktop())
	}
}

// A FIFO is not sent: opening it would wait for a writer and hold the
// transfer slot (复核 PR #40 第 8 条).
func TestLinuxDownloadRefusesFIFO(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		d, err := OpenDownload(fifo, Options{})
		if d != nil {
			d.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a FIFO was opened for download")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("opening a FIFO blocked")
	}
}

// A pasted file keeps a plain extension only; shell syntax in the name's
// extension is dropped (复核 PR #40 第 7 条).
func TestLinuxPasteExtension(t *testing.T) {
	base := t.TempDir()
	for name, want := range map[string]string{"notes.TXT": ".txt", "a.$(id)": "", "x.tar;rm": "", "b.it's": "", "c.verylongextension": ""} {
		u, err := BeginPaste(base, "0123-abcd", name, 10, false)
		if err != nil {
			t.Fatal(err)
		}
		if got := filepath.Ext(u.Final); got != want || strings.ContainsAny(u.Final, "$;'") {
			t.Errorf("%q pasted as %q, want extension %q", name, u.Final, want)
		}
		u.Abort()
	}
}
