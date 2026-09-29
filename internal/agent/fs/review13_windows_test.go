//go:build windows

package fs

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestReview13PageOverflow(t *testing.T) {
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("untrusted page panicked: %v", p)
		}
	}()
	dir := t.TempDir()
	for _, page := range []int{0, 1, int(^uint(0) >> 1), 9223372036854776} {
		got, err := List(dir, page, false, Options{})
		if err != nil || len(got.Entries) != 0 {
			t.Fatalf("page %d: %+v %v", page, got, err)
		}
	}
}

func TestReview13UploadCommitConflict(t *testing.T) {
	for _, policy := range []string{ConflictFail, ConflictRename, ConflictOverwrite} {
		t.Run(policy, func(t *testing.T) {
			dir := t.TempDir()
			u, err := BeginUpload(dir, "same.txt", 3, policy, Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer u.Abort()
			if err = u.Write(0, []byte("new")); err != nil {
				t.Fatal(err)
			}
			original := filepath.Join(dir, "same.txt")
			if err = os.WriteFile(original, []byte("old"), 0600); err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256([]byte("new"))
			final, err := u.Finish(hex.EncodeToString(sum[:]))
			if policy == ConflictFail {
				if code(err) != CodeExists {
					t.Fatalf("fail policy: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(original)
			want := "old"
			if policy == ConflictOverwrite {
				want = "new"
			}
			if string(got) != want {
				t.Fatalf("original=%q want=%q", got, want)
			}
			if policy == ConflictRename {
				if final == original {
					t.Fatal("rename overwrote original")
				}
				got, err = os.ReadFile(final)
				if err != nil || string(got) != "new" {
					t.Fatalf("renamed: %q %v", got, err)
				}
			}
		})
	}
}

func TestReview13ZipBounds(t *testing.T) {
	oldEntries, oldBytes := MaxZipEntries, MaxZipBytes
	t.Cleanup(func() { MaxZipEntries, MaxZipBytes = oldEntries, oldBytes })
	MaxZipEntries = 2
	dir := t.TempDir()
	for _, name := range []string{"a", "b", "c"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := PlanZip(dir, Options{}); code(err) != CodeTooManyEntries {
		t.Errorf("empty folders: %v", err)
	}
	MaxZipEntries, MaxZipBytes = oldEntries, 4
	dir = t.TempDir()
	name := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(name, []byte("abc"), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := PlanZip(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(name, []byte("12345"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = p.Write(io.Discard); err == nil {
		t.Fatal("grew after planning but archive succeeded")
	}
}
