package auth

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func countCompares(t *testing.T) *int32 {
	t.Helper()
	var n int32
	onCompare = func() { atomic.AddInt32(&n, 1) }
	t.Cleanup(func() { onCompare = nil })
	return &n
}

// Wrong second-factor codes are counted per user, across tickets; the fifth
// locks the second factor, and even the right code is refused until the lock
// expires. Consecutive locks last longer (安全复核 C1).
func TestSecondFactorLockout(t *testing.T) {
	s, ck := newService(t)
	secret, _ := enrolledAdmin(t, s, ck)
	for i := 1; i <= mfaFailures; i++ {
		r, err := s.Login("root", goodPw, "1.1.1.1", "", "")
		if err != nil || r.Ticket == "" {
			t.Fatalf("login %d: %v", i, err)
		}
		_, _, err = s.CompleteTOTP(r.Ticket, "000000", false, "", "1.1.1.1", "")
		if i < mfaFailures && !is(err, ErrTOTPInvalid) {
			t.Fatalf("wrong code %d: %v", i, err)
		}
		if i == mfaFailures && !is(err, ErrSecondFactorLocked) {
			t.Fatalf("the fifth wrong code must lock: %v", err)
		}
	}
	r, _ := s.Login("root", goodPw, "1.1.1.1", "", "")
	if _, _, err := s.CompleteTOTP(r.Ticket, code(secret, ck), false, "", "1.1.1.1", ""); !is(err, ErrSecondFactorLocked) {
		t.Fatalf("locked: the right code must be refused, got %v", err)
	}
	// A second lock right after the first (no success in between) lasts 30 minutes.
	ck.add(16 * time.Minute) // first lock: 15 minutes
	for i := 0; i < mfaFailures; i++ {
		r, _ := s.Login("root", goodPw, "1.1.1.1", "", "")
		s.CompleteTOTP(r.Ticket, "000000", false, "", "1.1.1.1", "")
	}
	ck.add(20 * time.Minute)
	r, _ = s.Login("root", goodPw, "1.1.1.1", "", "")
	if _, _, err := s.CompleteTOTP(r.Ticket, code(secret, ck), false, "", "1.1.1.1", ""); !is(err, ErrSecondFactorLocked) {
		t.Fatalf("the second lock must last longer than 15 minutes: %v", err)
	}
	ck.add(11 * time.Minute)
	r, _ = s.Login("root", goodPw, "1.1.1.1", "", "")
	if sess, _, err := s.CompleteTOTP(r.Ticket, code(secret, ck), false, "", "1.1.1.1", ""); err != nil || sess == "" {
		t.Fatalf("after the second lock: %v", err)
	}
}

// Slow guessing under the per-window count still runs into the long-term
// counter (复核第二轮).
func TestSecondFactorLongTermCounter(t *testing.T) {
	s, ck := newService(t)
	secret, _ := enrolledAdmin(t, s, ck)
	locked := false
	for round := 0; round < 8 && !locked; round++ {
		for i := 0; i < mfaFailures-1; i++ { // stay under the window count
			r, _ := s.Login("root", goodPw, "1.1.1.1", "", "")
			if _, _, err := s.CompleteTOTP(r.Ticket, "000000", false, "", "1.1.1.1", ""); is(err, ErrSecondFactorLocked) {
				locked = true
				break
			}
		}
		ck.add(16 * time.Minute)
	}
	if !locked {
		t.Fatal("32 wrong codes spread over windows never locked the second factor")
	}
	// The long-term lock is the longest one (4 h), never permanent: after it
	// the right code works again (复核第三轮).
	ck.add(4*time.Hour + time.Minute)
	r, _ := s.Login("root", goodPw, "1.1.1.1", "", "")
	if sess, _, err := s.CompleteTOTP(r.Ticket, code(secret, ck), false, "", "1.1.1.1", ""); err != nil || sess == "" {
		t.Fatalf("after the longest lock the right code must work: %v", err)
	}
}

// A reset by the administrator wipes every counter: the user re-enrols and
// logs in with the new authenticator at once (复核第三轮 新问题 1).
func TestResetTOTPClearsLocks(t *testing.T) {
	s, ck := newService(t)
	enrolledAdmin(t, s, ck)
	for round := 0; round < 6; round++ { // run the long-term counter past its limit
		for i := 0; i < mfaFailures-1; i++ {
			r, _ := s.Login("root", goodPw, "1.1.1.1", "", "")
			s.CompleteTOTP(r.Ticket, "000000", false, "", "1.1.1.1", "")
		}
		ck.add(16 * time.Minute)
	}
	temp, err := s.ResetTOTPLocal("root")
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Login("root", temp, "1.1.1.1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	id, _ := s.Authenticate(r.SessionToken, "1.1.1.1")
	if err := s.ChangePassword(id, temp, goodPw+"2"); err != nil {
		t.Fatal(err)
	}
	id, _ = s.Authenticate(r.SessionToken, "1.1.1.1")
	if _, _, err := s.TOTPBegin(id); err != nil {
		t.Fatal(err)
	}
	secret := pendingSecret(t, s, id)
	if _, err := s.TOTPConfirm(id, code(secret, ck)); err != nil {
		t.Fatal(err)
	}
	ck.add(31 * time.Second)
	r2, err := s.Login("root", goodPw+"2", "1.1.1.1", "", "")
	if err != nil || r2.Ticket == "" {
		t.Fatalf("login after reset: %v", err)
	}
	if sess, _, err := s.CompleteTOTP(r2.Ticket, code(secret, ck), false, "", "1.1.1.1", ""); err != nil || sess == "" {
		t.Fatalf("the new authenticator must work after the reset: %v", err)
	}
}

// Concurrent guesses across two login sessions (reverify) share the user's
// five: the numbers are reserved first, and anything beyond is not compared.
func TestSecondFactorConcurrencyAcrossSessions(t *testing.T) {
	s, ck := newService(t)
	secret, _ := enrolledAdmin(t, s, ck)
	var ids []*Identity
	for i := 0; i < 2; i++ {
		ck.add(31 * time.Second)
		r, _ := s.Login("root", goodPw, "1.1.1.1", "", "")
		sess, _, err := s.CompleteTOTP(r.Ticket, code(secret, ck), false, "", "1.1.1.1", "")
		if err != nil {
			t.Fatal(err)
		}
		id, _ := s.Authenticate(sess, "1.1.1.1")
		ids = append(ids, id)
	}
	compares := countCompares(t)
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); s.Reverify(ids[i%2], "000000") }(i)
	}
	wg.Wait()
	if n := atomic.LoadInt32(compares); n > int32(mfaFailures) {
		t.Fatalf("%d codes were compared across sessions, at most %d allowed", n, mfaFailures)
	}
}

// A burst of concurrent guesses on one ticket shares the five attempts.
func TestTicketGuessesAreReservedBeforeChecking(t *testing.T) {
	s, ck := newService(t)
	enrolledAdmin(t, s, ck)
	r, err := s.Login("root", goodPw, "1.1.1.1", "", "")
	if err != nil || r.Ticket == "" {
		t.Fatal(err)
	}
	compares := countCompares(t)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); s.CompleteTOTP(r.Ticket, "000000", false, "", "1.1.1.1", "") }()
	}
	wg.Wait()
	if n := atomic.LoadInt32(compares); n > 5 {
		t.Fatalf("%d guesses were compared on one ticket, at most 5 allowed", n)
	}
}

// Password attempts are counted before the hash runs: the eleventh in a row
// is refused without hashing (安全复核 H1).
func TestPasswordGuessesAreReservedBeforeChecking(t *testing.T) {
	s, _ := newService(t)
	if err := s.Setup(s.SetupToken(), "root", goodPw, ""); err != nil {
		t.Fatal(err)
	}
	verified := 0
	for i := 0; i < limitFailures+3; i++ {
		_, err := s.Login("root", "wrong password!!", "10.0.0.1", "", "")
		if err == ErrInvalidCredentials {
			verified++
		} else if err != ErrRateLimited {
			t.Fatal(err)
		}
	}
	if verified != limitFailures {
		t.Fatalf("%d password checks ran, want exactly %d", verified, limitFailures)
	}
	// And concurrently, from many addresses, the count still holds.
	s2, _ := newService(t)
	s2.Setup(s2.SetupToken(), "root", goodPw, "")
	var checked int32
	var wg sync.WaitGroup
	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := s2.Login("root", "wrong password!!", "10.0.0."+string(rune('1'+i%9)), "", ""); err == ErrInvalidCredentials {
				atomic.AddInt32(&checked, 1)
			}
		}(i)
	}
	wg.Wait()
	if checked > limitFailures {
		t.Fatalf("%d concurrent password checks ran for one user, at most %d allowed", checked, limitFailures)
	}
}

// Oversized inputs never reach the hash.
func TestLoginInputBounds(t *testing.T) {
	s, _ := newService(t)
	s.Setup(s.SetupToken(), "root", goodPw, "")
	big := make([]byte, 60000)
	for i := range big {
		big[i] = 'a'
	}
	t0 := time.Now()
	if _, err := s.Login("root", string(big), "1.1.1.1", "", ""); !is(err, ErrInvalidCredentials) {
		t.Fatal(err)
	}
	if _, err := s.Login(string(big), goodPw, "1.1.1.1", "", ""); !is(err, ErrInvalidCredentials) {
		t.Fatal(err)
	}
	if time.Since(t0) > time.Second {
		t.Fatalf("oversized input took %v: it was hashed", time.Since(t0))
	}
}

// A blocked username still admits its own trusted device (安全复核 M2), but
// the device has a budget of its own and is forfeited when a wrong password
// is tried too often with it; another user's device does not help.
func TestTrustedDeviceUnderUsernameBlock(t *testing.T) {
	s, ck := newService(t)
	secret, _ := enrolledAdmin(t, s, ck)
	var devs []string
	for i := 0; i < 2; i++ { // two trusted devices: one will be abused, one stays honest
		ck.add(31 * time.Second)
		r, _ := s.Login("root", goodPw, "1.1.1.1", "", "")
		_, dev, err := s.CompleteTOTP(r.Ticket, code(secret, ck), true, "laptop", "1.1.1.1", "")
		if err != nil || dev == "" {
			t.Fatal(err)
		}
		devs = append(devs, dev)
	}
	for i := 0; i < limitFailures+1; i++ { // a stranger hammers the username
		s.Login("root", "wrong password!!", "9.9.9."+string(rune('1'+i%9)), "", "")
	}
	if _, err := s.Login("root", goodPw, "8.8.8.8", "", ""); err != ErrRateLimited {
		t.Fatalf("username should be blocked: %v", err)
	}
	if _, err := s.Login("root", goodPw, "8.8.8.8", "", "not-a-device"); err != ErrRateLimited {
		t.Fatalf("a bogus device must not help: %v", err)
	}
	// With a stolen device cookie but wrong passwords from many addresses: the
	// first wrong password forfeits the device, however slowly one goes.
	checked := 0
	for i := 0; i < 20; i++ {
		if _, err := s.Login("root", "wrong password!!", "7.7."+string(rune('1'+i%9))+".1", "", devs[0]); err == ErrInvalidCredentials {
			checked++
		}
	}
	if checked != 1 {
		t.Fatalf("%d password checks with a device cookie on a blocked username, want 1", checked)
	}
	if _, err := s.Login("root", goodPw, "8.8.8.8", "", devs[0]); err != ErrRateLimited {
		t.Fatalf("the abused device must be forfeited: %v", err)
	}
	// The owner's other device, with the right password, still gets in.
	if res, err := s.Login("root", goodPw, "8.8.8.8", "", devs[1]); err != nil || res.SessionToken == "" {
		t.Fatalf("the honest trusted device must still get in: %v", err)
	}
}

// Confirming a new authenticator is limited per login session, and replacing
// an existing one needs a fresh second factor.
func TestTOTPConfirmLimited(t *testing.T) {
	s, ck := newService(t)
	_, id := enrolledAdmin(t, s, ck)
	if _, _, err := s.TOTPBegin(id); !is(err, ErrReverifyRequired) {
		t.Fatalf("replacing the factor without reverify: %v", err)
	}
	id.Reverified = true
	if _, _, err := s.TOTPBegin(id); err != nil {
		t.Fatal(err)
	}
	id.Reverified = false
	if _, err := s.TOTPConfirm(id, "000000"); !is(err, ErrReverifyRequired) {
		t.Fatalf("confirm without reverify: %v", err)
	}
	id.Reverified = true
	for i := 0; i < 5; i++ {
		if _, err := s.TOTPConfirm(id, "000000"); !is(err, ErrTOTPInvalid) {
			t.Fatalf("try %d: %v", i, err)
		}
	}
	// The sixth is refused without a compare, and the pending secret is gone.
	if _, err := s.TOTPConfirm(id, "000000"); !is(err, ErrTOTPInvalid) {
		t.Fatal(err)
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM settings WHERE key LIKE 'totp_pending:%'`).Scan(&n)
	if n != 0 {
		t.Fatal("the pending secret should have been discarded")
	}
}

// Resetting the second factor also replaces the password (安全复核 M4).
func TestResetTOTPReplacesPassword(t *testing.T) {
	s, ck := newService(t)
	enrolledAdmin(t, s, ck)
	temp, err := s.ResetTOTPLocal("root")
	if err != nil || temp == "" {
		t.Fatal(err)
	}
	if _, err := s.Login("root", goodPw, "1.1.1.1", "", ""); !is(err, ErrInvalidCredentials) {
		t.Fatalf("old password must be gone: %v", err)
	}
	r, err := s.Login("root", temp, "1.1.1.1", "", "")
	if err != nil || r.SessionToken == "" {
		t.Fatalf("temporary password: %v", err)
	}
	id, _ := s.Authenticate(r.SessionToken, "1.1.1.1")
	if len(id.Pending) != 2 {
		t.Fatalf("both first-login steps must be pending again: %v", id.Pending)
	}
}

// Unknown usernames are logged as a short hash only.
func TestAuditHidesUnknownUsernames(t *testing.T) {
	s, _ := newService(t)
	s.Setup(s.SetupToken(), "root", goodPw, "")
	s.Login("my secret password 123", "x", "1.1.1.1", "", "")
	rows, _ := s.QueryAudit(&Identity{User: User{Role: "admin"}}, "login", "", 5)
	for _, e := range rows {
		if e.Username == "my secret password 123" {
			t.Fatal("an unknown username was written to the audit log verbatim")
		}
	}
}
