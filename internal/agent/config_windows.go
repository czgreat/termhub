//go:build windows

package agent

import (
	"errors"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Dir is the per-user installation directory.
func Dir() string { return filepath.Join(os.Getenv("LOCALAPPDATA"), "termhub") }

func dpapi(in []byte, protect bool) ([]byte, error) {
	if len(in) == 0 {
		return nil, errors.New("agent: nothing to protect")
	}
	blob := windows.DataBlob{Size: uint32(len(in)), Data: &in[0]}
	var out windows.DataBlob
	var err error
	if protect {
		err = windows.CryptProtectData(&blob, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out)
	} else {
		err = windows.CryptUnprotectData(&blob, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out)
	}
	if err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}

// sealToken and unsealToken use DPAPI for the current Windows user, so the
// file is useless to anyone else, on this machine or another.
func sealToken(_ string, token []byte) ([]byte, error) { return dpapi(token, true) }

func unsealToken(_ string, sealed []byte) ([]byte, error) { return dpapi(sealed, false) }

const unsealHint = "agent: the stored token cannot be unsealed by this Windows user; enrol again as the user that runs the agent"
