//go:build windows

package agent

import (
	"crypto/sha256"
	"encoding/hex"

	"golang.org/x/sys/windows/registry"
)

// Fingerprint identifies this Windows installation without revealing the
// underlying identifier (docs/总体设计 8.3).
func Fingerprint() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Cryptography`, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return ""
	}
	defer k.Close()
	guid, _, err := k.GetStringValue("MachineGuid")
	if err != nil {
		return ""
	}
	sum := sha256.Sum256([]byte("termhub:" + guid))
	return hex.EncodeToString(sum[:])
}

// osBuild: the Hub has always shown Windows nodes without one.
func osBuild() string { return "" }
