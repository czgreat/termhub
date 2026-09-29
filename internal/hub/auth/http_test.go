package auth

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// browser is a cookie-keeping client that behaves like a page served from the
// Hub's own origin unless told otherwise.
type browser struct {
	t      *testing.T
	c      *http.Client
	base   string
	origin string
	csrf   string
}

func newBrowser(t *testing.T, srv *httptest.Server) *browser {
	jar, _ := cookiejar.New(nil)
	c := srv.Client()
	c.Jar = jar
	return &browser{t: t, c: c, base: srv.URL, origin: srv.URL}
}

func (b *browser) do(method, path string, body any) (int, map[string]any, http.Header) {
	b.t.Helper()
	var rd *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, b.base+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if b.origin != "" {
		req.Header.Set("Origin", b.origin)
	}
	if b.csrf != "" {
		req.Header.Set(headerCSRF, b.csrf)
	}
	resp, err := b.c.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out, resp.Header
}

func newHTTPFixture(t *testing.T) (*Service, *clock, *httptest.Server, *browser) {
	s, ck := newService(t)
	mux := http.NewServeMux()
	srv := httptest.NewTLSServer(SecurityHeaders(mux))
	t.Cleanup(srv.Close)
	h, err := NewHTTP(s, srv.URL, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.Register(mux)
	mux.Handle("GET /api/other", h.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"who": IdentityFrom(r.Context()).User.Username})
	})))
	mux.Handle("POST /api/other", h.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]bool{"ok": true})
	})))
	return s, ck, srv, newBrowser(t, srv)
}

func TestHTTPFirstLoginFlow(t *testing.T) {
	s, ck, _, b := newHTTPFixture(t)
	if code, out, _ := b.do("GET", "/api/setup", nil); code != 200 || out["needed"] != true {
		t.Fatalf("setup needed: %d %v", code, out)
	}
	if code, _, _ := b.do("POST", "/api/setup", map[string]string{"token": s.SetupToken(), "username": "root", "password": goodPw}); code != 200 {
		t.Fatalf("setup: %d", code)
	}
	if code, _, _ := b.do("POST", "/api/setup", map[string]string{"token": "x", "username": "r2", "password": goodPw}); code != 404 {
		t.Fatalf("setup must be closed afterwards: %d", code)
	}
	code, out, hdr := b.do("POST", "/api/auth/login", map[string]string{"username": "root", "password": goodPw})
	if code != 200 || out["done"] != true {
		t.Fatalf("login: %d %v", code, out)
	}
	sc := hdr.Get("Set-Cookie")
	for _, want := range []string{cookieSession + "=", "HttpOnly", "Secure", "SameSite=Lax", "Path=/"} {
		if !strings.Contains(sc, want) {
			t.Errorf("session cookie lacks %q: %s", want, sc)
		}
	}
	_, me, _ := b.do("GET", "/api/me", nil)
	b.csrf, _ = me["csrf"].(string)
	if pend, _ := me["pending"].([]any); len(pend) != 1 || pend[0] != "enroll_totp" || b.csrf == "" {
		t.Fatalf("me: %v", me)
	}
	// Forced steps: everything else is closed until TOTP is enrolled.
	if code, out, _ := b.do("GET", "/api/other", nil); code != 403 || out["code"] != "steps_pending" {
		t.Fatalf("other endpoint during first login: %d %v", code, out)
	}
	_, begin, _ := b.do("POST", "/api/me/totp/begin", map[string]string{})
	if !strings.HasPrefix(begin["uri"].(string), "otpauth://totp/") {
		t.Fatalf("begin: %v", begin)
	}
	secret := pendingSecret(t, s, &Identity{User: User{ID: 1}})
	code, conf, _ := b.do("POST", "/api/me/totp/confirm", map[string]string{"code": totpCode(secret, ck.now().Unix()/30)})
	if codes, _ := conf["recovery_codes"].([]any); code != 200 || len(codes) != 10 {
		t.Fatalf("confirm: %d %v", code, conf)
	}
	if code, out, _ := b.do("GET", "/api/other", nil); code != 200 || out["who"] != "root" {
		t.Fatalf("after enrolment: %d %v", code, out)
	}
	// Second factor and trusted device over HTTP.
	jar, _ := cookiejar.New(nil)
	c2 := *b.c // same TLS trust, separate cookies: another browser
	c2.Jar = jar
	b2 := &browser{t: t, c: &c2, base: b.base, origin: b.origin}
	ck.add(31 * time.Second)
	_, step1, _ := b2.do("POST", "/api/auth/login", map[string]string{"username": "root", "password": goodPw})
	if step1["done"] != false || step1["ticket"] == nil {
		t.Fatalf("second browser must get a ticket: %v", step1)
	}
	code, _, hdr = b2.do("POST", "/api/auth/totp", map[string]any{"ticket": step1["ticket"], "code": code6(secret, ck), "trust": true, "deviceName": "laptop"})
	if code != 200 || !strings.Contains(strings.Join(hdr.Values("Set-Cookie"), ";"), cookieDevice+"=") {
		t.Fatalf("totp step: %d %v", code, hdr.Values("Set-Cookie"))
	}
	b2.do("POST", "/api/auth/logout", nil) // no csrf yet: refused, harmless
	_, again, _ := b2.do("POST", "/api/auth/login", map[string]string{"username": "root", "password": goodPw})
	if again["done"] != true {
		t.Fatalf("trusted device should skip the second factor: %v", again)
	}
}

func code6(secret []byte, ck *clock) string { return totpCode(secret, ck.now().Unix()/30) }

func TestHTTPCSRFAndOrigin(t *testing.T) {
	s, _, _, b := newHTTPFixture(t)
	s.Setup(s.SetupToken(), "root", goodPw, "")
	s.db.Exec(`UPDATE users SET totp_confirmed_at=1, totp_secret=x'00'`) // skip enrolment for this test
	// A page on another origin cannot even log in.
	b.origin = "https://evil.example"
	if code, _, _ := b.do("POST", "/api/auth/login", map[string]string{"username": "root", "password": goodPw}); code != 403 {
		t.Fatalf("cross-origin login: %d", code)
	}
	b.origin = ""
	if code, _, _ := b.do("POST", "/api/auth/login", map[string]string{"username": "root", "password": goodPw}); code != 403 {
		t.Fatalf("login without Origin: %d", code)
	}
	b.origin = b.base
	// A form post (not JSON) is refused even from the right origin.
	req, _ := http.NewRequest("POST", b.base+"/api/auth/login", strings.NewReader("username=root&password=x"))
	req.Header.Set("Origin", b.origin)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if resp, _ := b.c.Do(req); resp.StatusCode != 400 {
		t.Fatalf("form-encoded login: %d", resp.StatusCode)
	}

	// Get a session the direct way (the user has a fake TOTP, so plant a trusted session).
	u, _ := s.userByID(1)
	tok, _ := s.newSession(u, "", "", "")
	req, _ = http.NewRequest("GET", b.base+"/", nil)
	b.c.Jar.SetCookies(req.URL, []*http.Cookie{{Name: cookieSession, Value: tok, Path: "/", Secure: true}})
	_, me, _ := b.do("GET", "/api/me", nil)
	csrf, _ := me["csrf"].(string)

	if code, out, _ := b.do("POST", "/api/other", map[string]string{}); code != 403 || out["code"] != "csrf" {
		t.Fatalf("write without csrf token: %d %v", code, out)
	}
	b.csrf = "wrong"
	if code, _, _ := b.do("POST", "/api/other", map[string]string{}); code != 403 {
		t.Fatalf("write with wrong csrf token: %d", code)
	}
	b.csrf, b.origin = csrf, "https://evil.example"
	if code, _, _ := b.do("POST", "/api/other", map[string]string{}); code != 403 {
		t.Fatalf("write from another origin with a stolen token: %d", code)
	}
	b.origin = b.base
	if code, _, _ := b.do("POST", "/api/other", map[string]string{}); code != 200 {
		t.Fatalf("legitimate write: %d", code)
	}
	if code, _, _ := b.do("GET", "/api/other", nil); code != 200 {
		t.Fatalf("reads need no csrf token: %d", code)
	}
	// Admin writes need reverification; the error code lets the UI prompt for it.
	if code, out, _ := b.do("POST", "/api/admin/users", map[string]string{"username": "alice", "role": "user"}); code != 403 || out["code"] != "reverify_required" {
		t.Fatalf("admin write: %d %v", code, out)
	}
}

func TestHTTPWebSocketGate(t *testing.T) {
	s, _, srv, _ := newHTTPFixture(t)
	s.Setup(s.SetupToken(), "root", goodPw, "")
	s.db.Exec(`UPDATE users SET totp_confirmed_at=1, totp_secret=x'00'`)
	u, _ := s.userByID(1)
	tok, _ := s.newSession(u, "", "", "")
	h, _ := NewHTTP(s, srv.URL, nil, nil)
	mk := func(origin, cookie string) *http.Request {
		r := httptest.NewRequest("GET", "/ws/events", nil)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: cookieSession, Value: cookie})
		}
		return r
	}
	if _, err := h.AuthenticateWS(mk(srv.URL, tok)); err != nil {
		t.Fatalf("legitimate upgrade: %v", err)
	}
	if _, err := h.AuthenticateWS(mk("https://evil.example", tok)); !is(err, ErrForbidden) {
		t.Fatalf("cross-site WebSocket with the victim's cookie: %v", err)
	}
	if _, err := h.AuthenticateWS(mk("", tok)); !is(err, ErrForbidden) {
		t.Fatalf("upgrade without Origin: %v", err)
	}
	if _, err := h.AuthenticateWS(mk(srv.URL, "")); !is(err, ErrUnauthenticated) {
		t.Fatalf("upgrade without a session: %v", err)
	}
}

func TestHTTPSecurityHeadersAndErrors(t *testing.T) {
	_, _, _, b := newHTTPFixture(t)
	code, out, hdr := b.do("GET", "/api/me", nil)
	if code != 401 || out["code"] != "unauthorized" {
		t.Fatalf("unauthenticated: %d %v", code, out)
	}
	csp := hdr.Get("Content-Security-Policy")
	for _, want := range []string{"frame-ancestors 'none'", "script-src 'self'", "connect-src 'self'", "object-src 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP lacks %q", want)
		}
	}
	if strings.Contains(strings.Split(csp, "style-src")[0], "unsafe-inline") {
		t.Error("scripts must not allow unsafe-inline")
	}
	if hdr.Get("X-Frame-Options") != "DENY" || hdr.Get("X-Content-Type-Options") != "nosniff" || hdr.Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("headers: %v", hdr)
	}
	if _, err := NewHTTP(nil, "http://hub.lan", nil, nil); err == nil {
		t.Error("a plain http public URL must be refused: __Host- cookies need TLS")
	}
}
