//go:build linux

package agent

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Dir is the per-user installation directory: $XDG_DATA_HOME/termhub, by
// default ~/.local/share/termhub. TH_AGENT_DIR overrides it for tests.
func Dir() string {
	if d := os.Getenv("TH_AGENT_DIR"); filepath.IsAbs(d) {
		return d
	}
	base := os.Getenv("XDG_DATA_HOME")
	if !filepath.IsAbs(base) {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "termhub")
}

// Linux has no DPAPI. The token is sealed with AES-256-GCM under a key
// derived from this machine's id and this user's uid. That binds the file to
// this machine and user: a copy in a backup or on another machine does not
// open. It is not a secret against this user or root, who can derive the same
// key; against other local users the protection is the file mode, which Load
// insists on (0700 folder, 0600 file, owned by this user), as for ~/.ssh.
func sealKey() ([]byte, error) {
	var id string
	for _, p := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		if b, err := os.ReadFile(p); err == nil && len(strings.TrimSpace(string(b))) >= 16 {
			id = strings.TrimSpace(string(b))
			break
		}
	}
	if id == "" {
		return nil, errors.New("no machine id (/etc/machine-id)")
	}
	k := sha256.Sum256([]byte("termhub-seal-v1\x00" + id + "\x00" + strconv.Itoa(os.Getuid())))
	return k[:], nil
}

func sealToken(dir string, token []byte) ([]byte, error) {
	if len(token) == 0 {
		return nil, errors.New("agent: nothing to protect")
	}
	key, err := sealKey()
	if err != nil {
		return nil, err
	}
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	nonce := make([]byte, gcm.NonceSize())
	rand.Read(nonce)
	return gcm.Seal(nonce, nonce, token, []byte("termhub-agent-token")), nil
}

func unsealToken(dir string, sealed []byte) ([]byte, error) {
	if err := private(dir, true); err != nil {
		return nil, err
	}
	if err := private(configPath(dir), false); err != nil {
		return nil, err
	}
	key, err := sealKey()
	if err != nil {
		return nil, err
	}
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	if len(sealed) < gcm.NonceSize() {
		return nil, errors.New("sealed token too short")
	}
	return gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], []byte("termhub-agent-token"))
}

// private checks that p belongs to this user and nobody else can read it.
func private(p string, dir bool) error {
	st, err := os.Lstat(p)
	if err != nil {
		return err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	switch {
	case !ok || int(sys.Uid) != os.Getuid():
		return fmt.Errorf("%s does not belong to this user", p)
	case dir != st.IsDir() || st.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("%s is not a plain %s", p, map[bool]string{true: "folder", false: "file"}[dir])
	case st.Mode().Perm()&0o077 != 0:
		return fmt.Errorf("%s is open to other users (mode %o); run: chmod go-rwx %s", p, st.Mode().Perm(), p)
	}
	return nil
}

var unsealHint = "agent: the stored token cannot be unsealed by this user on this machine (the files must be this user's, mode 0700/0600); enrol again as the user that runs the agent"
