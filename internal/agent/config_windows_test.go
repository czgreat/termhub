//go:build windows

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	"termhub/internal/proto"
)

func TestConfigSealsTheToken(t *testing.T) {
	dir := t.TempDir()
	const token = "thn_super-secret-node-token-value"
	if err := Save(dir, File{HubURL: "https://hub:27443", CertPin: "ab"}, token); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(configPath(dir))
	if strings.Contains(string(raw), token) || strings.Contains(string(raw), "super-secret") {
		t.Fatalf("the token is readable in the file:\n%s", raw)
	}
	f, got, err := Load(dir)
	if err != nil || got != token || f.HubURL != "https://hub:27443" {
		t.Fatalf("load: %+v %q %v", f, got, err)
	}
	// A damaged blob must produce a clear instruction, not a crash.
	os.WriteFile(configPath(dir), []byte(`{"hub_url":"https://h","sealed_token":"AAAA"}`), 0o600)
	if _, _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "enrol again") {
		t.Fatalf("damaged token: %v", err)
	}
	if _, _, err := Load(t.TempDir()); err == nil || !strings.Contains(err.Error(), "enroll --hub") {
		t.Fatalf("missing config: %v", err)
	}
}

// fakeHub accepts the token "good" and welcomes the node.
func fakeHub(t *testing.T) (*httptest.Server, string) {
	up := websocket.Upgrader{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/node/link" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("X-TH-Node-Token") != "good" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		_, raw, err := c.ReadMessage()
		if err != nil {
			return
		}
		hello, _ := proto.DecodeMsg(raw)
		reply := hello.Reply()
		reply.T = proto.MsgWelcome
		if hello.Fingerprint == "" {
			reply = hello.Fail(proto.ErrForbidden, "no fingerprint")
		}
		b, _ := proto.EncodeMsg(reply)
		c.WriteMessage(websocket.TextMessage, b)
	}))
	t.Cleanup(srv.Close)
	sum := sha256.Sum256(srv.Certificate().Raw)
	return srv, hex.EncodeToString(sum[:])
}

func TestProbeExplainsWhatIsWrong(t *testing.T) {
	srv, pin := fakeHub(t)
	ctx := context.Background()
	good, err := TLSConfig(pin)
	if err != nil {
		t.Fatal(err)
	}
	if err := Probe(ctx, Config{HubURL: srv.URL, Token: "good", TLS: good}); err != nil {
		t.Fatalf("a correct enrolment must pass: %v", err)
	}
	cases := []struct {
		name, url, token, pin, want string
	}{
		{"wrong token", srv.URL, "bad", pin, "refused the node token"},
		{"wrong pin", srv.URL, "good", strings.Repeat("0", 64), "does not match the enrolled pin"},
		{"self-signed without pin", srv.URL, "good", "", "pass --pin"},
		{"nothing listening", "https://127.0.0.1:1", "good", pin, "cannot reach the Hub"},
		{"not https", "http://hub", "good", pin, "must start with https://"},
	}
	for _, c := range cases {
		tlsc, err := TLSConfig(c.pin)
		if err != nil {
			t.Fatal(err)
		}
		err = Probe(ctx, Config{HubURL: c.url, Token: c.token, TLS: tlsc})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want an error containing %q, got %v", c.name, c.want, err)
		}
	}
	if _, err := TLSConfig("not-hex"); err == nil {
		t.Error("a malformed pin must be refused")
	}
	if Fingerprint() == "" || len(Fingerprint()) != 64 {
		t.Errorf("machine fingerprint: %q", Fingerprint())
	}
}
