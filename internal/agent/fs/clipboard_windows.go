package fs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/draw"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// ClipboardImage puts an image file from the paste folder on the clipboard of
// this process's window station (docs/M4 第 7 节). Claude Code reads its own
// machine's clipboard on Alt+V; since the CLI runs under this agent's session
// host, both see the same clipboard. Two formats are set: CF_DIB (what every
// Windows program reads) and the registered "PNG" format (lossless).
func ClipboardImage(base, path string) error {
	full, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	baseAbs, _ := filepath.Abs(base)
	if !strings.HasPrefix(strings.ToLower(full), strings.ToLower(baseAbs)+`\`) {
		return errors.New("clipboard: only files in the paste folder")
	}
	raw, err := os.ReadFile(full)
	if err != nil {
		return err
	}
	// The header is read first: a tiny file can claim a huge canvas, and
	// decoding it would take gigabytes and kill the agent (review 09-23).
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("clipboard: cannot read the image header: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > maxImageSide || cfg.Height > maxImageSide || cfg.Width*cfg.Height > maxImagePixels {
		return fmt.Errorf("clipboard: image too large (%d×%d)", cfg.Width, cfg.Height)
	}
	img, kind, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("clipboard: cannot decode the image: %w", err)
	}
	pngBytes := raw
	if kind != "png" {
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			return err
		}
		pngBytes = buf.Bytes()
	}
	return setClipboard(dibFromImage(img), pngBytes)
}

// dibFromImage renders the image as a packed 32-bit bottom-up DIB
// (BITMAPINFOHEADER followed by BGRA rows), the CF_DIB layout.
func dibFromImage(img image.Image) []byte {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	rgba := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(rgba, rgba.Bounds(), image.NewUniform(image.White), image.Point{}, draw.Src) // transparency over white
	draw.Draw(rgba, rgba.Bounds(), img, b.Min, draw.Over)
	out := make([]byte, 40+w*h*4)
	binary.LittleEndian.PutUint32(out[0:], 40)
	binary.LittleEndian.PutUint32(out[4:], uint32(w))
	binary.LittleEndian.PutUint32(out[8:], uint32(h)) // positive height: bottom-up
	binary.LittleEndian.PutUint16(out[12:], 1)
	binary.LittleEndian.PutUint16(out[14:], 32)
	binary.LittleEndian.PutUint32(out[16:], 0) // BI_RGB
	binary.LittleEndian.PutUint32(out[20:], uint32(w*h*4))
	binary.LittleEndian.PutUint32(out[24:], 2835) // 72 dpi
	binary.LittleEndian.PutUint32(out[28:], 2835)
	for y := 0; y < h; y++ {
		src := rgba.Pix[(h-1-y)*rgba.Stride : (h-1-y)*rgba.Stride+w*4]
		dst := out[40+y*w*4:]
		for x := 0; x < w; x++ {
			dst[x*4+0] = src[x*4+2] // B
			dst[x*4+1] = src[x*4+1] // G
			dst[x*4+2] = src[x*4+0] // R
			dst[x*4+3] = src[x*4+3] // A
		}
	}
	return out
}

var (
	user32                 = syscall.NewLazyDLL("user32.dll")
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procOpenClipboard      = user32.NewProc("OpenClipboard")
	procCloseClipboard     = user32.NewProc("CloseClipboard")
	procEmptyClipboard     = user32.NewProc("EmptyClipboard")
	procSetClipboardData   = user32.NewProc("SetClipboardData")
	procRegisterClipboardW = user32.NewProc("RegisterClipboardFormatW")
	procGlobalAlloc        = kernel32.NewProc("GlobalAlloc")
	procGlobalLock         = kernel32.NewProc("GlobalLock")
	procGlobalUnlock       = kernel32.NewProc("GlobalUnlock")
	procGlobalFree         = kernel32.NewProc("GlobalFree")
	procRtlMoveMemory      = kernel32.NewProc("RtlMoveMemory")
)

const (
	cfDIB        = 8
	gmemMoveable = 0x0002
)

func globalCopy(data []byte) (uintptr, error) {
	h, _, err := procGlobalAlloc.Call(gmemMoveable, uintptr(len(data)))
	if h == 0 {
		return 0, fmt.Errorf("GlobalAlloc: %w", err)
	}
	p, _, err := procGlobalLock.Call(h)
	if p == 0 {
		procGlobalFree.Call(h)
		return 0, fmt.Errorf("GlobalLock: %w", err)
	}
	// the locked block is addressed by a uintptr only; copy through the kernel
	procRtlMoveMemory.Call(p, uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)))
	procGlobalUnlock.Call(h)
	return h, nil
}

// Limits on what goes onto the clipboard (a screenshot is far below both).
const (
	maxImageSide   = 16384
	maxImagePixels = 40_000_000
)

func setClipboard(dib, pngBytes []byte) error {
	// Open, Set and Close must happen on the same OS thread; a goroutine may
	// otherwise be moved between them and leave the clipboard held forever.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	var opened bool
	for i := 0; i < 10; i++ { // another program may hold it for a moment
		if r, _, _ := procOpenClipboard.Call(0); r != 0 {
			opened = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !opened {
		return errors.New("clipboard: busy")
	}
	defer procCloseClipboard.Call()
	if r, _, err := procEmptyClipboard.Call(); r == 0 {
		return fmt.Errorf("EmptyClipboard: %w", err)
	}
	h, err := globalCopy(dib)
	if err != nil {
		return err
	}
	if r, _, err := procSetClipboardData.Call(cfDIB, h); r == 0 {
		procGlobalFree.Call(h)
		return fmt.Errorf("SetClipboardData(CF_DIB): %w", err)
	}
	name, _ := syscall.UTF16PtrFromString("PNG")
	if fmtPNG, _, _ := procRegisterClipboardW.Call(uintptr(unsafe.Pointer(name))); fmtPNG != 0 {
		if hp, err := globalCopy(pngBytes); err == nil {
			if r, _, _ := procSetClipboardData.Call(fmtPNG, hp); r == 0 {
				procGlobalFree.Call(hp)
			}
		}
	}
	return nil
}
