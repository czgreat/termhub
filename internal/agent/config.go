package agent

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// File is what enrolment writes to disk (docs/M5 第 3 节). The node token is
// never stored in the clear: it is sealed for the current user (config_windows.go:
// DPAPI; config_linux.go: a key bound to this machine and user).
type File struct {
	HubURL      string `json:"hub_url"`
	NodeName    string `json:"node_name,omitempty"`
	CertPin     string `json:"cert_pin,omitempty"` // SHA-256 of the Hub's certificate, hex
	SealedToken string `json:"sealed_token"`
}

func configPath(dir string) string { return filepath.Join(dir, "agent.json") }

// Save writes the configuration with the token sealed.
func Save(dir string, f File, token string) error {
	sealed, err := sealToken(dir, []byte(token))
	if err != nil {
		return fmt.Errorf("agent: cannot protect the node token: %w", err)
	}
	f.SealedToken = base64.StdEncoding.EncodeToString(sealed)
	b, _ := json.MarshalIndent(f, "", "  ")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := configPath(dir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, configPath(dir))
}

// Load reads the configuration and unseals the token.
func Load(dir string) (File, string, error) {
	var f File
	b, err := os.ReadFile(configPath(dir))
	if err != nil {
		return f, "", fmt.Errorf("agent: not enrolled (%w); run: termhub-agent enroll --hub <url> --token <token>", err)
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return f, "", fmt.Errorf("agent: %s is damaged: %w", configPath(dir), err)
	}
	sealed, err := base64.StdEncoding.DecodeString(f.SealedToken)
	if err != nil {
		return f, "", errors.New("agent: the stored token is damaged; enrol again")
	}
	token, err := unsealToken(dir, sealed)
	if err != nil {
		return f, "", errors.New(unsealHint)
	}
	return f, string(token), nil
}

// TLSConfig returns how the agent verifies the Hub. With a pin, exactly that
// certificate is accepted and nothing else, which is what a Hub with a
// self-signed LAN certificate needs. Without a pin the system roots apply.
// There is no way to switch verification off (docs/M5 第 5 节).
func TLSConfig(pin string) (*tls.Config, error) {
	pin = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(pin), ":", ""))
	if pin == "" {
		return &tls.Config{MinVersion: tls.VersionTLS12}, nil
	}
	want, err := hex.DecodeString(pin)
	if err != nil || len(want) != sha256.Size {
		return nil, errors.New("agent: the certificate pin must be a SHA-256 fingerprint (64 hex characters)")
	}
	return &tls.Config{MinVersion: tls.VersionTLS12,
		InsecureSkipVerify: true, // chain building is replaced by the exact match below, not skipped
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("agent: the Hub sent no certificate")
			}
			got := sha256.Sum256(raw[0])
			if hex.EncodeToString(got[:]) != pin {
				return fmt.Errorf("agent: the Hub's certificate (%x) does not match the enrolled pin; refusing to connect", got[:6])
			}
			return nil
		}}, nil
}
