//go:build linux

package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
)

// Fingerprint identifies this Linux installation without revealing the
// underlying identifier (docs/总体设计 8.3): the systemd machine id, which
// stays with the installation and changes when it is cloned and re-set up.
func Fingerprint() string {
	for _, p := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		if b, err := os.ReadFile(p); err == nil {
			if id := strings.TrimSpace(string(b)); id != "" {
				sum := sha256.Sum256([]byte("termhub-fp-v1:" + id))
				return hex.EncodeToString(sum[:])
			}
		}
	}
	return ""
}

// osBuild names the distribution for the node list, e.g. "Ubuntu 26.04.1 LTS".
func osBuild() string {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return "Linux"
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
			if v = strings.Trim(strings.TrimSpace(v), `"'`); v != "" && len(v) <= 64 {
				return v
			}
		}
	}
	return "Linux"
}
