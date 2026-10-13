// Package egress consumes existing gateway results; it never probes the public Internet.
package egress

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"sync"
	"time"
)

const PollInterval = 10 * time.Second
const MaxAge = 90 * time.Second

type Config struct {
	NodeID     int64  `json:"node_id"`
	URL        string `json:"url"`
	Pin        string `json:"cert_sha256"`
	Token      string `json:"token"`
	ExpectedIP string `json:"expected_ip"`
}

// Snapshot adapts existing watchdog state and its last successful heartbeat, never file mtime.
type Snapshot struct {
	CheckedAtRFC3339 string `json:"checked_at_rfc3339,omitempty"`
	Version          int    `json:"version"`
	State            string `json:"state"`
	ExpectedIP       string `json:"expected_ip"`
	ObservedIP       string `json:"observed_ip"`
	CheckedAt        int64  `json:"checked_at"`
	UpdatedAt        int64  `json:"updated_at"`
	Enforced         bool   `json:"enforced"`
	Guard            bool   `json:"guard"`
}
type Status struct {
	State      string `json:"state"`
	ExpectedIP string `json:"expected_ip"`
	ObservedIP string `json:"observed_ip,omitempty"`
	CheckedAt  int64  `json:"checked_at"`
	ValidUntil int64  `json:"valid_until"`
	Epoch      uint64 `json:"epoch"`
	Allowed    bool   `json:"allowed"`
	Reason     string `json:"reason"`
}
type entry struct {
	cfg     Config
	client  *http.Client
	status  Status
	expires time.Time
}
type Monitor struct {
	mu      sync.Mutex
	entries map[int64]*entry
	now     func() time.Time
}

func Load(path string) (*Monitor, error) {
	if path == "" {
		return New(nil)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("cannot read egress configuration")
	}
	var cfg []Config
	if len(b) > 65536 || json.Unmarshal(b, &cfg) != nil || len(cfg) == 0 {
		return nil, errors.New("invalid or empty egress configuration")
	}
	return New(cfg)
}
func New(cfg []Config) (*Monitor, error) {
	m := &Monitor{entries: map[int64]*entry{}, now: time.Now}
	if len(cfg) > 64 {
		return nil, errors.New("too many egress sources")
	}
	for _, c := range cfg {
		u, err := url.Parse(c.URL)
		ip, e := netip.ParseAddr(c.ExpectedIP)
		if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || e != nil || !ip.Is4() || c.NodeID <= 0 || len(c.Token) < 32 || len(c.Token) > 256 {
			return nil, errors.New("invalid egress source")
		}
		addr, e := netip.ParseAddr(u.Hostname())
		if e != nil || !addr.IsPrivate() || addr.Zone() != "" {
			return nil, errors.New("egress source must be a private literal IP")
		}
		pin, e := hex.DecodeString(c.Pin)
		if e != nil || len(pin) != 32 {
			return nil, errors.New("invalid egress certificate pin")
		}
		if _, ok := m.entries[c.NodeID]; ok {
			return nil, errors.New("duplicate egress node")
		}
		c.ExpectedIP = ip.String()
		tr := &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12,
			InsecureSkipVerify: true, // exact certificate pin replaces PKI validation
			VerifyConnection: func(cs tls.ConnectionState) error {
				sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
				if hex.EncodeToString(sum[:]) != hex.EncodeToString(pin) {
					return errors.New("egress pin mismatch")
				}
				return nil
			},
		}}
		client := &http.Client{Transport: tr, Timeout: 4 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("egress redirects forbidden") }}
		var seed [8]byte
		if _, err := rand.Read(seed[:]); err != nil {
			return nil, err
		}
		epoch := binary.BigEndian.Uint64(seed[:]) & ((1 << 48) - 1)
		m.entries[c.NodeID] = &entry{cfg: c, client: client, status: Status{State: "unknown", ExpectedIP: c.ExpectedIP, Epoch: epoch, Reason: "尚未取得出口结果"}}
	}
	return m, nil
}
func (m *Monitor) statusLocked(e *entry) Status {
	if e.status.Allowed && !m.now().Before(e.expires) {
		e.status.Allowed = false
		e.status.State = "unknown"
		e.status.Reason = "出口结果已过期"
		e.status.Epoch++
	}
	return e.status
}
func (m *Monitor) Status(id int64) *Status {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entries[id]
	if e == nil {
		return nil
	}
	s := m.statusLocked(e)
	return &s
}
func (m *Monitor) Allows(id int64, epoch uint64) bool {
	s := m.Status(id)
	return s == nil || (s.Allowed && s.Epoch == epoch)
}
func (m *Monitor) update(id int64, snap *Snapshot) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entries[id]
	old := m.statusLocked(e)
	now := m.now()
	s := Status{State: "unknown", ExpectedIP: e.cfg.ExpectedIP, Epoch: old.Epoch, Reason: "无法取得有效出口结果"}
	if snap != nil {
		observed, err := netip.ParseAddr(snap.ObservedIP)
		expected, eerr := netip.ParseAddr(snap.ExpectedIP)
		validTime := snap.CheckedAt > 0 && snap.UpdatedAt >= snap.CheckedAt && snap.UpdatedAt <= now.Add(5*time.Second).Unix() && snap.CheckedAt <= now.Add(5*time.Second).Unix()
		if err == nil && observed.Is4() {
			s.ObservedIP = observed.String()
		}
		if validTime {
			s.CheckedAt = snap.CheckedAt
		}
		if snap.Version == 1 && validTime && eerr == nil && expected.String() == e.cfg.ExpectedIP {
			deadline := time.Unix(snap.CheckedAt, 0).Add(MaxAge)
			s.ValidUntil = deadline.Unix()
			e.expires = deadline
			switch {
			case !now.Before(deadline):
				s.Reason = "出口结果已过期"
			case !snap.Enforced:
				s.Reason = "网关未启用隔离保护"
			case snap.Guard:
				s.State = "blocked"
				s.Reason = "网关已阻断出口"
			case s.ObservedIP != "" && s.ObservedIP != e.cfg.ExpectedIP:
				s.State = "blocked"
				s.Reason = "出口 IP 与预期不符"
			case snap.State == "healthy" && s.ObservedIP == e.cfg.ExpectedIP:
				s.State = "healthy"
				s.Reason = "出口正常"
				s.Allowed = true
			case snap.State == "degraded":
				s.State = "degraded"
				s.Reason = "探测暂时异常"
			case snap.State == "blocked":
				s.State = "blocked"
				s.Reason = "网关报告出口异常"
			}
		}
	}
	if s.Allowed != old.Allowed {
		s.Epoch++
	}
	e.status = s
}
func (m *Monitor) poll(ctx context.Context, id int64) {
	e := m.entries[id]
	req, err := http.NewRequestWithContext(ctx, "GET", e.cfg.URL, nil)
	if err != nil {
		m.update(id, nil)
		return
	}
	req.Header.Set("Authorization", "Bearer "+e.cfg.Token)
	resp, err := e.client.Do(req)
	if err != nil {
		m.update(id, nil)
		return
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
	var s Snapshot
	if err != nil || resp.StatusCode != 200 || len(b) > 4096 || json.Unmarshal(b, &s) != nil {
		m.update(id, nil)
		return
	}
	if s.CheckedAtRFC3339 != "" {
		observed, err := time.Parse(time.RFC3339, s.CheckedAtRFC3339)
		if err != nil || (s.CheckedAt != 0 && s.CheckedAt != observed.Unix()) {
			m.update(id, nil)
			return
		}
		s.CheckedAt = observed.Unix()
		if s.UpdatedAt == 0 {
			s.UpdatedAt = s.CheckedAt
		}
	}
	m.update(id, &s)
}

// Run performs bounded LAN reads even with no browser attached. UI traffic adds none.
func (m *Monitor) Run(ctx context.Context, changed func(int64)) {
	var wg sync.WaitGroup
	for id := range m.entries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			timer := time.NewTicker(PollInterval)
			defer timer.Stop()
			for {
				m.poll(ctx, id)
				if ctx.Err() != nil {
					return
				}
				changed(id)
				select {
				case <-ctx.Done():
					return
				case <-timer.C:
				}
			}
		}()
	}
	wg.Wait()
	for _, e := range m.entries {
		e.client.CloseIdleConnections()
	}
}
