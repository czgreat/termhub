//go:build linux

package fs

import (
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"golang.org/x/sys/unix"
)

// readFlags opens files to send. O_NONBLOCK: opening a FIFO would otherwise
// wait for a writer and hold the transfer slot; the caller then refuses
// anything that is not a regular file, and regular files ignore the flag
// (复核 PR #40 第 8 条).
const readFlags = os.O_RDONLY | unix.O_NONBLOCK

// CleanPath validates an absolute POSIX path and returns its canonical form.
// Options.AllowUNC has no meaning here.
func CleanPath(p string, opt Options) (string, error) {
	if len(p) == 0 || len(p) > 4096 {
		return "", fail(CodePathInvalid, "empty or too long")
	}
	for _, r := range p {
		if r == 0 || unicode.IsControl(r) {
			return "", fail(CodePathInvalid, "control character in path")
		}
	}
	if !strings.HasPrefix(p, "/") {
		return "", fail(CodePathInvalid, "an absolute path like /home/me/folder is required")
	}
	return filepath.Clean(p), nil // resolves . and ..; cannot climb above /
}

// CheckName validates a single file or folder name (docs/M4 第 3 节). Linux
// forbids only the separator and NUL; control characters are refused too,
// as on Windows, since a terminal would render them.
func CheckName(name string) error {
	if name == "" || len(name) > 255 || name == "." || name == ".." {
		return fail(CodeNameInvalid, "empty, too long, or a dot name")
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || r == '/' {
			return fail(CodeNameInvalid, "a name cannot contain / or control characters")
		}
	}
	return nil
}

// long is the identity on Linux: there is no path length workaround to apply.
func long(p string) string { return p }

// Desktop is where the folder picker's quick button leads. On a Linux node
// that is the user's home: projects live there (/home/<user>/...), and a
// desktop folder, if one exists at all, is usually empty.
func Desktop() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	if fi, err := os.Stat(home); err != nil || !fi.IsDir() {
		return ""
	}
	return home
}

// Drives lists the one root, with the space of its file system.
func Drives() []Drive {
	d := Drive{Letter: "/", Kind: "fixed"}
	var st unix.Statfs_t
	if unix.Statfs("/", &st) == nil {
		d.Total, d.Free = st.Blocks*uint64(st.Bsize), st.Bavail*uint64(st.Bsize)
	}
	return []Drive{d}
}

// entryOf: hidden is a leading dot; read-only is no write bit for the owner.
func entryOf(name string, fi os.FileInfo) Entry {
	e := Entry{Name: name, Dir: fi.IsDir(), Size: fi.Size(), Modified: fi.ModTime().Unix(),
		Link: fi.Mode()&os.ModeSymlink != 0, Hidden: strings.HasPrefix(name, "."),
		ReadOnly: fi.Mode().Perm()&0o200 == 0}
	if e.Dir {
		e.Size = 0
	}
	return e
}

// archiveName is the suggested name of a folder download; / has no base name.
func archiveName(clean string) string {
	if clean == "/" {
		return "root.zip"
	}
	return filepath.Base(clean) + ".zip"
}
