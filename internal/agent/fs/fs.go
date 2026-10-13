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

// Drive is one entry of Drives.
type Drive struct {
	Letter string `json:"letter"`
	Kind   string `json:"kind"` // fixed, removable, network, cdrom, ram, unknown
	Label  string `json:"label,omitempty"`
	Total  uint64 `json:"total,omitempty"`
	Free   uint64 `json:"free,omitempty"`
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
