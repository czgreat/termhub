//go:build windows

package fs

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

const (
	CodeNoSpace      = "no_space"
	CodeHashMismatch = "hash_mismatch"
	CodeChanged      = "changed_during_transfer"
	CodeTooLarge     = "too_large"
)

// Conflict policies for an upload whose target name exists (docs/M4 第 5 节).
const (
	ConflictFail      = "fail"
	ConflictRename    = "rename"
	ConflictOverwrite = "overwrite"
)

// Upload receives one file into a temporary sibling of its target and moves
// it into place only after size and hash check out, so a broken transfer
// never leaves a half-written file under the real name.
type Upload struct {
	Final    string
	temp     string
	f        *os.File
	size     int64
	got      int64
	sum      hash.Hash
	sniff    func(head []byte) error // optional check of the first bytes
	conflict string
	base     string // original requested target, before a numbered copy
	copyNo   int
}

// BeginUpload validates the target and opens the temporary file.
func BeginUpload(dir, name string, size int64, conflict string, opt Options) (*Upload, error) {
	clean, err := CleanPath(dir, opt)
	if err != nil {
		return nil, err
	}
	if err := CheckName(name); err != nil {
		return nil, err
	}
	if size < 0 {
		return nil, fail(CodePathInvalid, "negative size")
	}
	if fi, err := os.Stat(long(clean)); err != nil {
		return nil, mapErr(err)
	} else if !fi.IsDir() {
		return nil, fail(CodeNotDirectory, "the target is not a folder")
	}
	final := filepath.Join(clean, name)
	base, copyNo := final, 0
	if _, err := os.Lstat(long(final)); err == nil {
		switch conflict {
		case ConflictOverwrite:
		case ConflictRename:
			ext := filepath.Ext(name)
			stem := strings.TrimSuffix(name, ext)
			for i := 1; ; i++ {
				copyNo = i
				final = filepath.Join(clean, fmt.Sprintf("%s (%d)%s", stem, i, ext))
				if _, err := os.Lstat(long(final)); err != nil {
					break
				}
				if i > 9999 {
					return nil, fail(CodeExists, "too many copies")
				}
			}
		default:
			return nil, fail(CodeExists, "a file with this name already exists")
		}
	}
	// Refuse before a single byte is sent when the disk cannot hold it.
	dirP, _ := windows.UTF16PtrFromString(clean)
	var free, total, totalFree uint64
	if windows.GetDiskFreeSpaceEx(dirP, &free, &total, &totalFree) == nil && uint64(size) > free {
		return nil, fail(CodeNoSpace, "not enough free space on the target drive")
	}
	rnd := make([]byte, 6)
	rand.Read(rnd)
	temp := filepath.Join(clean, "."+filepath.Base(final)+"."+hex.EncodeToString(rnd)+".thpart")
	f, err := os.OpenFile(long(temp), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, mapErr(err)
	}
	return &Upload{Final: final, temp: temp, f: f, size: size, sum: sha256.New(), conflict: conflict, base: base, copyNo: copyNo}, nil
}

// Received is the offset the next chunk must start at.
func (u *Upload) Received() int64 { return u.got }

// Write appends a chunk that must start exactly at offset.
func (u *Upload) Write(offset int64, p []byte) error {
	if offset != u.got {
		return fail(CodePathInvalid, fmt.Sprintf("chunk at %d, expected %d", offset, u.got))
	}
	if u.got+int64(len(p)) > u.size {
		return fail(CodeTooLarge, "more data than announced")
	}
	if u.got == 0 && u.sniff != nil {
		if err := u.sniff(p); err != nil {
			return err
		}
	}
	if _, err := u.f.Write(p); err != nil {
		return fail(CodeNoSpace, err.Error())
	}
	u.sum.Write(p)
	u.got += int64(len(p))
	return nil
}

// Finish checks size and SHA-256, then moves the file into place. With the
// overwrite policy the old file is replaced in one step and stays intact if
// anything before that step fails.
func (u *Upload) Finish(sha256hex string) (string, error) {
	defer u.Abort()
	if u.got != u.size {
		return "", fail(CodeHashMismatch, fmt.Sprintf("received %d of %d bytes", u.got, u.size))
	}
	if got := hex.EncodeToString(u.sum.Sum(nil)); !strings.EqualFold(got, sha256hex) {
		return "", fail(CodeHashMismatch, "the received data does not match the announced SHA-256")
	}
	if err := u.f.Close(); err != nil {
		return "", mapErr(err)
	}
	u.f = nil
	// Only overwrite may replace a target created after BeginUpload (#7 F02).
	from, err := windows.UTF16PtrFromString(long(u.temp))
	if err != nil {
		return "", mapErr(err)
	}
	var flags uint32
	if u.conflict == ConflictOverwrite {
		flags = windows.MOVEFILE_REPLACE_EXISTING
	}
	for {
		to, err := windows.UTF16PtrFromString(long(u.Final))
		if err != nil {
			return "", mapErr(err)
		}
		err = windows.MoveFileEx(from, to, flags)
		if err == nil {
			break
		}
		if !errors.Is(err, windows.ERROR_ALREADY_EXISTS) && !errors.Is(err, windows.ERROR_FILE_EXISTS) {
			return "", mapErr(err)
		}
		if u.conflict != ConflictRename || u.copyNo >= 9999 {
			return "", fail(CodeExists, "a file with this name already exists")
		}
		u.copyNo++
		ext := filepath.Ext(u.base)
		u.Final = fmt.Sprintf("%s (%d)%s", strings.TrimSuffix(u.base, ext), u.copyNo, ext)
	}
	u.temp = ""
	return u.Final, nil
}

// Abort discards the temporary file. It is safe to call more than once and
// after Finish.
func (u *Upload) Abort() {
	if u.f != nil {
		u.f.Close()
		u.f = nil
	}
	if u.temp != "" {
		os.Remove(long(u.temp))
		u.temp = ""
	}
}

// Download is an opened file being sent.
type Download struct {
	Name     string
	Size     int64
	Modified int64
	f        *os.File
}

// OpenDownload opens a regular file for reading.
func OpenDownload(path string, opt Options) (*Download, error) {
	clean, err := CleanPath(path, opt)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(long(clean))
	if err != nil {
		return nil, mapErr(err)
	}
	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		f.Close()
		return nil, fail(CodeNotFound, "not a file")
	}
	return &Download{Name: filepath.Base(clean), Size: fi.Size(), Modified: fi.ModTime().UnixNano(), f: f}, nil
}

func (d *Download) ReadAt(p []byte, off int64) (int, error) { return d.f.ReadAt(p, off) }

// Unchanged reports whether the file still has the size and time it had when
// it was opened: a file modified while being sent must not be delivered as if
// it were consistent (docs/M4 第 6 节).
func (d *Download) Unchanged() bool {
	fi, err := d.f.Stat()
	return err == nil && fi.Size() == d.Size && fi.ModTime().UnixNano() == d.Modified
}

func (d *Download) Close() { d.f.Close() }
