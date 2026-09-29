// Package server assembles the Hub: configuration, data directory, master key,
// TLS, the two listeners and the modules behind them (docs/M8).
package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"termhub/internal/hub/auth"
	"termhub/internal/hub/route"
	"termhub/internal/hub/store"
	"termhub/internal/hub/web"
)

// Config is read from TH_* environment variables (docs/M8 第 5 节).
type Config struct {
	PublicURLs     string // comma separated https URLs browsers use
	LANListen      string // HTTPS, for agents and LAN browsers
	ProxyListen    string // plain HTTP, only for the trusted reverse proxy; empty = off
	TrustedProxies []netip.Prefix
	// ProxyKey, when set, must arrive as X-TH-Proxy-Key on every proxy-listener
	// request: the source address alone says only "came through the tunnel",
	// and anything that can reach the tunnel's far end would pass as the proxy
	// (复核：公网入口 1).
	ProxyKey  string
	DataDir   string
	Auth      auth.Config
	AuditKeep time.Duration
	Log       *slog.Logger
}

// ConfigFromEnv builds a Config, refusing anything ambiguous.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	c := Config{PublicURLs: getenv("TH_PUBLIC_URL"), LANListen: or(getenv("TH_LAN_LISTEN"), ":27443"),
		ProxyListen: getenv("TH_PROXY_LISTEN"), ProxyKey: getenv("TH_PROXY_KEY"), DataDir: or(getenv("TH_DATA_DIR"), "/data"), AuditKeep: 180 * 24 * time.Hour}
	if c.PublicURLs == "" {
		return c, errors.New("TH_PUBLIC_URL is required, e.g. https://192.168.1.10:27443")
	}
	for _, p := range strings.Split(getenv("TH_TRUSTED_PROXIES"), ",") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		if !strings.Contains(p, "/") {
			if a, err := netip.ParseAddr(p); err == nil {
				p = netip.PrefixFrom(a, a.BitLen()).String()
			}
		}
		pfx, err := netip.ParsePrefix(p)
		if err != nil {
			return c, fmt.Errorf("TH_TRUSTED_PROXIES: %q is not an address or prefix", p)
		}
		c.TrustedProxies = append(c.TrustedProxies, pfx)
	}
	if c.ProxyListen != "" && len(c.TrustedProxies) == 0 {
		return c, errors.New("TH_PROXY_LISTEN is set but TH_TRUSTED_PROXIES is empty: refusing to trust everyone")
	}
	days := func(key string, def int) (time.Duration, error) {
		v := getenv(key)
		if v == "" {
			return time.Duration(def) * 24 * time.Hour, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("%s: %q is not a positive number of days", key, v)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	var err error
	if c.Auth.SessionIdle, err = days("TH_SESSION_IDLE_DAYS", 7); err != nil {
		return c, err
	}
	if c.Auth.SessionMax, err = days("TH_SESSION_MAX_DAYS", 30); err != nil {
		return c, err
	}
	if c.Auth.TrustDevice, err = days("TH_TRUST_DEVICE_DAYS", 30); err != nil {
		return c, err
	}
	if c.AuditKeep, err = days("TH_AUDIT_KEEP_DAYS", 180); err != nil {
		return c, err
	}
	if c.Auth.Hash, err = auth.HashParamsFromEnv(getenv); err != nil {
		return c, err
	}
	return c, nil
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Server is a running Hub.
type Server struct {
	cfg         Config
	DB          *store.DB
	Auth        *auth.Service
	Hub         *route.Hub
	Fingerprint string // SHA-256 of the LAN certificate, what agents pin
	lan, proxy  net.Listener
	httpLAN     *http.Server
	httpProxy   *http.Server
	stop        chan struct{}
}

// loadMasterKey creates the key on a virgin data directory and refuses to
// invent a new one next to an existing database: that would silently orphan
// every sealed secret (docs/M8 验收 5).
func loadMasterKey(dir string, getenv func(string) string) ([]byte, error) {
	if v := getenv("TH_MASTER_KEY"); v != "" {
		key, err := hex.DecodeString(v)
		if err != nil || len(key) != 32 {
			return nil, errors.New("TH_MASTER_KEY must be 64 hex characters")
		}
		return key, nil
	}
	path := filepath.Join(dir, "master.key")
	key, err := os.ReadFile(path)
	if err == nil {
		if len(key) != 32 {
			return nil, fmt.Errorf("%s is damaged: expected 32 bytes", path)
		}
		return key, nil
	}
	if _, dbErr := os.Stat(filepath.Join(dir, "termhub.db")); dbErr == nil {
		return nil, fmt.Errorf("%s is missing but a database exists; restore the key from a backup. "+
			"Without it every TOTP secret and profile secret is unreadable", path)
	}
	key = make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return key, os.WriteFile(path, key, 0o600)
}

// loadCertificate returns the LAN certificate, generating a self-signed one on
// first start. Agents pin its fingerprint (docs/M8 第 6 节).
func loadCertificate(dir string, hosts []string) (tls.Certificate, string, error) {
	certPath, keyPath := filepath.Join(dir, "tls", "hub.crt"), filepath.Join(dir, "tls", "hub.key")
	if cert, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		sum := sha256.Sum256(cert.Certificate[0])
		return cert, hex.EncodeToString(sum[:]), nil
	}
	if err := os.MkdirAll(filepath.Dir(certPath), 0o700); err != nil {
		return tls.Certificate{}, "", err
	}
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "termhub"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(5, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else if h != "" {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return tls.Certificate{}, "", err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return tls.Certificate{}, "", err
	}
	return loadCertificate(dir, hosts)
}

// New opens everything and starts listening.
func New(cfg Config, getenv func(string) string) (*Server, error) {
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewJSONHandler(os.Stdout, nil))
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	key, err := loadMasterKey(cfg.DataDir, getenv)
	if err != nil {
		return nil, err
	}
	db, err := store.Open(filepath.Join(cfg.DataDir, "termhub.db"))
	if err != nil {
		return nil, err
	}
	svc, err := auth.New(db, key, cfg.Auth)
	if err != nil {
		db.Close()
		return nil, err
	}
	s := &Server{cfg: cfg, DB: db, Auth: svc, stop: make(chan struct{})}

	var hosts []string
	for _, u := range strings.Split(cfg.PublicURLs, ",") {
		if p, err := url.Parse(strings.TrimSpace(u)); err == nil {
			hosts = append(hosts, p.Hostname())
		}
	}
	cert, fp, err := loadCertificate(cfg.DataDir, hosts)
	if err != nil {
		db.Close()
		return nil, err
	}
	s.Fingerprint = fp

	webAuth, err := auth.NewHTTP(svc, cfg.PublicURLs, s.clientIP, cfg.Log)
	if err != nil {
		db.Close()
		return nil, err
	}
	seal, _ := auth.NewSealer(key)
	s.Hub = route.NewHub(route.NewRegistry(db, seal, nil), webAuth, route.Limits{}, cfg.Log)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	webAuth.Register(mux)
	s.Hub.Register(mux)
	mux.Handle("GET /api/admin/system", webAuth.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !auth.IdentityFrom(r.Context()).User.IsAdmin() {
			webAuth.Fail(w, r, auth.ErrForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"cert_fingerprint":%q,"server_time":%d}`, s.Fingerprint, time.Now().Unix())
	})))
	mux.Handle("/", web.Handler()) // everything that is not an API route is the application shell
	handler := auth.SecurityHeaders(mux)

	if s.lan, err = tls.Listen("tcp", cfg.LANListen, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}); err != nil {
		db.Close()
		return nil, err
	}
	s.httpLAN = &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	go s.httpLAN.Serve(s.lan)

	if cfg.ProxyListen != "" {
		if s.proxy, err = net.Listen("tcp", cfg.ProxyListen); err != nil {
			s.Close()
			return nil, err
		}
		s.httpProxy = &http.Server{Handler: s.proxyGate(handler), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
		go s.httpProxy.Serve(s.proxy)
	}
	go s.housekeeping()
	return s, nil
}

// LANAddr and ProxyAddr report the bound addresses (useful with port 0).
func (s *Server) LANAddr() string { return s.lan.Addr().String() }
func (s *Server) ProxyAddr() string {
	if s.proxy == nil {
		return ""
	}
	return s.proxy.Addr().String()
}

func (s *Server) trusted(addr string) bool {
	ap, err := netip.ParseAddrPort(addr)
	if err != nil {
		return false
	}
	ip := ap.Addr().Unmap()
	for _, p := range s.cfg.TrustedProxies {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

type proxiedKey struct{}

const proxyKeyHeader = "X-TH-Proxy-Key"

// proxyGate guards the plain-HTTP listener: only the trusted reverse proxy may
// connect, and it must state that the browser came in over HTTPS (docs/M8 第 6 节).
func (s *Server) proxyGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.trusted(r.RemoteAddr) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if s.cfg.ProxyKey != "" {
			if subtle.ConstantTimeCompare([]byte(r.Header.Get(proxyKeyHeader)), []byte(s.cfg.ProxyKey)) != 1 {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			r.Header.Del(proxyKeyHeader)
		}
		if r.URL.Path == "/healthz" {
			// The proxy's own probe: proves the process is up, nothing more. The
			// real check (database, disk) is on the LAN side; the public side gets
			// no error text and no disk write (复核第四轮 7).
			w.Write([]byte("ok\n"))
			return
		}
		if !strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
			http.Error(w, "this listener is only for a reverse proxy that terminates HTTPS", http.StatusForbidden)
			return
		}
		// Nodes enrol and connect over the LAN listener only (docs/M8 第 6 节);
		// the public side gets no chance to grind node tokens (安全复核 L5).
		if p := path.Clean("/" + r.URL.Path); p == "/node" || strings.HasPrefix(p, "/node/") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		// Without a usable client address there is no rate limiting and no
		// honest audit trail, so such a request is refused rather than counted
		// against the proxy's own address (安全复核 M3).
		if forwardedFor(r) == "" {
			http.Error(w, "the reverse proxy must set X-Forwarded-For", http.StatusBadRequest)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), proxiedKey{}, true)))
	})
}

// forwardedFor is the client address the trusted proxy appended: the last
// value of the LAST X-Forwarded-For header line (a client may send its own
// line first; the proxy's is always the final one). Empty when unusable.
func forwardedFor(r *http.Request) string {
	lines := r.Header.Values("X-Forwarded-For")
	if len(lines) == 0 {
		return ""
	}
	parts := strings.Split(lines[len(lines)-1], ",")
	last := strings.TrimSpace(parts[len(parts)-1])
	if ip := net.ParseIP(last); ip != nil {
		return ip.String()
	}
	return ""
}

// clientIP believes X-Forwarded-For only on the proxy listener, and there only
// the hop the trusted proxy itself appended.
func (s *Server) clientIP(r *http.Request) string {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if proxied, _ := r.Context().Value(proxiedKey{}).(bool); proxied {
		if ip := forwardedFor(r); ip != "" {
			return ip
		}
	}
	return host
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	var one int
	if err := s.DB.QueryRow(`SELECT 1`).Scan(&one); err != nil {
		s.cfg.Log.Warn("healthz: database", "err", err)
		http.Error(w, "unhealthy", http.StatusServiceUnavailable)
		return
	}
	probe := filepath.Join(s.cfg.DataDir, ".healthz")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		s.cfg.Log.Warn("healthz: data directory", "err", err)
		http.Error(w, "unhealthy", http.StatusServiceUnavailable)
		return
	}
	os.Remove(probe)
	w.Write([]byte("ok\n"))
}

// housekeeping prunes the audit log and takes the daily backup (docs/M8 第 9 节).
func (s *Server) housekeeping() {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		s.Auth.PruneAudit(s.cfg.AuditKeep)
		if err := s.Backup(); err != nil {
			s.cfg.Log.Error("backup failed", "err", err)
		}
		select {
		case <-s.stop:
			return
		case <-t.C:
		}
	}
}

// Backup writes today's consistent copy of the database and the keys, once per
// day, keeping 14.
func (s *Server) Backup() error {
	dir := filepath.Join(s.cfg.DataDir, "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	day := time.Now().Format("2006-01-02")
	dst := filepath.Join(dir, "termhub-"+day+".db")
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	if _, err := s.DB.Exec(`VACUUM INTO ?`, dst); err != nil {
		return err
	}
	keyDir := filepath.Join(dir, "keys-"+day)
	os.MkdirAll(keyDir, 0o700)
	for _, f := range []string{"master.key", filepath.Join("tls", "hub.crt"), filepath.Join("tls", "hub.key")} {
		if b, err := os.ReadFile(filepath.Join(s.cfg.DataDir, f)); err == nil {
			os.WriteFile(filepath.Join(keyDir, filepath.Base(f)), b, 0o600)
		}
	}
	entries, _ := filepath.Glob(filepath.Join(dir, "termhub-*.db"))
	for i := 0; i+14 < len(entries); i++ { // names sort by date
		os.Remove(entries[i])
		os.RemoveAll(strings.Replace(strings.TrimSuffix(entries[i], ".db"), "termhub-", "keys-", 1))
	}
	return nil
}

// Close stops listening and closes the database.
func (s *Server) Close() {
	select {
	case <-s.stop:
		return
	default:
		close(s.stop)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if s.httpLAN != nil {
		s.httpLAN.Shutdown(ctx)
	}
	if s.httpProxy != nil {
		s.httpProxy.Shutdown(ctx)
	}
	s.DB.Close()
}
