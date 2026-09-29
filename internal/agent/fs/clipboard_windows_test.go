package fs

import (
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// The DIB layout: header, bottom-up rows, BGRA order, transparency over white.
func TestDIBFromImage(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.NRGBA{255, 0, 0, 255}) // top-left red
	img.Set(1, 1, color.NRGBA{0, 0, 255, 255}) // bottom-right blue
	img.Set(1, 0, color.NRGBA{0, 0, 0, 0})     // transparent → white
	dib := dibFromImage(img)
	if len(dib) != 40+16 {
		t.Fatalf("size %d", len(dib))
	}
	if binary.LittleEndian.Uint32(dib[4:]) != 2 || binary.LittleEndian.Uint32(dib[8:]) != 2 || binary.LittleEndian.Uint16(dib[14:]) != 32 {
		t.Fatalf("bad header %v", dib[:40])
	}
	row0, row1 := dib[40:48], dib[48:56]                // row0 is the BOTTOM image row
	if got := row1[0:4]; got[2] != 255 || got[0] != 0 { // top-left red: B=0,G=0,R=255
		t.Fatalf("top-left = %v, want red in BGRA", got)
	}
	if got := row0[4:8]; got[0] != 255 || got[2] != 0 { // bottom-right blue
		t.Fatalf("bottom-right = %v, want blue in BGRA", got)
	}
	if got := row1[4:8]; got[0] != 255 || got[1] != 255 || got[2] != 255 { // transparent over white
		t.Fatalf("transparent = %v, want white", got)
	}
}

// Files outside the paste folder are refused before anything is read.
func TestClipboardImageRefusesOutsideBase(t *testing.T) {
	base := t.TempDir()
	other := filepath.Join(t.TempDir(), "x.png")
	os.WriteFile(other, []byte("\x89PNG"), 0o600)
	if err := ClipboardImage(base, other); err == nil {
		t.Fatal("expected refusal")
	}
}

// Setting the real clipboard touches the developer's clipboard; only on request.
func TestClipboardImageReal(t *testing.T) {
	if os.Getenv("TH_CLIPBOARD_TEST") == "" {
		t.Skip("set TH_CLIPBOARD_TEST=1 to overwrite this machine's clipboard")
	}
	base := t.TempDir()
	p := filepath.Join(base, "a.png")
	f, _ := os.Create(p)
	if err := png.Encode(f, image.NewNRGBA(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := ClipboardImage(base, p); err != nil {
		t.Fatal(err)
	}
}
