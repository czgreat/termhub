package auth

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Authentication regression coverage: what one address can do without
// logging in, stolen device cookies, the limiter at capacity, and revocation.

func auditCount(t *testing.T, s *Service, event, result string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE event=? AND result=?`, event, result).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Junk from one address (empty or oversized input, refused attempts) is
// audited once per window, not once per request, and invented usernames do
// not get limiter entries of their own (复核第四轮 1、2).
func TestJunkLoginsCostNothing(t *testing.T) {
	s, ck := newService(t)
	enrolledAdmin(t, s, ck)
	for i := 0; i < 200; i++ {
		s.Login("", "x", "5.5.5.5", "", "")
		s.Login(strings.Repeat("a", 65), "x", "5.5.5.5", "", "")
	}
	if n := auditCount(t, s, "login", "failed"); n > 2 {
		t.Fatalf("%d 'failed' audit rows for junk from one address, want at most 2", n)
	}
	if n := auditCount(t, s, "login", "rate_limited"); n > 1 {
		t.Fatalf("%d 'rate_limited' audit rows for one address in one window, want 1", n)
	}
	// invented names from many addresses: no per-username keys
	before := len(s.limit.entries)
	for i := 0; i < 50; i++ {
		s.Login(fmt.Sprintf("nobody%d", i), "wrong password!!", fmt.Sprintf("6.6.%d.1", i), "", "")
	}
	users := 0
	s.limit.mu.Lock()
	for k := range s.limit.entries {
		if strings.HasPrefix(k, "user:nobody") {
			users++
		}
	}
	s.limit.mu.Unlock()
	if users != 0 {
		t.Fatalf("%d limiter entries for usernames that do not exist (table had %d)", users, before)
	}
}

// A stolen trusted-device cookie is no licence to guess slowly: the first
// wrong password tried with it forfeits the device even while the username
// is not blocked (复核第四轮 3).
func TestDeviceForfeitedOnFirstWrongPassword(t *testing.T) {
	s, ck := newService(t)
	secret, _ := enrolledAdmin(t, s, ck)
	r, _ := s.Login("root", goodPw, "1.1.1.1", "", "")
	_, dev, err := s.CompleteTOTP(r.Ticket, code(secret, ck), true, "laptop", "1.1.1.1", "")
	if err != nil || dev == "" {
		t.Fatal(err)
	}
	if _, err := s.Login("root", "wrong password!!", "7.7.7.7", "", dev); err != ErrInvalidCredentials {
		t.Fatalf("one wrong password: %v", err)
	}
	if res, err := s.Login("root", goodPw, "7.7.7.8", "", dev); err != nil || res.SessionToken != "" || res.Ticket == "" {
		t.Fatalf("after one wrong password the device must be gone (second factor again): %+v %v", res, err)
	}
}

// At capacity the limiter admits new keys by evicting unblocked ones, and a
// blocked key stays blocked (复核第四轮 2).
func TestLimiterEvictsAtCapacity(t *testing.T) {
	ck := &clock{t: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)}
	l := newLimiter(ck.now)
	for i := 0; i <= limitFailures; i++ {
		l.try("user:victim", limitFailures)
	}
	if !l.blocked("user:victim") {
		t.Fatal("setup: victim should be blocked")
	}
	for i := 0; len(l.entries) < limitMaxEntries; i++ {
		l.try(fmt.Sprintf("ip:spray%d", i), 30)
	}
	if !l.try("ip:newcomer", 30) {
		t.Fatal("a new key must be admitted when the table is full")
	}
	if len(l.entries) > limitMaxEntries {
		t.Fatalf("table grew past the bound: %d", len(l.entries))
	}
	if !l.blocked("user:victim") {
		t.Fatal("eviction must never lift a block")
	}
	for i := 0; i < 1000; i++ { // keep spraying: still bounded, still blocked
		l.try(fmt.Sprintf("ip:more%d", i), 30)
	}
	if len(l.entries) > limitMaxEntries || !l.blocked("user:victim") {
		t.Fatal("bound or block lost under a continued spray")
	}
}

// Revoking a trusted device ends the logins made from it; a user can list
// and end their own logins (复核第四轮 5).
func TestRevokeDeviceEndsItsLogins(t *testing.T) {
	s, ck := newService(t)
	secret, _ := enrolledAdmin(t, s, ck)
	r, _ := s.Login("root", goodPw, "1.1.1.1", "ua", "")
	sessA, dev, err := s.CompleteTOTP(r.Ticket, code(secret, ck), true, "phone", "1.1.1.1", "ua")
	if err != nil || dev == "" {
		t.Fatal(err)
	}
	// a second login from that device, password only
	res, err := s.Login("root", goodPw, "1.1.1.2", "ua", dev)
	if err != nil || res.SessionToken == "" {
		t.Fatalf("device login: %v", err)
	}
	sessB := res.SessionToken
	// and one from elsewhere, with the second factor
	ck.add(31 * time.Second)
	r, _ = s.Login("root", goodPw, "2.2.2.2", "ua", "")
	sessC, _, err := s.CompleteTOTP(r.Ticket, code(secret, ck), false, "", "2.2.2.2", "ua")
	if err != nil {
		t.Fatal(err)
	}
	idC, err := s.Authenticate(sessC, "2.2.2.2")
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.ListLoginSessions(idC) // the enrolment login, A, B and C
	if err != nil || len(list) != 4 {
		t.Fatalf("login sessions: %+v %v", list, err)
	}
	current := 0
	for _, l := range list {
		if l.Current {
			current++
		}
	}
	if current != 1 {
		t.Fatalf("%d sessions marked current, want 1", current)
	}
	devs, _ := s.ListDevices(idC.User.ID)
	if err := s.RevokeDevice(idC, devs[0].ID); err != nil {
		t.Fatal(err)
	}
	for _, sess := range []string{sessA, sessB} {
		if _, err := s.Authenticate(sess, "1.1.1.1"); err == nil {
			t.Fatal("a login made from the revoked device must be over")
		}
	}
	if _, err := s.Authenticate(sessC, "2.2.2.2"); err != nil {
		t.Fatalf("the login from elsewhere must survive: %v", err)
	}
	// ending one's own login by id
	list, _ = s.ListLoginSessions(idC)
	if len(list) != 2 { // the enrolment login and C
		t.Fatalf("after revocation: %+v", list)
	}
	for _, l := range list {
		if l.Current {
			if err := s.RevokeLoginSession(idC, l.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := s.Authenticate(sessC, "2.2.2.2"); err == nil {
		t.Fatal("revoked login session still valid")
	}
}

// A first user made from the command line closes the web setup (复核第四轮 8).
func TestSetupClosedByCommandLineUser(t *testing.T) {
	s, _ := newService(t)
	tok := s.SetupToken()
	if tok == "" {
		t.Fatal("setup should be open on an empty database")
	}
	if _, err := s.CreateUserLocal("first", "admin"); err != nil {
		t.Fatal(err)
	}
	if s.SetupToken() != "" {
		t.Fatal("setup must be closed once a user exists")
	}
	if err := s.Setup(tok, "second", "a long enough password", "1.1.1.1"); err != ErrSetupClosed {
		t.Fatalf("setup with the old token: %v", err)
	}
}
