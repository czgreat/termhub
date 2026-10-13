package egress

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) (*Monitor, *Snapshot, *time.Time) {
	t.Helper()
	m, e := New([]Config{{NodeID: 1, URL: "https://192.168.1.1/status", Pin: strings.Repeat("0", 64), Token: strings.Repeat("k", 32), ExpectedIP: "203.0.113.1"}})
	if e != nil {
		t.Fatal(e)
	}
	now := time.Unix(1000, 0)
	m.now = func() time.Time { return now }
	s := &Snapshot{Version: 1, State: "healthy", ExpectedIP: "203.0.113.1", ObservedIP: "203.0.113.1", CheckedAt: 1000, UpdatedAt: 1000, Enforced: true}
	return m, s, &now
}
func TestFailClosedTransitions(t *testing.T) {
	m, s, now := fixture(t)
	if m.Status(1).Allowed || !m.Allows(2, 0) {
		t.Fatal("startup/default policy")
	}
	m.update(1, s)
	epoch := m.Status(1).Epoch
	if !m.Allows(1, epoch) {
		t.Fatal("healthy refused")
	}
	*now = now.Add(MaxAge)
	if m.Allows(1, epoch) {
		t.Fatal("expiry needs no poll")
	}
	s.CheckedAt = now.Unix()
	s.UpdatedAt = s.CheckedAt
	m.update(1, s)
	if !m.Status(1).Allowed || m.Allows(1, epoch) {
		t.Fatal("old queue revived on recovery")
	}
	m.update(1, nil)
	if m.Status(1).Allowed {
		t.Fatal("failed read stayed green")
	}
}
func TestSnapshotValidation(t *testing.T) {
	for _, test := range []struct {
		name   string
		modify func(*Snapshot)
	}{
		{"wrong IP", func(s *Snapshot) { s.ObservedIP = "203.0.113.2" }},
		{"wrong policy", func(s *Snapshot) { s.ExpectedIP = "203.0.113.2" }},
		{"future", func(s *Snapshot) { s.CheckedAt = 1100; s.UpdatedAt = 1100 }},
		{"stale", func(s *Snapshot) { s.CheckedAt = 900 }},
		{"unknown", func(s *Snapshot) { s.State = "unknown" }},
		{"guard", func(s *Snapshot) { s.Guard = true }},
		{"observe only", func(s *Snapshot) { s.Enforced = false }},
		{"invalid IP", func(s *Snapshot) { s.ObservedIP = "bad" }},
		{"local failure", func(s *Snapshot) { s.State = "blocked" }},
		{"degraded", func(s *Snapshot) { s.State = "degraded" }},
		{"schema", func(s *Snapshot) { s.Version = 2 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			m, s, _ := fixture(t)
			test.modify(s)
			m.update(1, s)
			if m.Status(1).Allowed {
				t.Fatal("unsafe snapshot accepted")
			}
		})
	}
}
func TestSourceNoRedirectPinAndBoundedBody(t *testing.T) {
	m, s, _ := fixture(t)
	mode := "ok"
	targetHits := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetHits++ }))
	defer target.Close()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("k", 32) {
			t.Error("missing auth")
		}
		switch mode {
		case "redirect":
			http.Redirect(w, r, target.URL, 302)
		case "large":
			w.Write([]byte(strings.Repeat("x", 4097)))
		default:
			if mode == "rfc3339" {
				copy := *s
				copy.CheckedAtRFC3339 = time.Unix(s.CheckedAt, 0).UTC().Format(time.RFC3339)
				copy.CheckedAt = 0
				copy.UpdatedAt = 0
				json.NewEncoder(w).Encode(copy)
			} else {
				json.NewEncoder(w).Encode(s)
			}
		}
	}))
	defer srv.Close()
	e := m.entries[1]
	e.cfg.URL = srv.URL
	sum := sha256.Sum256(srv.Certificate().Raw)
	c := e.cfg
	c.URL = "https://192.168.1.1/status"
	c.Pin = hex.EncodeToString(sum[:])
	pinned, err := New([]Config{c})
	if err != nil {
		t.Fatal(err)
	}
	e.client = pinned.entries[1].client
	m.poll(context.Background(), 1)
	if !m.Status(1).Allowed {
		t.Fatal("pinned response rejected")
	}
	mode = "rfc3339"
	m.poll(context.Background(), 1)
	if !m.Status(1).Allowed || m.Status(1).CheckedAt != s.CheckedAt {
		t.Fatal("heartbeat timestamp lost")
	}
	for _, value := range []string{"redirect", "large"} {
		mode = value
		m.poll(context.Background(), 1)
		if m.Status(1).Allowed {
			t.Fatal(value)
		}
	}
	if targetHits != 0 {
		t.Fatal("redirect followed")
	}
}
func TestSourceConfiguration(t *testing.T) {
	for _, url := range []string{"http://192.168.1.1/status", "https://example.com/status", "https://127.0.0.1/status", "https://192.168.1.1/status?token=x", "https://u:p@192.168.1.1/status"} {
		_, err := New([]Config{{NodeID: 1, URL: url, Pin: strings.Repeat("0", 64), Token: strings.Repeat("k", 32), ExpectedIP: "203.0.113.1"}})
		if err == nil {
			t.Fatal(url)
		}
	}
}
