//go:build linux

package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinuxTokenSealRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "termhub")
	if err := Save(dir, File{HubURL: "https://hub:27443", CertPin: "ab"}, "thn_secret-token"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(configPath(dir))
	if strings.Contains(string(raw), "thn_secret-token") {
		t.Fatal("token stored in the clear")
	}
	for p, want := range map[string]os.FileMode{dir: 0o700, configPath(dir): 0o600} {
		if st, err := os.Stat(p); err != nil || st.Mode().Perm() != want {
			t.Fatalf("%s: %v %v", p, st.Mode().Perm(), err)
		}
	}
	f, token, err := Load(dir)
	if err != nil || token != "thn_secret-token" || f.HubURL != "https://hub:27443" {
		t.Fatalf("load: %+v %q %v", f, token, err)
	}
}

// A file other users can read is refused, as ssh refuses an open key.
func TestLinuxLoadRefusesOpenFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "termhub")
	if err := Save(dir, File{HubURL: "https://hub:27443"}, "thn_x"); err != nil {
		t.Fatal(err)
	}
	os.Chmod(configPath(dir), 0o644)
	if _, _, err := Load(dir); err == nil {
		t.Fatal("a world-readable agent.json was accepted")
	}
	os.Chmod(configPath(dir), 0o600)
	os.Chmod(dir, 0o755)
	if _, _, err := Load(dir); err == nil {
		t.Fatal("an open folder was accepted")
	}
}

// The sealed token does not open with another machine id or user: tamper
// with the ciphertext as the cheapest stand-in for "another key".
func TestLinuxSealedTokenIsAuthenticated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "termhub")
	sealed, err := sealToken(dir, []byte("thn_x"))
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(dir, 0o700)
	os.WriteFile(configPath(dir), []byte("{}"), 0o600)
	sealed[len(sealed)-1] ^= 1
	if _, err := unsealToken(dir, sealed); err == nil {
		t.Fatal("a modified token was accepted")
	}
}

func TestLinuxFingerprintAndOS(t *testing.T) {
	if fp := Fingerprint(); len(fp) != 64 {
		t.Fatalf("fingerprint %q", fp)
	}
	if osBuild() == "" {
		t.Fatal("no OS name")
	}
}
