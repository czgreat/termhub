package server

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"termhub/internal/hub/auth"
	"termhub/internal/hub/store"
)

func noEnv(string) string { return "" }

func testConfig(dir string, trusted string) Config {
	return Config{PublicURLs: "https://hub.lan:27443, https://term.example.com", LANListen: "127.0.0.1:0", ProxyListen: "127.0.0.1:0",
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix(trusted)}, DataDir: dir,
		Auth: auth.Config{Hash: auth.HashParams{MemoryKiB: 64, Time: 1, Threads: 1}},
		Log:  slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// pinned returns a client that accepts exactly the certificate with this fingerprint,
// the way an agent does.
func pinned(fp string) *http.Client {
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
			if hex.EncodeToString(sum[:]) != fp {
				return io.ErrUnexpectedEOF
			}
			return nil
		}}}}
}

func get(t *testing.T, c *http.Client, url string, hdr map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestFreshStartAndRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := New(testConfig(dir, "127.0.0.1/32"), noEnv)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"master.key", "termhub.db", "tls/hub.crt", "tls/hub.key"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("missing %s", f)
		}
	}
	if s.Auth.SetupToken() == "" || len(s.Fingerprint) != 64 {
		t.Fatalf("setup token %q fingerprint %q", s.Auth.SetupToken(), s.Fingerprint)
	}
	if code, body := get(t, pinned(s.Fingerprint), "https://"+s.LANAddr()+"/healthz", nil); code != 200 || body != "ok\n" {
		t.Fatalf("healthz over pinned TLS: %d %q", code, body)
	}
	if _, err := pinned(strings.Repeat("0", 64)).Get("https://" + s.LANAddr() + "/healthz"); err == nil {
		t.Fatal("a wrong pin must fail the handshake")
	}
	if err := s.Backup(); err != nil {
		t.Fatal(err)
	}
	backups, _ := filepath.Glob(filepath.Join(dir, "backups", "termhub-*.db"))
	if len(backups) != 1 {
		t.Fatalf("backups: %v", backups)
	}
	if db, err := store.Open(backups[0]); err != nil {
		t.Fatalf("the backup is not a usable database: %v", err)
	} else {
		db.Close()
	}
	if _, err := os.Stat(filepath.Join(strings.Replace(strings.TrimSuffix(backups[0], ".db"), "termhub-", "keys-", 1), "master.key")); err != nil {
		t.Error("the backup must include the master key")
	}
	fp := s.Fingerprint
	s.Close()

	// Restart: same certificate, so enrolled agents keep working.
	s2, err := New(testConfig(dir, "127.0.0.1/32"), noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Fingerprint != fp {
		t.Fatal("the certificate changed across a restart")
	}
	s2.Close()

	// The master key is gone but the database is there: refuse, and do not invent a new key.
	os.Remove(filepath.Join(dir, "master.key"))
	if _, err := New(testConfig(dir, "127.0.0.1/32"), noEnv); err == nil || !strings.Contains(err.Error(), "master.key") {
		t.Fatalf("expected a refusal naming the key, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "master.key")); err == nil {
		t.Fatal("a new master key was silently generated next to an existing database")
	}
}

func TestProxyListenerTrust(t *testing.T) {
	s, err := New(testConfig(t.TempDir(), "127.0.0.1/32"), noEnv)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	base := "http://" + s.ProxyAddr()
	if code, _ := get(t, http.DefaultClient, base+"/api/setup", nil); code != 403 {
		t.Fatalf("without X-Forwarded-Proto=https the proxy listener must refuse: %d", code)
	}
	if code, _ := get(t, http.DefaultClient, base+"/api/setup", map[string]string{"X-Forwarded-Proto": "https"}); code != 400 {
		t.Fatalf("without X-Forwarded-For there is no client address to limit or audit; must refuse: %d", code)
	}
	if code, body := get(t, http.DefaultClient, base+"/api/setup", map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-For": "203.0.113.9"}); code != 200 || !strings.Contains(body, "needed") {
		t.Fatalf("trusted proxy with https: %d %q", code, body)
	}
	if code, _ := get(t, http.DefaultClient, base+"/node/link", map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-For": "203.0.113.9"}); code != 404 {
		t.Fatalf("node endpoints must not be reachable through the public proxy: %d", code)
	}
	if code, _ := get(t, http.DefaultClient, base+"/healthz", nil); code != 200 {
		t.Fatalf("healthz must work for the proxy's own checks: %d", code)
	}
	// X-Forwarded-For: only the hop appended by the trusted proxy counts.
	req, _ := http.NewRequest("GET", base+"/", nil)
	req.RemoteAddr = "127.0.0.1:5555"
	req.Header.Set("X-Forwarded-For", "6.6.6.6, 203.0.113.9")
	var seen string
	s.proxyGate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = s.clientIP(r) })).
		ServeHTTP(discard{}, withProto(req))
	if seen != "203.0.113.9" {
		t.Fatalf("client ip %q: a client-supplied hop was believed", seen)
	}
	// A client-supplied header LINE before the proxy's own line is ignored too.
	req2, _ := http.NewRequest("GET", base+"/", nil)
	req2.RemoteAddr = "127.0.0.1:5555"
	req2.Header.Add("X-Forwarded-For", "6.6.6.6")
	req2.Header.Add("X-Forwarded-For", "203.0.113.10")
	seen = ""
	s.proxyGate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = s.clientIP(r) })).
		ServeHTTP(discard{}, withProto(req2))
	if seen != "203.0.113.10" {
		t.Fatalf("client ip %q: the first header line was believed", seen)
	}
	// On the LAN listener the header is ignored entirely.
	req3, _ := http.NewRequest("GET", "https://x/", nil)
	req3.RemoteAddr = "192.168.1.50:1234"
	req3.Header.Set("X-Forwarded-For", "6.6.6.6")
	if ip := s.clientIP(req3); ip != "192.168.1.50" {
		t.Fatalf("LAN listener believed X-Forwarded-For: %q", ip)
	}

	for _, p := range []string{"//node/link", "/x/../node/link", "/node"} {
		req, _ := http.NewRequest("GET", base+"/", nil)
		req.URL.Path = p
		req.RemoteAddr = "127.0.0.1:5555"
		req.Header.Set("X-Forwarded-For", "203.0.113.9")
		rec := &codeWriter{}
		s.proxyGate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })).ServeHTTP(rec, withProto(req))
		if rec.code != 404 {
			t.Fatalf("%s passed the node gate: %d", p, rec.code)
		}
	}

	// With a proxy key, the right source address is not enough.
	cfgKey := testConfig(t.TempDir(), "127.0.0.1/32")
	cfgKey.ProxyKey = "k3y-from-the-proxy"
	sk, err := New(cfgKey, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	defer sk.Close()
	kbase := "http://" + sk.ProxyAddr()
	hdr := map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-For": "203.0.113.9"}
	if code, _ := get(t, http.DefaultClient, kbase+"/api/setup", hdr); code != 403 {
		t.Fatalf("no proxy key must be refused: %d", code)
	}
	hdr["X-TH-Proxy-Key"] = "wrong"
	if code, _ := get(t, http.DefaultClient, kbase+"/api/setup", hdr); code != 403 {
		t.Fatalf("a wrong proxy key must be refused: %d", code)
	}
	hdr["X-TH-Proxy-Key"] = "k3y-from-the-proxy"
	if code, _ := get(t, http.DefaultClient, kbase+"/api/setup", hdr); code != 200 {
		t.Fatalf("the right proxy key must pass: %d", code)
	}
	if c, err := ConfigFromEnv(func(k string) string {
		return map[string]string{"TH_PUBLIC_URL": "https://x", "TH_PROXY_KEY": "abc"}[k]
	}); err != nil || c.ProxyKey != "abc" {
		t.Fatalf("TH_PROXY_KEY not read: %v %q", err, c.ProxyKey)
	}

	// Somebody who is not the proxy connects to the proxy port.
	s2, err := New(testConfig(t.TempDir(), "10.9.8.0/24"), noEnv)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if code, _ := get(t, http.DefaultClient, "http://"+s2.ProxyAddr()+"/healthz", map[string]string{"X-Forwarded-Proto": "https"}); code != 403 {
		t.Fatalf("an untrusted source reached the proxy listener: %d", code)
	}
}

type discard struct{}

func (discard) Header() http.Header         { return http.Header{} }
func (discard) Write(b []byte) (int, error) { return len(b), nil }
func (discard) WriteHeader(int)             {}

func withProto(r *http.Request) *http.Request { r.Header.Set("X-Forwarded-Proto", "https"); return r }

func TestConfigFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if _, err := ConfigFromEnv(env(nil)); err == nil {
		t.Error("TH_PUBLIC_URL must be required")
	}
	if _, err := ConfigFromEnv(env(map[string]string{"TH_PUBLIC_URL": "https://h", "TH_PROXY_LISTEN": ":27180"})); err == nil {
		t.Error("a proxy listener without trusted proxies must be refused")
	}
	if _, err := ConfigFromEnv(env(map[string]string{"TH_PUBLIC_URL": "https://h", "TH_TRUSTED_PROXIES": "not-an-ip"})); err == nil {
		t.Error("bad prefix accepted")
	}
	c, err := ConfigFromEnv(env(map[string]string{"TH_PUBLIC_URL": "https://h", "TH_PROXY_LISTEN": ":27180",
		"TH_TRUSTED_PROXIES": "192.168.1.1, 10.0.0.0/8", "TH_TRUST_DEVICE_DAYS": "14"}))
	if err != nil || len(c.TrustedProxies) != 2 || c.LANListen != ":27443" || c.Auth.TrustDevice.Hours() != 14*24 {
		t.Fatalf("%+v %v", c, err)
	}
	// A plain http public URL is refused when the server starts: __Host- cookies need TLS.
	bad := testConfig(t.TempDir(), "127.0.0.1/32")
	bad.PublicURLs = "http://hub.lan"
	if _, err := New(bad, noEnv); err == nil {
		t.Error("http public URL accepted")
	}
}

type codeWriter struct {
	code int
	h    http.Header
}

func (c *codeWriter) Header() http.Header {
	if c.h == nil {
		c.h = http.Header{}
	}
	return c.h
}
func (c *codeWriter) Write(b []byte) (int, error) {
	if c.code == 0 {
		c.code = 200
	}
	return len(b), nil
}
func (c *codeWriter) WriteHeader(code int) { c.code = code }
