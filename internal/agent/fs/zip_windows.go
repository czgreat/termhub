//go:build windows

package fs

import (
	"archive/zip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	CodeTooManyEntries = "too_many_entries"
	skippedListName    = "_termhub_skipped.txt"
)

// Limits of a folder download (docs/M4 第 6 节). Variables so that a node
// setting can change them later, and tests can lower them.
var (
	MaxZipBytes   int64 = 2 << 30
	MaxZipEntries       = 50000
)

// ZipPlan is what a folder download will contain, decided before a byte is sent.
type ZipPlan struct {
	Root    string
	Name    string   // suggested file name, "<folder>.zip"
	Files   []string // paths relative to Root, forward slashes
	Dirs    []string // empty folders, so they survive the round trip
	Skipped []string // what was left out, with the reason
	Bytes   int64
	planned map[string]os.FileInfo // the identity and size accepted by the scan
}

// PlanZip walks the folder once. Links (symbolic links and junctions) are
// never followed: that is the simple way to guarantee the archive cannot reach
// outside the folder. Unreadable entries are skipped and listed. Exceeding a
// limit refuses the whole download, with the actual numbers.
func PlanZip(path string, opt Options) (*ZipPlan, error) {
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
	p := &ZipPlan{Root: clean, Name: filepath.Base(clean) + ".zip", planned: map[string]os.FileInfo{}}
	if len(clean) <= 3 { // a drive root has no base name
		p.Name = strings.TrimSuffix(clean[:1], ":") + "-drive.zip"
	}
	root := long(clean)
	scanned := 0
	err = filepath.WalkDir(root, func(full string, d fs.DirEntry, walkErr error) error {
		rel, _ := filepath.Rel(root, full)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		scanned++
		if scanned > MaxZipEntries {
			return fail(CodeTooManyEntries, "too many scanned entries")
		}
		if walkErr != nil {
			p.Skipped = append(p.Skipped, rel+": "+errLabel(walkErr))
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			p.Skipped = append(p.Skipped, rel+": unreadable")
			return nil
		}
		if entryOf(d.Name(), info).Link {
			p.Skipped = append(p.Skipped, rel+": link, not followed")
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if es, err := os.ReadDir(full); err == nil && len(es) == 0 {
				p.Dirs = append(p.Dirs, rel+"/")
				if len(p.Files)+len(p.Dirs) > MaxZipEntries {
					return fail(CodeTooManyEntries, fmt.Sprintf("more than %d entries", MaxZipEntries))
				}
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			p.Skipped = append(p.Skipped, rel+": not a regular file")
			return nil
		}
		p.Files = append(p.Files, rel)
		p.planned[rel] = info
		if info.Size() > MaxZipBytes-p.Bytes {
			return fail(CodeTooLarge, "archive exceeds size limit")
		}
		p.Bytes += info.Size()
		if len(p.Files)+len(p.Dirs) > MaxZipEntries {
			return fail(CodeTooManyEntries, fmt.Sprintf("more than %d entries", MaxZipEntries))
		}
		if p.Bytes > MaxZipBytes {
			return fail(CodeTooLarge, fmt.Sprintf("more than %d MB", MaxZipBytes>>20))
		}
		return nil
	})
	if err != nil {
		if _, ok := err.(*Error); ok {
			return nil, err
		}
		return nil, mapErr(err)
	}
	if len(p.Skipped) > 0 && len(p.Files)+len(p.Dirs)+1 > MaxZipEntries {
		return nil, fail(CodeTooManyEntries, "too many archive entries")
	}
	return p, nil
}

func errLabel(err error) string {
	if e, ok := mapErr(err).(*Error); ok {
		return e.Code
	}
	return "error"
}

// Write streams the archive. Files are stored, not deflated: the link is a LAN
// and most project content is already compressed or small; the CPU of the node
// belongs to the CLIs running on it. A file that cannot be opened any more is
// added to the skipped list instead of failing the download.
func (p *ZipPlan) Write(w io.Writer) error {
	if len(p.Files)+len(p.Dirs) > MaxZipEntries {
		return fail(CodeTooManyEntries, "too many entries")
	}
	zw := zip.NewWriter(w)
	var written int64
	entries := len(p.Dirs)
	skipped := append([]string(nil), p.Skipped...)
	for _, d := range p.Dirs {
		if _, err := zw.CreateHeader(&zip.FileHeader{Name: d, Method: zip.Store}); err != nil {
			return err
		}
	}
	for _, rel := range p.Files {
		full := long(filepath.Join(p.Root, filepath.FromSlash(rel)))
		f, err := os.Open(full)
		if err != nil {
			skipped = append(skipped, rel+": "+errLabel(err))
			continue
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			skipped = append(skipped, rel+": unreadable")
			continue
		}
		before := p.planned[rel]
		if before == nil || !info.Mode().IsRegular() || !os.SameFile(before, info) || info.Size() != before.Size() || !info.ModTime().Equal(before.ModTime()) {
			f.Close()
			skipped = append(skipped, rel+": changed while archiving (not included)")
			continue
		}
		if info.Size() > MaxZipBytes-written {
			f.Close()
			return fail(CodeTooLarge, "archive exceeds size limit")
		}
		entries++
		if entries > MaxZipEntries {
			f.Close()
			return fail(CodeTooManyEntries, "too many archive entries")
		}
		hdr, _ := zip.FileInfoHeader(info)
		hdr.Name, hdr.Method = rel, zip.Store
		out, err := zw.CreateHeader(hdr)
		if err != nil {
			f.Close()
			return err
		}
		// G1: streaming ZIP entries cannot be retracted. Keep at most the
		// planned bytes and label an entry that changed as potentially partial.
		n, copyErr := io.CopyN(out, f, before.Size())
		written += n
		after, statErr := f.Stat()
		var extra [1]byte
		extraN, readErr := f.Read(extra[:])
		f.Close()
		if copyErr != nil && copyErr != io.EOF && copyErr != io.ErrUnexpectedEOF {
			return copyErr
		}
		if copyErr != nil || statErr != nil || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) || extraN != 0 || readErr != io.EOF {
			skipped = append(skipped, rel+": changed while archiving (included bounded, possibly partial content)")
		}
	}
	if len(skipped) > 0 {
		if entries+1 > MaxZipEntries {
			return fail(CodeTooManyEntries, "too many archive entries")
		}
		report := "These entries were skipped or changed during archiving (see each reason):\r\n\r\n" + strings.Join(skipped, "\r\n") + "\r\n"
		if int64(len(report)) > MaxZipBytes-written {
			return fail(CodeTooLarge, "archive exceeds size limit")
		}
		out, err := zw.Create(skippedListName)
		if err != nil {
			return err
		}
		if _, err := io.WriteString(out, report); err != nil {
			return err
		}
	}
	return zw.Close()
}
