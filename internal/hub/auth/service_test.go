package auth

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"termhub/internal/hub/store"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

const goodPw = "correct horse battery"

func newService(t *testing.T) (*Service, *clock) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ck := &clock{t: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)}
	s, err := New(db, make([]byte, 32), Config{Hash: HashParams{MemoryKiB: 64, Time: 1, Threads: 1}, Now: ck.now})
	if err != nil {
		t.Fatal(err)
	}
	return s, ck
}

func is(err error, want *Error) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == want.Code
}

// enrolledAdmin runs the whole first-login path and returns the TOTP secret.
func enrolledAdmin(t *testing.T, s *Service, ck *clock) (secret []byte, id *Identity) {
	t.Helper()
	if err := s.Setup(s.SetupToken(), "root", goodPw, "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	r, err := s.Login("root", goodPw, "1.1.1.1", "ua", "")
	if err != nil || r.SessionToken == "" {
		t.Fatalf("first login: %+v %v", r, err)
	}
	id, err = s.Authenticate(r.SessionToken, "1.1.1.1")
	if err != nil {
		t.Fatal(err)
	}
	text, uri, err := s.TOTPBegin(id)
	if err != nil || !strings.Contains(uri, text) || !strings.HasPrefix(uri, "otpauth://totp/termhub:root?") {
		t.Fatalf("totp begin: %q %q %v", text, uri, err)
	}
	secret = pendingSecret(t, s, id)
	codes, err := s.TOTPConfirm(id, totpCode(secret, ck.now().Unix()/30))
	if err != nil || len(codes) != 10 {
		t.Fatalf("confirm: %v %v", codes, err)
	}
	ck.add(31 * time.Second) // the confirming code is spent; move to a fresh step
	id, _ = s.Authenticate(r.SessionToken, "1.1.1.1")
	lastAdminToken = r.SessionToken
	return secret, id
}

func pendingSecret(t *testing.T, s *Service, id *Identity) []byte {
	t.Helper()
	var hexSealed string
	if err := s.db.QueryRow(`SELECT value FROM settings WHERE key LIKE 'totp_pending:%'`).Scan(&hexSealed); err != nil {
		t.Fatal(err)
	}
	var sealed []byte
	if _, err := fmt.Sscanf(hexSealed, "%x", &sealed); err != nil {
		t.Fatal(err)
	}
	secret, err := s.seal.Open(sealed, fmt.Sprintf("users.totp_pending:%d", id.User.ID))
	if err != nil {
		t.Fatal(err)
	}
	return secret
}

func code(secret []byte, ck *clock) string { return totpCode(secret, ck.now().Unix()/30) }

func TestSetupOnlyOnce(t *testing.T) {
	s, _ := newService(t)
	tok := s.SetupToken()
	if tok == "" {
		t.Fatal("no setup token on an empty database")
	}
	if err := s.Setup("wrong", "root", goodPw, ""); !is(err, ErrSetupClosed) {
		t.Fatalf("wrong token: %v", err)
	}
	if err := s.Setup(tok, "root", "short", ""); !is(err, ErrWeakPassword) {
		t.Fatalf("weak password: %v", err)
	}
	if err := s.Setup(tok, "root", "root", ""); !is(err, ErrWeakPassword) {
		t.Fatalf("password equal to username: %v", err)
	}
	if err := s.Setup(tok, "root", goodPw, ""); err != nil {
		t.Fatal(err)
	}
	if s.SetupToken() != "" {
		t.Fatal("setup must close")
	}
	if err := s.Setup(tok, "other", goodPw, ""); !is(err, ErrSetupClosed) {
		t.Fatalf("second setup: %v", err)
	}
}

func TestFirstLoginStepsAndSecondFactor(t *testing.T) {
	s, ck := newService(t)
	secret, id := enrolledAdmin(t, s, ck)
	if len(id.Pending) != 0 || !id.User.TOTPConfirmed {
		t.Fatalf("after enrolment: %+v", id)
	}
	// From now on a password alone yields a ticket, not a session.
	r, err := s.Login("root", goodPw, "2.2.2.2", "ua", "")
	if err != nil || r.SessionToken != "" || r.Ticket == "" {
		t.Fatalf("login with 2fa: %+v %v", r, err)
	}
	if _, _, err := s.CompleteTOTP(r.Ticket, "000000", false, "", "2.2.2.2", "ua"); !is(err, ErrTOTPInvalid) {
		t.Fatalf("wrong code: %v", err)
	}
	c := code(secret, ck)
	sess, dev, err := s.CompleteTOTP(r.Ticket, c, false, "", "2.2.2.2", "ua")
	if err != nil || sess == "" || dev != "" {
		t.Fatalf("right code: %q %q %v", sess, dev, err)
	}
	// The same code must not work a second time, even on a fresh ticket.
	r2, _ := s.Login("root", goodPw, "2.2.2.2", "ua", "")
	if _, _, err := s.CompleteTOTP(r2.Ticket, c, false, "", "2.2.2.2", "ua"); !is(err, ErrTOTPInvalid) {
		t.Fatalf("replayed code accepted: %v", err)
	}
	// A spent ticket is gone.
	if _, _, err := s.CompleteTOTP(r.Ticket, c, false, "", "", ""); !is(err, ErrTicketInvalid) {
		t.Fatalf("reused ticket: %v", err)
	}
	// Five wrong codes void a ticket.
	r3, _ := s.Login("root", goodPw, "2.2.2.2", "ua", "")
	for i := 0; i < 5; i++ {
		s.CompleteTOTP(r3.Ticket, "111111", false, "", "", "")
	}
	ck.add(31 * time.Second)
	if _, _, err := s.CompleteTOTP(r3.Ticket, code(secret, ck), false, "", "", ""); !is(err, ErrTicketInvalid) {
		t.Fatalf("ticket should be void after 5 failures: %v", err)
	}
	// Tickets expire.
	r4, _ := s.Login("root", goodPw, "2.2.2.2", "ua", "")
	ck.add(6 * time.Minute)
	if _, _, err := s.CompleteTOTP(r4.Ticket, code(secret, ck), false, "", "", ""); !is(err, ErrTicketInvalid) {
		t.Fatalf("expired ticket: %v", err)
	}
}

func TestRecoveryCodeWorksOnce(t *testing.T) {
	s, ck := newService(t)
	_, id := enrolledAdmin(t, s, ck)
	if err := s.Reverify(id, "bogus-bogus-bogus"); !is(err, ErrTOTPInvalid) {
		t.Fatalf("bogus recovery code: %v", err)
	}
	// Regenerating needs a reverified session; use a TOTP-less trick: read a code via a fresh issue.
	codes, err := s.issueRecoveryCodes(id.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := s.Login("root", goodPw, "3.3.3.3", "ua", "")
	if _, _, err := s.CompleteTOTP(r.Ticket, strings.ToUpper(codes[0]), false, "", "", ""); err != nil {
		t.Fatalf("recovery code (upper case, as users type it): %v", err)
	}
	r, _ = s.Login("root", goodPw, "3.3.3.3", "ua", "")
	if _, _, err := s.CompleteTOTP(r.Ticket, codes[0], false, "", "", ""); !is(err, ErrTOTPInvalid) {
		t.Fatalf("recovery code reused: %v", err)
	}
}

func TestTrustedDevice(t *testing.T) {
	s, ck := newService(t)
	secret, _ := enrolledAdmin(t, s, ck)
	r, _ := s.Login("root", goodPw, "4.4.4.4", "ua", "")
	_, dev, err := s.CompleteTOTP(r.Ticket, code(secret, ck), true, "my laptop", "4.4.4.4", "ua")
	if err != nil || dev == "" {
		t.Fatalf("trust: %q %v", dev, err)
	}
	// Within 30 days: password only.
	ck.add(29 * 24 * time.Hour)
	if r, err := s.Login("root", goodPw, "4.4.4.4", "ua", dev); err != nil || r.SessionToken == "" {
		t.Fatalf("trusted device login: %+v %v", r, err)
	}
	// Someone else's cookie value does not help.
	if r, _ := s.Login("root", goodPw, "4.4.4.4", "ua", "not-the-token"); r.SessionToken != "" {
		t.Fatal("unknown device token skipped the second factor")
	}
	// After 30 days from the TOTP verification: second factor again. Daily use does not extend it.
	ck.add(2 * 24 * time.Hour)
	if r, _ := s.Login("root", goodPw, "4.4.4.4", "ua", dev); r.SessionToken != "" {
		t.Fatal("expired device still trusted")
	}
	// Revocation takes effect at once.
	r, _ = s.Login("root", goodPw, "4.4.4.4", "ua", "")
	sess, dev2, _ := s.CompleteTOTP(r.Ticket, code(secret, ck), true, "phone", "4.4.4.4", "ua")
	id, _ := s.Authenticate(sess, "4.4.4.4")
	list, _ := s.ListDevices(id.User.ID)
	if len(list) != 1 || list[0].Name != "phone" {
		t.Fatalf("device list: %+v", list)
	}
	if err := s.RevokeDevice(id, list[0].ID); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.Login("root", goodPw, "4.4.4.4", "ua", dev2); r.SessionToken != "" {
		t.Fatal("revoked device still trusted")
	}
}

func TestPasswordChangeRevokesEverythingElse(t *testing.T) {
	s, ck := newService(t)
	secret, _ := enrolledAdmin(t, s, ck)
	login := func(trust bool) (string, string) {
		r, _ := s.Login("root", goodPw, "5.5.5.5", "ua", "")
		ck.add(31 * time.Second)
		sess, dev, err := s.CompleteTOTP(r.Ticket, code(secret, ck), trust, "d", "5.5.5.5", "ua")
		if err != nil {
			t.Fatal(err)
		}
		return sess, dev
	}
	here, _ := login(false)
	elsewhere, dev := login(true)
	id, _ := s.Authenticate(here, "5.5.5.5")
	if err := s.ChangePassword(id, goodPw, "another fine passphrase"); !is(err, ErrReverifyRequired) {
		t.Fatalf("password change without reverify: %v", err)
	}
	ck.add(31 * time.Second)
	if err := s.Reverify(id, code(secret, ck)); err != nil {
		t.Fatal(err)
	}
	if err := s.ChangePassword(id, "wrong old password", "another fine passphrase"); !is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong old password: %v", err)
	}
	if err := s.ChangePassword(id, goodPw, "another fine passphrase"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(elsewhere, ""); !is(err, ErrUnauthenticated) {
		t.Fatal("the other browser's session survived a password change")
	}
	if _, err := s.Authenticate(here, ""); err != nil {
		t.Fatal("the acting session should survive")
	}
	if r, _ := s.Login("root", "another fine passphrase", "5.5.5.5", "ua", dev); r.SessionToken != "" {
		t.Fatal("trusted device survived a password change")
	}
	if _, err := s.Login("root", goodPw, "5.5.5.5", "ua", ""); !is(err, ErrInvalidCredentials) {
		t.Fatal("old password still works")
	}
}

func TestSessionLifetimes(t *testing.T) {
	s, ck := newService(t)
	s.Setup(s.SetupToken(), "root", goodPw, "")
	r, _ := s.Login("root", goodPw, "", "", "")
	ck.add(6 * 24 * time.Hour)
	if _, err := s.Authenticate(r.SessionToken, ""); err != nil {
		t.Fatal("active within the idle window")
	}
	ck.add(6 * 24 * time.Hour) // 12 days since login, but only 6 since last seen
	if _, err := s.Authenticate(r.SessionToken, ""); err != nil {
		t.Fatal("idle window slides with use")
	}
	ck.add(8 * 24 * time.Hour)
	if _, err := s.Authenticate(r.SessionToken, ""); !is(err, ErrUnauthenticated) {
		t.Fatal("idle timeout not enforced")
	}
	r, _ = s.Login("root", goodPw, "", "", "")
	for i := 0; i < 6; i++ { // keep it busy past the absolute limit
		ck.add(6 * 24 * time.Hour)
		s.Authenticate(r.SessionToken, "")
	}
	if _, err := s.Authenticate(r.SessionToken, ""); !is(err, ErrUnauthenticated) {
		t.Fatal("absolute lifetime not enforced")
	}
}

func TestRateLimits(t *testing.T) {
	s, ck := newService(t)
	s.Setup(s.SetupToken(), "root", goodPw, "")
	for i := 0; i < 10; i++ {
		if _, err := s.Login("root", "wrong password!!", "9.9.9.9", "", ""); !is(err, ErrInvalidCredentials) {
			t.Fatal(err)
		}
	}
	// Blocked now: even the right password fails, with the very same error.
	_, err := s.Login("root", goodPw, "9.9.9.9", "", "")
	if err != ErrRateLimited || err.Error() != ErrInvalidCredentials.Error() {
		t.Fatalf("blocked login must look like any failure: %v", err)
	}
	// The username is blocked from other addresses too; unknown users look the same.
	if _, err := s.Login("root", goodPw, "8.8.8.8", "", ""); err == nil {
		t.Fatal("per-username block missing")
	}
	if _, err := s.Login("nobody", goodPw, "7.7.7.7", "", ""); err.Error() != ErrInvalidCredentials.Error() {
		t.Fatalf("unknown user: %v", err)
	}
	ck.add(16 * time.Minute)
	if r, err := s.Login("root", goodPw, "8.8.8.8", "", ""); err != nil || r.SessionToken == "" {
		t.Fatalf("block did not expire: %v", err)
	}
}

func TestAdminRules(t *testing.T) {
	s, ck := newService(t)
	secret, admin := enrolledAdmin(t, s, ck)
	if _, err := s.CreateUser(admin, "alice", "Alice", "user"); !is(err, ErrReverifyRequired) {
		t.Fatalf("admin write without reverify: %v", err)
	}
	if err := s.Reverify(admin, code(secret, ck)); err != nil {
		t.Fatal(err)
	}
	temp, err := s.CreateUser(admin, "alice", "Alice", "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser(admin, "ALICE", "", "user"); !is(err, ErrConflict) {
		t.Fatalf("usernames are case-insensitive: %v", err)
	}
	if err := s.SetStatus(admin, admin.User.ID, "disabled"); !is(err, ErrLastAdmin) {
		t.Fatalf("disabling the last admin: %v", err)
	}
	if err := s.SetRole(admin, admin.User.ID, "user"); !is(err, ErrLastAdmin) {
		t.Fatalf("demoting the last admin: %v", err)
	}
	// Alice: forced steps, and no admin powers.
	r, err := s.Login("alice", temp, "6.6.6.6", "", "")
	if err != nil || r.SessionToken == "" {
		t.Fatalf("alice first login: %v", err)
	}
	alice, _ := s.Authenticate(r.SessionToken, "6.6.6.6")
	if len(alice.Pending) != 2 {
		t.Fatalf("pending steps: %v", alice.Pending)
	}
	if _, _, err := s.TOTPBegin(alice); !is(err, ErrStepsPending) {
		t.Fatalf("totp before password change: %v", err)
	}
	if _, err := s.ListUsers(alice); !is(err, ErrForbidden) {
		t.Fatalf("user listing users: %v", err)
	}
	if err := s.ChangePassword(alice, temp, "alice has a long password"); err != nil {
		t.Fatal(err)
	}
	// Disabling ends her sessions at once.
	if err := s.SetStatus(admin, alice.User.ID, "disabled"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(r.SessionToken, ""); !is(err, ErrUnauthenticated) {
		t.Fatal("disabled user still authenticated")
	}
	if _, err := s.Login("alice", "alice has a long password", "6.6.6.6", "", ""); !is(err, ErrInvalidCredentials) {
		t.Fatal("disabled user can log in")
	}
	// Reverification expires.
	ck.add(11 * time.Minute)
	admin, _ = s.Authenticate(lastAdminToken, "")
	if admin == nil || admin.Reverified {
		t.Fatal("reverified state must expire")
	}
}

var lastAdminToken string

func TestAuditNeverContainsSecrets(t *testing.T) {
	s, ck := newService(t)
	secret, admin := enrolledAdmin(t, s, ck)
	s.Login("root", "a wrong password!", "1.2.3.4", "", "")
	s.Reverify(admin, code(secret, ck))
	temp, _ := s.CreateUser(admin, "bob", "", "user")
	entries, err := s.QueryAudit(admin, "", "", 1000)
	if err != nil || len(entries) < 5 {
		t.Fatalf("audit: %d %v", len(entries), err)
	}
	for _, e := range entries {
		line := e.Username + e.Event + e.Object + e.Result + e.Detail
		for _, secret := range []string{goodPw, "a wrong password!", temp, totpSecretText(secret)} {
			if strings.Contains(line, secret) {
				t.Fatalf("audit entry leaks a secret: %+v", e)
			}
		}
	}
	var events []string
	for _, e := range entries {
		events = append(events, e.Event+":"+e.Result)
	}
	for _, want := range []string{"setup:ok", "login:ok", "login:failed", "totp_enrolled:ok", "user_created:ok"} {
		if !strings.Contains(strings.Join(events, " "), want) {
			t.Errorf("missing audit event %s in %v", want, events)
		}
	}
}

func TestCryptoPrimitives(t *testing.T) {
	// RFC 6238 appendix B, SHA-1, secret "12345678901234567890": 59 -> 94287082 (8 digits) -> last 6.
	if got := totpCode([]byte("12345678901234567890"), 59/30); got != "287082" {
		t.Fatalf("totp vector: %s", got)
	}
	h := hashPassword("pw", HashParams{MemoryKiB: 64, Time: 1, Threads: 1})
	if !verifyPassword("pw", h) || verifyPassword("pW", h) || verifyPassword("pw", "argon2id$999999999$1$1$AA$AA") || verifyPassword("pw", "garbage") {
		t.Fatal("password hashing")
	}
	sl, _ := NewSealer(make([]byte, 32))
	sealed := sl.Seal([]byte("secret"), "ctx-a")
	if p, err := sl.Open(sealed, "ctx-a"); err != nil || string(p) != "secret" {
		t.Fatal("seal round trip")
	}
	if _, err := sl.Open(sealed, "ctx-b"); err == nil {
		t.Fatal("a sealed value must not open under another context")
	}
	if _, err := NewSealer(make([]byte, 16)); err == nil {
		t.Fatal("short master key accepted")
	}
}
