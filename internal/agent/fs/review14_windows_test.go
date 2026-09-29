//go:build windows

package fs

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReview14ZipChanged(t *testing.T) {
	for _, change := range []string{"grow", "replace", "shrink"} {
		t.Run(change, func(t *testing.T) {
			dir := t.TempDir()
			name := filepath.Join(dir, "changing.txt")
			for _, n := range []string{"changing.txt", "stable.txt"} {
				if err := os.WriteFile(filepath.Join(dir, n), []byte("old"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			p, err := PlanZip(dir, Options{})
			if err != nil {
				t.Fatal(err)
			}
			data := "growing"
			if change == "replace" {
				if err := os.Remove(name); err != nil {
					t.Fatal(err)
				}
				data = "new"
			}
			if change == "shrink" {
				data = "x"
			}
			if err := os.WriteFile(name, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			var b bytes.Buffer
			if err := p.Write(&b); err != nil {
				t.Fatal(err)
			}
			z, err := zip.NewReader(bytes.NewReader(b.Bytes()), int64(b.Len()))
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]string{}
			for _, f := range z.File {
				r, err := f.Open()
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(r)
				r.Close()
				if err != nil {
					t.Fatal(err)
				}
				got[f.Name] = string(data)
			}
			if got["stable.txt"] != "old" {
				t.Fatalf("stable file: %v", got)
			}
			if _, ok := got["changing.txt"]; ok {
				t.Fatal("changed file included")
			}
			if !strings.Contains(got[skippedListName], "changing.txt: changed while archiving") {
				t.Fatalf("missing report: %v", got)
			}
			old := MaxZipBytes
			MaxZipBytes = 4
			defer func() { MaxZipBytes = old }()
			if err := p.Write(io.Discard); code(err) != CodeTooLarge {
				t.Fatalf("report budget: %v", err)
			}
		})
	}
}

// Change the source when the first ZIP bytes are flushed, while CopyN is
// writing its entry. A few KB are enough; no concurrent writer or load test.
type review14Writer struct {
	bytes.Buffer
	change func()
}

func (w *review14Writer) Write(b []byte) (int, error) {
	if w.change != nil {
		fn := w.change
		w.change = nil
		fn()
	}
	return w.Buffer.Write(b)
}
func TestReview14ZipChangesDuringCopy(t *testing.T) {
	for _, shrink := range []bool{false, true} {
		t.Run(fmt.Sprint(shrink), func(t *testing.T) {
			dir := t.TempDir()
			name := filepath.Join(dir, "changing.txt")
			size := 5000
			if shrink {
				size = 40000
			} // exceed CopyN's buffer to exercise a short read
			original := bytes.Repeat([]byte("a"), size)
			if err := os.WriteFile(name, original, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "stable.txt"), []byte("ok"), 0600); err != nil {
				t.Fatal(err)
			}
			p, err := PlanZip(dir, Options{})
			if err != nil {
				t.Fatal(err)
			}
			w := &review14Writer{change: func() {
				size := int64(6000)
				if shrink {
					size = 1
				}
				if err := os.Truncate(name, size); err != nil {
					t.Fatal(err)
				}
			}}
			if err := p.Write(w); err != nil {
				t.Fatal(err)
			}
			z, err := zip.NewReader(bytes.NewReader(w.Bytes()), int64(w.Len()))
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]string{}
			var total uint64
			for _, f := range z.File {
				total += f.UncompressedSize64
				r, err := f.Open()
				if err != nil {
					t.Fatal(err)
				}
				b, err := io.ReadAll(r)
				r.Close()
				if err != nil {
					t.Fatal(err)
				}
				got[f.Name] = string(b)
			}
			if got["stable.txt"] != "ok" || len(got["changing.txt"]) > len(original) || !strings.Contains(got[skippedListName], "included bounded, possibly partial content") {
				t.Fatalf("bad archive: names=%d report=%q", len(got), got[skippedListName])
			}
			if shrink && len(got["changing.txt"]) >= len(original) {
				t.Fatal("shrink did not exercise a short read")
			}
			if total > uint64(MaxZipBytes) {
				t.Fatal("over budget")
			}
		})
	}
}
