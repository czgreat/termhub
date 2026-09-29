//go:build windows

package fs

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	CodeTypeNotAllowed = "type_not_allowed"
	MaxImageSize       = 20 << 20
	pasteKeep          = 7 * 24 * time.Hour
)

// imageType recognises an image by its leading bytes. Neither the file name
// nor what the browser claims is trusted (docs/M4 第 7 节).
func imageType(head []byte) string {
	switch {
	case bytes.HasPrefix(head, []byte("\x89PNG\r\n\x1a\n")):
		return "png"
	case bytes.HasPrefix(head, []byte("\xff\xd8\xff")):
		return "jpg"
	case bytes.HasPrefix(head, []byte("GIF87a")), bytes.HasPrefix(head, []byte("GIF89a")):
		return "gif"
	case len(head) >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == "WEBP":
		return "webp"
	case bytes.HasPrefix(head, []byte("BM")) && len(head) >= 14:
		return "bmp"
	}
	return ""
}

// BeginPaste opens an upload into the session's own paste folder under base.
// For images the type is decided from the first bytes and the extension is
// chosen accordingly; anything else is refused when imageOnly is set.
func BeginPaste(base, session, name string, size int64, imageOnly bool) (*Upload, error) {
	if size <= 0 || (imageOnly && size > MaxImageSize) {
		return nil, fail(CodeTooLarge, "empty or over the size limit for pasted images")
	}
	for _, r := range session {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r == '-') {
			return nil, fail(CodePathInvalid, "bad session id")
		}
	}
	dir := filepath.Join(base, "paste", session)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, mapErr(err)
	}
	rnd := make([]byte, 4)
	rand.Read(rnd)
	stem := fmt.Sprintf("%d-%s", time.Now().UnixMilli(), hex.EncodeToString(rnd))
	ext := strings.ToLower(filepath.Ext(name))
	if CheckName("x"+ext) != nil || len(ext) > 16 || strings.ContainsAny(ext, " ") {
		ext = ""
	}
	u := &Upload{size: size, sum: sha256.New()}
	u.temp = filepath.Join(dir, "."+stem+".thpart")
	u.Final = filepath.Join(dir, stem+ext)
	if imageOnly {
		u.sniff = func(head []byte) error {
			kind := imageType(head)
			if kind == "" {
				return fail(CodeTypeNotAllowed, "only png, jpg, gif, webp and bmp images can be pasted")
			}
			u.Final = filepath.Join(dir, stem+"."+kind) // the extension follows the content, not the name
			return nil
		}
	}
	f, err := os.OpenFile(long(u.temp), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, mapErr(err)
	}
	u.f = f
	return u, nil
}

// RemovePaste deletes a session's paste folder; called when the session ends.
func RemovePaste(base, session string) {
	if session == "" || strings.ContainsAny(session, `\/.`) {
		return
	}
	os.RemoveAll(filepath.Join(base, "paste", session))
}

// SweepPaste removes paste folders of sessions that no longer exist and files
// past the retention period. keep reports whether a session id is still alive.
func SweepPaste(base string, keep func(session string) bool) {
	root := filepath.Join(base, "paste")
	sessions, _ := os.ReadDir(root)
	for _, s := range sessions {
		dir := filepath.Join(root, s.Name())
		if !s.IsDir() || !keep(s.Name()) {
			os.RemoveAll(dir)
			continue
		}
		files, _ := os.ReadDir(dir)
		for _, f := range files {
			if info, err := f.Info(); err == nil && time.Since(info.ModTime()) > pasteKeep {
				os.Remove(filepath.Join(dir, f.Name()))
			}
		}
	}
}
