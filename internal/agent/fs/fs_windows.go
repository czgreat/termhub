//go:build windows

// Package fs is the node side of docs/M4-文件与目录.md: browsing, file
// information and creating folders. The path and name rules are enforced
// here, on the node; whatever the Hub checks is only for early feedback.
package fs

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unicode"

	"golang.org/x/sys/windows"
)

// Error carries one of the codes of docs/M4 第 9 节.
type Error struct{ Code, Msg string }

func (e *Error) Error() string { return e.Code + ": " + e.Msg }

const (
	CodePathInvalid  = "path_invalid"
	CodeNameInvalid  = "name_invalid"
	CodeNotFound     = "not_found"
	CodeNotDirectory = "not_a_directory"
	CodeAccessDenied = "access_denied"
	CodeExists       = "exists"
	CodeUNCDisabled  = "unc_disabled"
)

func fail(code, msg string) *Error { return &Error{code, msg} }

// Options are per-node settings.
type Options struct {
	AllowUNC bool // \\server\share paths; off by default (docs/M4 第 3 节)
}

// PageSize is the number of entries per List page.
const PageSize = 1000

// CleanPath validates an absolute Windows path and returns its canonical form.
func CleanPath(p string, opt Options) (string, error) {
	if len(p) == 0 || len(p) > 32000 {
		return "", fail(CodePathInvalid, "empty or too long")
	}
	for _, r := range p {
		if r == 0 || unicode.IsControl(r) {
			return "", fail(CodePathInvalid, "control character in path")
		}
	}
	p = strings.ReplaceAll(p, "/", `\`)
	if strings.HasPrefix(p, `\\?\`) || strings.HasPrefix(p, `\\.\`) || strings.HasPrefix(p, `\??\`) {
		return "", fail(CodePathInvalid, "device paths are not accepted")
	}
	if strings.HasPrefix(p, `\\`) {
		if !opt.AllowUNC {
			return "", fail(CodeUNCDisabled, "network paths are disabled on this node")
		}
		rest := strings.Split(strings.TrimLeft(p, `\`), `\`)
		if len(rest) < 2 || rest[0] == "" || rest[1] == "" {
			return "", fail(CodePathInvalid, `a network path needs \\server\share`)
		}
		return filepath.Clean(p), nil
	}
	if len(p) < 3 || p[1] != ':' || p[2] != '\\' || !isDriveLetter(p[0]) {
		return "", fail(CodePathInvalid, `an absolute path like C:\folder is required`)
	}
	// Reject alternate data streams and drive-relative tricks: no further colon.
	if strings.Contains(p[2:], ":") {
		return "", fail(CodePathInvalid, "colon outside the drive letter")
	}
	clean := filepath.Clean(p) // resolves . and ..; cannot climb above the drive root
	return strings.ToUpper(clean[:1]) + clean[1:], nil
}

func isDriveLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

var reserved = map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true}

// CheckName validates a single file or folder name (docs/M4 第 3 节).
func CheckName(name string) error {
	if name == "" || len(name) > 255 || name == "." || name == ".." {
		return fail(CodeNameInvalid, "empty, too long, or a dot name")
	}
	for _, r := range name {
		if r < 0x20 || strings.ContainsRune(`\/:*?"<>|`, r) {
			return fail(CodeNameInvalid, `a name cannot contain \ / : * ? " < > | or control characters`)
		}
	}
	if last := name[len(name)-1]; last == ' ' || last == '.' {
		return fail(CodeNameInvalid, "a name cannot end with a space or a dot")
	}
	stem := strings.ToUpper(strings.TrimRight(strings.SplitN(name, ".", 2)[0], " "))
	if reserved[stem] {
		return fail(CodeNameInvalid, stem+" is reserved by Windows")
	}
	return nil
}

// long turns a validated path into the extended-length form, so paths beyond
// 260 characters work. Callers never see this form.
func long(p string) string {
	if strings.HasPrefix(p, `\\`) {
		return `\\?\UNC\` + p[2:]
	}
	return `\\?\` + p
}

func mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, os.ErrNotExist):
		return fail(CodeNotFound, "no such file or folder")
	case errors.Is(err, os.ErrPermission):
		return fail(CodeAccessDenied, "access denied")
	case errors.Is(err, os.ErrExist):
		return fail(CodeExists, "already exists")
	}
	return fail(CodePathInvalid, err.Error())
}

// Desktop is the desktop folder of the user the node runs as, wherever
// Windows keeps it (OneDrive may have moved it), or "" when there is none.
// The folder picker offers it next to the drives: getting there from C: by
// hand took too many steps (主人 2026-09-25).
func Desktop() string {
	known, _ := windows.KnownFolderPath(windows.FOLDERID_Desktop, 0)
	home, _ := windows.KnownFolderPath(windows.FOLDERID_Profile, 0)
	windir, _ := windows.KnownFolderPath(windows.FOLDERID_Windows, 0)
	p := pickDesktop(known, home, windir)
	if p == "" {
		return ""
	}
	if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
		return ""
	}
	return p
}

// pickDesktop: on some machines the user's registered desktop points into
// SYSTEM's profile under the Windows folder, and a
// session started there would work inside C:\Windows; their real desktop is
// the one in the user's own profile (桌面按钮复核). Nothing under the Windows
// folder is offered.
func pickDesktop(known, home, windir string) string {
	under := func(p string) bool {
		return windir != "" && strings.HasPrefix(strings.ToLower(filepath.Clean(p))+`\`, strings.ToLower(filepath.Clean(windir))+`\`)
	}
	if known != "" && !under(known) {
		return known
	}
	if home != "" && !under(home) {
		return filepath.Join(home, "Desktop")
	}
	return ""
}

// Drive is one entry of Drives.
type Drive struct {
	Letter string `json:"letter"`
	Kind   string `json:"kind"` // fixed, removable, network, cdrom, ram, unknown
	Label  string `json:"label,omitempty"`
	Total  uint64 `json:"total,omitempty"`
	Free   uint64 `json:"free,omitempty"`
}

// Drives lists the logical drives. Network drives are not queried for label
// and space: a disconnected share would stall the call for a long time.
func Drives() []Drive {
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return nil
	}
	var out []Drive
	for i := 0; i < 26; i++ {
		if mask&(1<<i) == 0 {
			continue
		}
		root := string(rune('A'+i)) + `:\`
		rootP, _ := windows.UTF16PtrFromString(root)
		d := Drive{Letter: root[:2]}
		switch windows.GetDriveType(rootP) {
		case windows.DRIVE_FIXED:
			d.Kind = "fixed"
		case windows.DRIVE_REMOVABLE:
			d.Kind = "removable"
		case windows.DRIVE_REMOTE:
			d.Kind = "network"
		case windows.DRIVE_CDROM:
			d.Kind = "cdrom"
		case windows.DRIVE_RAMDISK:
			d.Kind = "ram"
		default:
			d.Kind = "unknown"
		}
		if d.Kind == "fixed" || d.Kind == "ram" {
			var label [windows.MAX_PATH + 1]uint16
			if windows.GetVolumeInformation(rootP, &label[0], uint32(len(label)), nil, nil, nil, nil, 0) == nil {
				d.Label = windows.UTF16ToString(label[:])
			}
			var free, total, totalFree uint64
			if windows.GetDiskFreeSpaceEx(rootP, &free, &total, &totalFree) == nil {
				d.Total, d.Free = total, free
			}
		}
		out = append(out, d)
	}
	return out
}

// Entry describes a file or folder.
type Entry struct {
	Name     string `json:"name"`
	Dir      bool   `json:"dir"`
	Link     bool   `json:"link,omitempty"`
	Size     int64  `json:"size"`
	Modified int64  `json:"modified"` // unix seconds
	Hidden   bool   `json:"hidden,omitempty"`
	System   bool   `json:"system,omitempty"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

func entryOf(name string, fi os.FileInfo) Entry {
	e := Entry{Name: name, Dir: fi.IsDir(), Size: fi.Size(), Modified: fi.ModTime().Unix(), Link: fi.Mode()&os.ModeSymlink != 0}
	if d, ok := fi.Sys().(*syscall.Win32FileAttributeData); ok {
		e.Hidden = d.FileAttributes&windows.FILE_ATTRIBUTE_HIDDEN != 0
		e.System = d.FileAttributes&windows.FILE_ATTRIBUTE_SYSTEM != 0
		e.ReadOnly = d.FileAttributes&windows.FILE_ATTRIBUTE_READONLY != 0
		if d.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			e.Link = true // junctions too
		}
	}
	if e.Dir {
		e.Size = 0
	}
	return e
}

// Listing is one page of a folder.
type Listing struct {
	Path    string  `json:"path"`
	Parent  string  `json:"parent,omitempty"`
	Entries []Entry `json:"entries"`
	Page    int     `json:"page"`
	Pages   int     `json:"pages"`
	Total   int     `json:"total"`
}

// List returns one page of a folder: folders first, then by name. Hidden and
// system entries are left out unless asked for.
func List(path string, page int, showHidden bool, opt Options) (*Listing, error) {
	clean, err := CleanPath(path, opt)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(long(clean))
	if err != nil {
		return nil, mapErr(err)
	}
	if !fi.IsDir() {
		return nil, fail(CodeNotDirectory, "not a folder")
	}
	des, err := os.ReadDir(long(clean))
	if err != nil {
		return nil, mapErr(err)
	}
	entries := make([]Entry, 0, len(des))
	for _, de := range des {
		info, err := de.Info()
		if err != nil {
			continue // vanished or unreadable: it does not spoil the rest
		}
		e := entryOf(de.Name(), info)
		if e.Link { // follow for the folder/file distinction, keep the link mark
			if target, err := os.Stat(long(filepath.Join(clean, de.Name()))); err == nil {
				e.Dir = target.IsDir()
			}
		}
		if (e.Hidden || e.System) && !showHidden {
			continue
		}
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Dir != entries[j].Dir {
			return entries[i].Dir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	l := &Listing{Path: clean, Total: len(entries), Pages: (len(entries) + PageSize - 1) / PageSize, Page: max(page, 0)}
	if parent := filepath.Dir(clean); parent != clean {
		l.Parent = parent
	}
	// Clamp before multiplying: a peer may send any representable int (#7 F01).
	l.Page = min(l.Page, l.Pages)
	start := len(entries)
	if l.Page < l.Pages {
		start = l.Page * PageSize
	}
	l.Entries = entries[start:min(start+PageSize, len(entries))]
	return l, nil
}

// Stat describes one file or folder.
func Stat(path string, opt Options) (*Entry, error) {
	clean, err := CleanPath(path, opt)
	if err != nil {
		return nil, err
	}
	fi, err := os.Lstat(long(clean))
	if err != nil {
		return nil, mapErr(err)
	}
	e := entryOf(filepath.Base(clean), fi)
	return &e, nil
}

// Mkdir creates one folder named name inside parent, which must exist.
func Mkdir(parent, name string, opt Options) (string, error) {
	clean, err := CleanPath(parent, opt)
	if err != nil {
		return "", err
	}
	if err := CheckName(name); err != nil {
		return "", err
	}
	fi, err := os.Stat(long(clean))
	if err != nil {
		return "", mapErr(err)
	}
	if !fi.IsDir() {
		return "", fail(CodeNotDirectory, "the parent is not a folder")
	}
	full := filepath.Join(clean, name)
	if err := os.Mkdir(long(full), 0o755); err != nil {
		return "", mapErr(err)
	}
	return full, nil
}
