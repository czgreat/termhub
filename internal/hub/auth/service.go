package auth

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"termhub/internal/hub/store"
)

// Config carries the tunables of docs/M6 and M8 第 5 节.
type Config struct {
	Hash            HashParams
	HashConcurrency int           // default 4
	SessionIdle     time.Duration // default 7 days
	SessionMax      time.Duration // default 30 days
	TrustDevice     time.Duration // default 30 days
	ReverifyFor     time.Duration // default 10 minutes
	Issuer          string        // shown in authenticator apps; default "termhub"
	Now             func() time.Time
}

func (c *Config) defaults() {
	if c.Hash == (HashParams{}) {
		c.Hash = DefaultHashParams
	}
	if c.HashConcurrency == 0 {
		c.HashConcurrency = 4
	}
	if c.SessionIdle == 0 {
		c.SessionIdle = 7 * 24 * time.Hour
	}
	if c.SessionMax == 0 {
		c.SessionMax = 30 * 24 * time.Hour
	}
	if c.TrustDevice == 0 {
		c.TrustDevice = 30 * 24 * time.Hour
	}
	if c.ReverifyFor == 0 {
		c.ReverifyFor = 10 * time.Minute
	}
	if c.Issuer == "" {
		c.Issuer = "termhub"
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// Error is an auth failure with a stable code and an HTTP status.
type Error struct {
	Code   string
	Msg    string
	Status int
}

func (e *Error) Error() string { return e.Code + ": " + e.Msg }

var (
	ErrInvalidCredentials = &Error{"invalid_credentials", "用户名或密码错误", 401}
	ErrRateLimited        = &Error{"invalid_credentials", "用户名或密码错误", 401} // indistinguishable on purpose
	ErrTicketInvalid      = &Error{"ticket_invalid", "验证已过期，请重新登录", 401}
	ErrTOTPInvalid        = &Error{"totp_invalid", "验证码不正确", 401}
	ErrUnauthenticated    = &Error{"unauthorized", "未登录或登录已失效", 401}
	ErrForbidden          = &Error{"forbidden", "无权执行此操作", 403}
	ErrReverifyRequired   = &Error{"reverify_required", "此操作需要再次验证", 403}
	ErrStepsPending       = &Error{"steps_pending", "请先完成首次登录的必要步骤", 403}
	ErrSetupClosed        = &Error{"not_found", "not found", 404}
	ErrWeakPassword       = &Error{"weak_password", "密码不符合要求：至少 11 位，不能是常见弱密码，不能与用户名相同", 400}
	ErrLastAdmin          = &Error{"last_admin", "不能禁用或降级最后一个管理员", 409}
	ErrConflict           = &Error{"conflict", "用户名已存在", 409}
	ErrNotFound           = &Error{"not_found", "对象不存在", 404}
	ErrBadRequest         = &Error{"bad_request", "请求无效", 400}
	ErrSecondFactorLocked = &Error{"totp_locked", "动态码连续错误次数过多，第二因子已暂时锁定，请稍后再试", 429}
	ErrBusy               = &Error{"busy", "服务器忙，请稍后再试", 503}
)

// User is an account as seen by the rest of the Hub.
type User struct {
	ID                 int64
	Username           string
	DisplayName        string
	Role               string
	Status             string
	MustChangePassword bool
	TOTPConfirmed      bool
	CreatedAt          int64
	LastLoginAt        int64
}

func (u User) IsAdmin() bool { return u.Role == "admin" }

// Identity is an authenticated request's context.
type Identity struct {
	User            User
	CSRF            string
	Reverified      bool
	ReverifiedUntil int64    // unix seconds; 0 when not reverified
	Pending         []string // forced first-login steps still open
	sessHash        []byte
	IP              string
}

type ticket struct {
	userID   int64
	expires  time.Time
	failures int
}

// Service is the authentication core. HTTP lives in http.go.
type Service struct {
	db        *store.DB
	seal      *Sealer
	cfg       Config
	sem       chan struct{}
	dummy     string        // hash verified when the username is unknown: equal timing
	dummyCost time.Duration // how long one hash takes here: the delay of a refused attempt
	hideKey   []byte        // keyed hash for names that must not appear in the audit log
	limit     *limiter

	mu         sync.Mutex
	tickets    map[string]*ticket
	inflight   map[string]int // password checks in progress, per user and per address
	auditSeen  map[string]time.Time
	setupToken string
}

const (
	loginIPFailures   = 30 // per address (a household shares one), 15 minutes
	loginInflightUser = 2  // concurrent password checks for one username
	loginInflightIP   = 4
	mfaFailures       = 5 // wrong second-factor codes per user per window, then a lock
	mfaWindow         = 15 * time.Minute
	mfaLock           = 15 * time.Minute // doubles with each consecutive lock, up to 16×
)

// New creates the service. When the database has no users yet, a one-time
// setup token is generated; the caller prints it to the container log.
func New(db *store.DB, masterKey []byte, cfg Config) (*Service, error) {
	cfg.defaults()
	seal, err := NewSealer(masterKey)
	if err != nil {
		return nil, err
	}
	s := &Service{db: db, seal: seal, cfg: cfg, sem: make(chan struct{}, cfg.HashConcurrency),
		tickets: map[string]*ticket{}, inflight: map[string]int{}, limit: newLimiter(cfg.Now)}
	hk := sha256.Sum256(append([]byte("termhub audit hide:"), masterKey...))
	s.hideKey = hk[:]
	t0 := time.Now()
	s.dummy = hashPassword(newToken(), cfg.Hash)
	s.dummyCost = time.Since(t0)
	if s.dummyCost < 50*time.Millisecond {
		s.dummyCost = 50 * time.Millisecond
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return nil, err
	}
	if n == 0 {
		s.setupToken = newToken()
	}
	return s, nil
}

// SetupToken is non-empty only while no user exists. The database decides:
// a first user made with the command line closes setup too (复核第四轮 8).
func (s *Service) SetupToken() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.setupToken != "" && s.usersExist() {
		s.setupToken = ""
	}
	return s.setupToken
}

func (s *Service) usersExist() bool {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return true // fail closed
	}
	return n > 0
}

func (s *Service) now() int64 { return s.cfg.Now().Unix() }

// hash and verify run under a semaphore so that a flood of logins queues up
// instead of exhausting the NAS's memory (docs/M6 第 2 节).
func (s *Service) hash(pw string) string {
	s.sem <- struct{}{}
	defer func() { <-s.sem }()
	return hashPassword(pw, s.cfg.Hash)
}

func (s *Service) verify(pw, encoded string) bool {
	s.sem <- struct{}{}
	defer func() { <-s.sem }()
	return verifyPassword(pw, encoded)
}

// verifyOrBusy is verify for the login path: a queue that does not move
// within a few seconds is a flood, and callers get "busy" instead of waiting
// forever (安全复核 M1).
func (s *Service) verifyOrBusy(pw, encoded string) (ok bool, busy bool) {
	select {
	case s.sem <- struct{}{}:
	case <-time.After(5 * time.Second):
		return false, true
	}
	defer func() { <-s.sem }()
	return verifyPassword(pw, encoded), false
}

func (s *Service) hideName(username string) string {
	m := hmac.New(sha256.New, s.hideKey)
	m.Write([]byte(strings.ToLower(username)))
	return fmt.Sprintf("?%x", m.Sum(nil)[:4])
}

// auditOnce writes one audit line per address, event and result every 15
// minutes for the noise one address can make without logging in (bad input,
// refused attempts): the audit log must not be the thing an attacker fills,
// nor the database write that slows everyone down (复核第四轮 1).
func (s *Service) auditOnce(hidden, ip, event, result string) {
	k := limitIP(ip) + "|" + event + "|" + result
	now := s.cfg.Now()
	s.mu.Lock()
	if s.auditSeen == nil {
		s.auditSeen = map[string]time.Time{}
	}
	if len(s.auditSeen) > 20000 {
		for k2, t := range s.auditSeen {
			if now.Sub(t) > limitWindow {
				delete(s.auditSeen, k2)
			}
		}
	}
	if t, ok := s.auditSeen[k]; ok && now.Sub(t) < limitWindow {
		s.mu.Unlock()
		return
	}
	s.auditSeen[k] = now
	s.mu.Unlock()
	s.audit(nil, hidden, ip, event, "", result, "")
}

// AuditNoisy is auditOnce for the other front doors (a wrong node token).
func (s *Service) AuditNoisy(ip, event, result string) { s.auditOnce("", ip, event, result) }

// rateLimited is the answer to a refused login attempt: the delay of a real
// check, one audit line per window, and the error.
func (s *Service) rateLimited(hidden, ip string) (*LoginResult, error) {
	time.Sleep(s.dummyCost) // no hashing for a blocked attempt, but the same delay
	s.auditOnce(hidden, ip, "login", "rate_limited")
	return nil, ErrRateLimited
}

// limitIP is the rate-limit key for an address: IPv6 is counted per /64,
// because one subscriber holds a whole prefix (安全复核 M3).
func limitIP(ip string) string {
	p := net.ParseIP(ip)
	if p == nil {
		return ip
	}
	if v4 := p.To4(); v4 != nil { // one form for "1.2.3.4" and "::ffff:1.2.3.4"
		return v4.String()
	}
	return p.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// acquire counts a login attempt in progress under key; false when too many run at once.
func (s *Service) acquire(key string, max int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight[key] >= max {
		return false
	}
	s.inflight[key]++
	return true
}

func (s *Service) release(key string) {
	s.mu.Lock()
	if s.inflight[key] <= 1 {
		delete(s.inflight, key)
	} else {
		s.inflight[key]--
	}
	s.mu.Unlock()
}

// forfeitDevice removes a trusted device that was used to guess.
func (s *Service) forfeitDevice(deviceToken string) {
	s.db.Exec(`DELETE FROM trusted_devices WHERE token_hash=?`, tokenHash(deviceToken))
	s.limit.reset(fmt.Sprintf("dev:%x", tokenHash(deviceToken)))
}

// deviceOf reports whether deviceToken is a trusted device of the account
// named username, without touching it. A blocked username may still log in
// from such a device, so that a stranger cannot lock the owner out (安全复核 M2).
func (s *Service) deviceOf(username, deviceToken string) bool {
	if deviceToken == "" {
		return false
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM trusted_devices d JOIN users u ON u.id=d.user_id WHERE u.username=? AND d.token_hash=? AND d.expires_at>?`,
		username, tokenHash(deviceToken), s.now()).Scan(&n)
	return n == 1
}

var weakPasswords = map[string]bool{"password1234": true, "12345678901": true, "11111111111": true, "password123": true, "qwertyuiop1": true, "123456789012": true, "qwertyuiop12": true,
	"111111111111": true, "000000000000": true, "passwordpassword": true, "admin1234567": true,
	"iloveyou1234": true, "1q2w3e4r5t6y": true, "abcdefghijkl": true, "123123123123": true}

func checkPassword(username, pw string) error {
	if utf8.RuneCountInString(pw) < 11 || len(pw) > 256 || strings.EqualFold(pw, username) || weakPasswords[strings.ToLower(pw)] {
		return ErrWeakPassword
	}
	return nil
}

func checkUsername(u string) error {
	if n := utf8.RuneCountInString(u); n < 3 || n > 32 || strings.ContainsAny(u, " \t\r\n/\\:\"'<>") {
		return &Error{"bad_request", "用户名需为 3 到 32 位，不含空白和特殊符号", 400}
	}
	return nil
}

// Setup creates the first administrator and closes setup for good.
func (s *Service) Setup(token, username, password, ip string) error {
	s.mu.Lock()
	ok := s.setupToken != "" && hmacEqual(token, s.setupToken)
	s.mu.Unlock()
	if !ok {
		return ErrSetupClosed
	}
	if err := checkUsername(username); err != nil {
		return err
	}
	if err := checkPassword(username, password); err != nil {
		return err
	}
	h := s.hash(password)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.setupToken == "" || s.usersExist() {
		s.setupToken = ""
		return ErrSetupClosed
	}
	if _, err := s.db.Exec(`INSERT INTO users(username, role, password_hash, created_at) VALUES (?,?,?,?)`,
		username, "admin", h, s.now()); err != nil {
		return err
	}
	s.setupToken = ""
	s.audit(nil, username, ip, "setup", username, "ok", "")
	return nil
}

func hmacEqual(a, b string) bool { return bytes.Equal(tokenHash(a), tokenHash(b)) }

const userCols = `id, username, display_name, role, status, must_change_password, totp_confirmed_at IS NOT NULL, created_at, COALESCE(last_login_at,0)`

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Username, &u.DisplayName, &u.Role, &u.Status, &u.MustChangePassword, &u.TOTPConfirmed, &u.CreatedAt, &u.LastLoginAt)
	return u, err
}

func (s *Service) userByID(id int64) (User, error) {
	u, err := scanUser(s.db.QueryRow(`SELECT `+userCols+` FROM users WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	return u, err
}

// LoginResult is either a session (done) or a ticket (second factor needed).
type LoginResult struct {
	SessionToken string
	Ticket       string
}

// Login is step one. deviceToken is the trusted-device cookie, if any.
func (s *Service) Login(username, password, ip, ua, deviceToken string) (*LoginResult, error) {
	// Bounded input before anything costs: no argon2 over a 64 KiB "password".
	// A username that does not exist (or is being rate limited before we look)
	// is logged only as a short keyed hash: a password typed into the wrong box
	// must not end up in the audit log, nor be recoverable from it (安全复核 L6).
	hidden := s.hideName(username)
	ipKey := "ip:" + limitIP(ip)
	// The address's concurrency and budget come first, before any input is
	// looked at: a stream of junk from one address must not reach the database
	// (复核第四轮 1). The attempt is counted before the password is checked, so
	// that a burst of concurrent requests cannot all pass the check (安全复核 H1).
	if !s.acquire(ipKey, loginInflightIP) {
		return s.rateLimited(hidden, ip)
	}
	defer s.release(ipKey)
	if !s.limit.try(ipKey, loginIPFailures) {
		return s.rateLimited(hidden, ip)
	}
	if username == "" || len(username) > 64 || len(password) > 256 {
		s.auditOnce(hidden, ip, "login", "failed")
		return nil, ErrInvalidCredentials
	}
	userKey := "user:" + strings.ToLower(username)
	var id int64
	var hash, status string
	err := s.db.QueryRow(`SELECT id, password_hash, status FROM users WHERE username=?`, username).Scan(&id, &hash, &status)
	if errors.Is(err, sql.ErrNoRows) {
		hash = s.dummy
	} else if err != nil {
		return nil, err
	}
	// A trusted-device cookie of this very account: the first wrong password
	// tried with it forfeits the device, however slowly the guesses come, and
	// whether or not the username is blocked (复核第二、四轮).
	viaDevice := s.deviceOf(username, deviceToken)
	if id != 0 {
		// The per-username budget exists only for names that exist: an invented
		// name must not cost a table entry (复核第四轮 2). An unknown name still
		// pays the dummy hash below, so the timing is the same.
		if s.limit.blocked(userKey) {
			// A blocked username still admits its own trusted devices (M2), a
			// handful of concurrent tries at most.
			if !viaDevice {
				return s.rateLimited(hidden, ip)
			}
			if !s.limit.try(fmt.Sprintf("dev:%x", tokenHash(deviceToken)), 5) {
				s.forfeitDevice(deviceToken)
				return s.rateLimited(hidden, ip)
			}
		} else if !s.limit.try(userKey, limitFailures) {
			return s.rateLimited(hidden, ip)
		}
		if !s.acquire(userKey, loginInflightUser) {
			return s.rateLimited(hidden, ip)
		}
		defer s.release(userKey)
	}
	ok, busy := s.verifyOrBusy(password, hash)
	if busy {
		return nil, ErrBusy
	}
	if !ok || id == 0 || status != "active" {
		name := username
		if id == 0 {
			name = hidden
		}
		if viaDevice {
			s.forfeitDevice(deviceToken)
		}
		s.audit(nil, name, ip, "login", "", "failed", "")
		return nil, ErrInvalidCredentials
	}
	s.limit.undo(ipKey) // a successful login is not a failure
	u, err := s.userByID(id)
	if err != nil {
		return nil, err
	}
	if u.TOTPConfirmed && !s.deviceTrusted(u.ID, deviceToken) {
		t := newToken()
		s.mu.Lock()
		for k, v := range s.tickets { // one live ticket per user; expired ones go too
			if s.cfg.Now().After(v.expires) || v.userID == u.ID {
				delete(s.tickets, k)
			}
		}
		s.tickets[t] = &ticket{userID: u.ID, expires: s.cfg.Now().Add(5 * time.Minute)}
		s.mu.Unlock()
		return &LoginResult{Ticket: t}, nil
	}
	// No second factor yet (first login: enrolment is forced next) or a trusted device.
	tok, err := s.newSession(u, ip, ua, deviceToken)
	if err != nil {
		return nil, err
	}
	s.limit.reset(userKey)
	return &LoginResult{SessionToken: tok}, nil
}

func (s *Service) deviceTrusted(userID int64, deviceToken string) bool {
	if deviceToken == "" {
		return false
	}
	res, err := s.db.Exec(`UPDATE trusted_devices SET last_used_at=? WHERE token_hash=? AND user_id=? AND expires_at>?`,
		s.now(), tokenHash(deviceToken), userID, s.now())
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n == 1
}

func (s *Service) newSession(u User, ip, ua, deviceToken string) (string, error) {
	tok := newToken()
	var dev any
	if deviceToken != "" {
		dev = tokenHash(deviceToken)
	}
	if len(ua) > 300 {
		ua = ua[:300]
	}
	_, err := s.db.Exec(`INSERT INTO login_sessions(token_hash,user_id,created_at,last_seen_at,ip,user_agent,csrf_token,device_hash) VALUES (?,?,?,?,?,?,?,?)`,
		tokenHash(tok), u.ID, s.now(), s.now(), ip, ua, newToken(), dev)
	if err != nil {
		return "", err
	}
	s.db.Exec(`UPDATE users SET last_login_at=? WHERE id=?`, s.now(), u.ID)
	s.audit(&u, u.Username, ip, "login", "", "ok", "")
	return tok, nil
}

// CompleteTOTP is step two: a TOTP code or a recovery code. When trust is
// set, a trusted-device token is returned for the browser to keep.
func (s *Service) CompleteTOTP(ticketToken, code string, trust bool, deviceName, ip, ua string) (session, device string, err error) {
	s.mu.Lock()
	t := s.tickets[ticketToken]
	if t == nil || s.cfg.Now().After(t.expires) || t.failures >= 5 {
		delete(s.tickets, ticketToken)
		s.mu.Unlock()
		return "", "", ErrTicketInvalid
	}
	t.failures++ // reserved before the check: concurrent guesses share the five (安全复核 C1)
	userID := t.userID
	s.mu.Unlock()

	u, err := s.userByID(userID)
	if err != nil || u.Status != "active" {
		return "", "", ErrTicketInvalid
	}
	if ok, locked := s.checkSecondFactor(u, code, ip); !ok {
		s.audit(&u, u.Username, ip, "totp", "", "failed", "")
		if locked {
			return "", "", ErrSecondFactorLocked
		}
		return "", "", ErrTOTPInvalid
	}
	s.mu.Lock()
	delete(s.tickets, ticketToken)
	s.mu.Unlock()

	if trust {
		device = newToken()
		if len(deviceName) > 100 {
			deviceName = deviceName[:100]
		}
		if _, err := s.db.Exec(`INSERT INTO trusted_devices(token_hash,user_id,name,created_at,last_used_at,expires_at) VALUES (?,?,?,?,?,?)`,
			tokenHash(device), u.ID, deviceName, s.now(), s.now(), s.cfg.Now().Add(s.cfg.TrustDevice).Unix()); err != nil {
			return "", "", err
		}
		// At most 10 per user: drop the least recently used beyond that.
		s.db.Exec(`DELETE FROM trusted_devices WHERE user_id=? AND token_hash NOT IN
			(SELECT token_hash FROM trusted_devices WHERE user_id=? ORDER BY last_used_at DESC LIMIT 10)`, u.ID, u.ID)
		s.audit(&u, u.Username, ip, "device_trusted", deviceName, "ok", "")
	}
	session, err = s.newSession(u, ip, ua, device)
	s.limit.reset("user:" + strings.ToLower(u.Username))
	return session, device, err
}

// mfaReserve counts one second-factor attempt for the user before the code
// is compared (安全复核 C1): a single UPDATE, so concurrent attempts are
// serialized by the database. It returns the attempt's number inside the
// current window, or locked when the user's second factor is locked.
func (s *Service) mfaReserve(userID int64) (n, total int, locked bool) {
	now := s.now()
	win := int64(mfaWindow.Seconds())
	// SET expressions see the row as it was: the long-term counter forgets
	// itself after a day without failures (复核第三轮: it must never become a
	// permanent lock).
	err := s.db.QueryRow(`UPDATE users SET
		mfa_failures = CASE WHEN ? - mfa_window_start > ? THEN 1 ELSE mfa_failures + 1 END,
		mfa_total = CASE WHEN ? - mfa_window_start > ? THEN 1 ELSE mfa_total + 1 END,
		mfa_window_start = CASE WHEN ? - mfa_window_start > ? THEN ? ELSE mfa_window_start END
		WHERE id=? AND mfa_locked_until <= ? RETURNING mfa_failures, mfa_total`, now, win, now, int64(mfaTotalDecay.Seconds()), now, win, now, userID, now).Scan(&n, &total)
	if err != nil {
		return 0, 0, true // no row: locked (or a database fault: fail closed)
	}
	return n, total, false
}

// mfaLock locks the user's second factor in one atomic statement; each
// consecutive lock lasts twice as long, up to 16 times the base.
func (s *Service) mfaLock(u User, ip string, longest bool) {
	base := int64(mfaLock.Seconds())
	if longest {
		s.db.Exec(`UPDATE users SET mfa_locked_until = ? + (? << 4), mfa_lockouts = 4,
			mfa_failures = 0 WHERE id=? AND mfa_locked_until <= ?`, s.now(), base, u.ID, s.now())
	} else {
		s.db.Exec(`UPDATE users SET mfa_locked_until = ? + (? << MIN(mfa_lockouts, 4)), mfa_lockouts = mfa_lockouts + 1,
			mfa_failures = 0 WHERE id=? AND mfa_locked_until <= ?`, s.now(), base, u.ID, s.now())
	}
	s.audit(&u, u.Username, ip, "totp_locked", "", "ok", "")
}

// mfaClear: a correct code (or a fresh enrolment) wipes the slate, lock included.
func (s *Service) mfaClear(userID int64) {
	s.db.Exec(`UPDATE users SET mfa_failures=0, mfa_window_start=0, mfa_lockouts=0, mfa_total=0, mfa_locked_until=0 WHERE id=?`, userID)
}

// mfaTotalMax: wrong codes across windows within mfaTotalDecay. A patient
// attacker who stays under the per-window count still ends up in the longest
// lock every time; the counter is never permanent (复核第二、三轮).
const (
	mfaTotalMax   = 20
	mfaTotalDecay = 24 * time.Hour
)

// onCompare is a test hook counting real second-factor comparisons.
var onCompare func()

// checkSecondFactor accepts a current TOTP code (once) or an unused recovery
// code (once). Every attempt is counted against the user first: five wrong
// codes in a window lock the second factor, and wrong codes also count
// against the address. A correct password plus a wrong code is a strong sign
// that the password has leaked, hence the audit event and the lock.
func (s *Service) checkSecondFactor(u User, code, ip string) (ok bool, locked bool) {
	n, total, locked := s.mfaReserve(u.ID)
	if locked {
		s.limit.failN("ip:"+limitIP(ip), loginIPFailures)
		return false, true
	}
	if n > mfaFailures {
		// Reserved beyond the window's budget (a burst that got its numbers
		// before the fifth failure wrote the lock): not compared at all.
		s.limit.failN("ip:"+limitIP(ip), loginIPFailures)
		s.mfaLock(u, ip, total >= mfaTotalMax)
		return false, true
	}
	if onCompare != nil {
		onCompare()
	}
	if s.secondFactorMatches(u, code) {
		s.mfaClear(u.ID)
		return true, false
	}
	s.limit.failN("ip:"+limitIP(ip), loginIPFailures)
	if total >= mfaTotalMax { // slow guessing: every failure now costs the longest lock
		s.mfaLock(u, ip, true)
		return false, true
	}
	if n >= mfaFailures {
		s.mfaLock(u, ip, false)
		return false, true
	}
	return false, false
}

func (s *Service) secondFactorMatches(u User, code string) bool {
	var sealed []byte
	var last int64
	if err := s.db.QueryRow(`SELECT totp_secret, totp_last_step FROM users WHERE id=? AND totp_confirmed_at IS NOT NULL`, u.ID).Scan(&sealed, &last); err != nil {
		return false
	}
	secret, err := s.seal.Open(sealed, fmt.Sprintf("users.totp_secret:%d", u.ID))
	if err != nil {
		return false
	}
	if step, ok := totpMatch(secret, code, s.now(), last); ok {
		// The compare-and-set makes a code single-use even under concurrency.
		res, err := s.db.Exec(`UPDATE users SET totp_last_step=? WHERE id=? AND totp_last_step<?`, step, u.ID, step)
		if err != nil {
			return false
		}
		n, _ := res.RowsAffected()
		return n == 1
	}
	if rc := normalizeRecoveryCode(code); len(rc) >= 12 {
		res, err := s.db.Exec(`UPDATE recovery_codes SET used_at=? WHERE user_id=? AND code_hash=? AND used_at IS NULL`,
			s.now(), u.ID, tokenHash(rc))
		if err == nil {
			if n, _ := res.RowsAffected(); n == 1 {
				s.audit(&u, u.Username, "", "recovery_code_used", "", "ok", "")
				return true
			}
		}
	}
	return false
}

// Authenticate resolves a session cookie. It enforces idle and absolute
// lifetimes and re-reads the user every time, so disabling takes effect at once.
func (s *Service) Authenticate(sessionToken, ip string) (*Identity, error) {
	if sessionToken == "" {
		return nil, ErrUnauthenticated
	}
	h := tokenHash(sessionToken)
	var userID, created, seen, reverified int64
	var csrf string
	err := s.db.QueryRow(`SELECT user_id, created_at, last_seen_at, csrf_token, reverified_until FROM login_sessions WHERE token_hash=?`, h).
		Scan(&userID, &created, &seen, &csrf, &reverified)
	if err != nil {
		return nil, ErrUnauthenticated
	}
	now := s.now()
	if now-seen > int64(s.cfg.SessionIdle.Seconds()) || now-created > int64(s.cfg.SessionMax.Seconds()) {
		s.db.Exec(`DELETE FROM login_sessions WHERE token_hash=?`, h)
		return nil, ErrUnauthenticated
	}
	u, err := s.userByID(userID)
	if err != nil || u.Status != "active" {
		s.db.Exec(`DELETE FROM login_sessions WHERE token_hash=?`, h)
		return nil, ErrUnauthenticated
	}
	if now-seen >= 60 { // keep writes rare
		s.db.Exec(`UPDATE login_sessions SET last_seen_at=? WHERE token_hash=?`, now, h)
	}
	id := &Identity{User: u, CSRF: csrf, Reverified: reverified > now, sessHash: h, IP: ip}
	if id.Reverified {
		id.ReverifiedUntil = reverified
	}
	if u.MustChangePassword {
		id.Pending = append(id.Pending, "change_password")
	}
	if !u.TOTPConfirmed {
		id.Pending = append(id.Pending, "enroll_totp")
	}
	return id, nil
}

func (s *Service) Logout(id *Identity) {
	s.db.Exec(`DELETE FROM login_sessions WHERE token_hash=?`, id.sessHash)
	s.audit(&id.User, id.User.Username, id.IP, "logout", "", "ok", "")
}

// Reverify grants the session the elevated state for sensitive operations.
func (s *Service) Reverify(id *Identity, code string) error {
	key := fmt.Sprintf("reverify:%x", id.sessHash)
	// Reserved before the check (安全复核 C1): the sixth concurrent guess on a
	// login session ends that session.
	if !s.limit.try(key, 5) {
		s.db.Exec(`DELETE FROM login_sessions WHERE token_hash=?`, id.sessHash)
		return ErrUnauthenticated
	}
	if ok, locked := s.checkSecondFactor(id.User, code, id.IP); !ok {
		s.audit(&id.User, id.User.Username, id.IP, "reverify", "", "failed", "")
		if locked {
			return ErrSecondFactorLocked
		}
		return ErrTOTPInvalid
	}
	s.limit.reset(key)
	until := s.cfg.Now().Add(s.cfg.ReverifyFor).Unix()
	_, err := s.db.Exec(`UPDATE login_sessions SET reverified_until=? WHERE token_hash=?`, until, id.sessHash)
	id.Reverified = err == nil
	if id.Reverified {
		id.ReverifiedUntil = until
	}
	return err
}

// FreshlyReverified: reverified within the last minute, for an act that asks
// every time rather than once per ReverifyFor (taking over someone else's
// session).
func (s *Service) FreshlyReverified(id *Identity) bool {
	return id.Reverified && id.ReverifiedUntil-s.now() >= int64((s.cfg.ReverifyFor-time.Minute).Seconds())
}

// revokeAll ends a user's sessions (optionally sparing one) and forgets all
// trusted devices: after a password or TOTP change nothing old may remain valid.
func (s *Service) revokeAll(userID int64, keep []byte) {
	if keep != nil {
		s.db.Exec(`DELETE FROM login_sessions WHERE user_id=? AND token_hash<>?`, userID, keep)
	} else {
		s.db.Exec(`DELETE FROM login_sessions WHERE user_id=?`, userID)
	}
	s.db.Exec(`DELETE FROM trusted_devices WHERE user_id=?`, userID)
	s.clearTickets(userID)
}

// clearTickets ends a user's half-finished logins.
func (s *Service) clearTickets(userID int64) {
	s.mu.Lock()
	for k, v := range s.tickets {
		if v.userID == userID {
			delete(s.tickets, k)
		}
	}
	s.mu.Unlock()
}

// ChangePassword is for the user themself. During the forced first-login step
// no second factor exists yet; afterwards the session must be reverified.
func (s *Service) ChangePassword(id *Identity, oldPw, newPw string) error {
	if id.User.TOTPConfirmed && !id.User.MustChangePassword && !id.Reverified {
		return ErrReverifyRequired
	}
	var hash string
	if err := s.db.QueryRow(`SELECT password_hash FROM users WHERE id=?`, id.User.ID).Scan(&hash); err != nil {
		return err
	}
	if !s.verify(oldPw, hash) {
		return ErrInvalidCredentials
	}
	if err := checkPassword(id.User.Username, newPw); err != nil {
		return err
	}
	if oldPw == newPw {
		return ErrWeakPassword
	}
	if _, err := s.db.Exec(`UPDATE users SET password_hash=?, must_change_password=0 WHERE id=?`, s.hash(newPw), id.User.ID); err != nil {
		return err
	}
	s.revokeAll(id.User.ID, id.sessHash)
	s.audit(&id.User, id.User.Username, id.IP, "password_changed", "", "ok", "")
	return nil
}

// TOTPBegin creates a new, unconfirmed secret and returns it for display.
func (s *Service) TOTPBegin(id *Identity) (secretText, uri string, err error) {
	if id.User.TOTPConfirmed && !id.Reverified {
		return "", "", ErrReverifyRequired
	}
	if id.User.MustChangePassword {
		return "", "", ErrStepsPending
	}
	secret := newTOTPSecret()
	sealed := s.seal.Seal(secret, fmt.Sprintf("users.totp_pending:%d", id.User.ID))
	if _, err := s.db.Exec(`INSERT INTO settings(key,value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		fmt.Sprintf("totp_pending:%d", id.User.ID), fmt.Sprintf("%x", sealed)); err != nil {
		return "", "", err
	}
	return totpSecretText(secret), totpURI(s.cfg.Issuer, id.User.Username, secret), nil
}

// TOTPConfirm activates the pending secret once the user proves they can
// produce a code, and returns fresh recovery codes, shown exactly once.
func (s *Service) TOTPConfirm(id *Identity, code string) ([]string, error) {
	if id.User.TOTPConfirmed && !id.Reverified { // replacing a factor is a sensitive operation
		return nil, ErrReverifyRequired
	}
	key := fmt.Sprintf("totp_pending:%d", id.User.ID)
	// Five tries per login session, reserved before the compare; then the
	// pending secret is discarded (复核第二轮: TOTPConfirm 不限次数).
	if !s.limit.try(fmt.Sprintf("enrol:%x", id.sessHash), 5) {
		s.db.Exec(`DELETE FROM settings WHERE key=?`, key)
		return nil, ErrTOTPInvalid
	}
	var hexSealed string
	if err := s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&hexSealed); err != nil {
		return nil, ErrBadRequest
	}
	var sealed []byte
	fmt.Sscanf(hexSealed, "%x", &sealed)
	secret, err := s.seal.Open(sealed, fmt.Sprintf("users.totp_pending:%d", id.User.ID))
	if err != nil {
		return nil, ErrBadRequest
	}
	step, ok := totpMatch(secret, code, s.now(), 0)
	if !ok {
		return nil, ErrTOTPInvalid
	}
	final := s.seal.Seal(secret, fmt.Sprintf("users.totp_secret:%d", id.User.ID))
	if _, err := s.db.Exec(`UPDATE users SET totp_secret=?, totp_confirmed_at=?, totp_last_step=? WHERE id=?`,
		final, s.now(), step, id.User.ID); err != nil {
		return nil, err
	}
	s.db.Exec(`DELETE FROM settings WHERE key=?`, key)
	s.mfaClear(id.User.ID)     // a new authenticator starts with a clean record
	if id.User.TOTPConfirmed { // a replacement, not the first enrolment
		s.revokeAll(id.User.ID, id.sessHash)
	}
	s.audit(&id.User, id.User.Username, id.IP, "totp_enrolled", "", "ok", "")
	return s.issueRecoveryCodes(id.User.ID)
}

func (s *Service) issueRecoveryCodes(userID int64) ([]string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM recovery_codes WHERE user_id=?`, userID); err != nil {
		return nil, err
	}
	codes := make([]string, 10)
	for i := range codes {
		codes[i] = newRecoveryCode()
		if _, err := tx.Exec(`INSERT INTO recovery_codes(user_id, code_hash) VALUES (?,?)`, userID, tokenHash(normalizeRecoveryCode(codes[i]))); err != nil {
			return nil, err
		}
	}
	return codes, tx.Commit()
}

// RegenerateRecoveryCodes replaces all recovery codes.
func (s *Service) RegenerateRecoveryCodes(id *Identity) ([]string, error) {
	if !id.Reverified {
		return nil, ErrReverifyRequired
	}
	s.audit(&id.User, id.User.Username, id.IP, "recovery_codes_regenerated", "", "ok", "")
	return s.issueRecoveryCodes(id.User.ID)
}

// Device is a trusted device as shown to its owner.
type Device struct {
	ID         string // hex of the token hash; safe to show
	Name       string
	CreatedAt  int64
	LastUsedAt int64
	ExpiresAt  int64
}

func (s *Service) ListDevices(userID int64) ([]Device, error) {
	rows, err := s.db.Query(`SELECT hex(token_hash), name, created_at, last_used_at, expires_at FROM trusted_devices WHERE user_id=? AND expires_at>? ORDER BY last_used_at DESC`, userID, s.now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Device
	for rows.Next() {
		var d Device
		if err := rows.Scan(&d.ID, &d.Name, &d.CreatedAt, &d.LastUsedAt, &d.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// RevokeDevice removes one trusted device of the acting user, and with it
// every login made from that device: a lost phone is out at once, not in
// a week (复核第四轮 5).
func (s *Service) RevokeDevice(id *Identity, deviceID string) error {
	var hash []byte
	if err := s.db.QueryRow(`SELECT token_hash FROM trusted_devices WHERE user_id=? AND hex(token_hash)=?`, id.User.ID, strings.ToUpper(deviceID)).Scan(&hash); err != nil {
		return ErrNotFound
	}
	if _, err := s.db.Exec(`DELETE FROM trusted_devices WHERE token_hash=?`, hash); err != nil {
		return err
	}
	s.db.Exec(`DELETE FROM login_sessions WHERE user_id=? AND device_hash=?`, id.User.ID, hash)
	s.audit(&id.User, id.User.Username, id.IP, "device_revoked", deviceID, "ok", "")
	return nil
}

// LoginSession is one browser login of the acting user (docs/M6 第 12 节).
type LoginSession struct {
	ID         string `json:"id"`
	Current    bool   `json:"current"`
	IP         string `json:"ip"`
	UserAgent  string `json:"user_agent"`
	CreatedAt  int64  `json:"created_at"`
	LastSeenAt int64  `json:"last_seen_at"`
}

// ListLoginSessions lists the acting user's logins, the current one marked.
func (s *Service) ListLoginSessions(id *Identity) ([]LoginSession, error) {
	rows, err := s.db.Query(`SELECT token_hash, ip, user_agent, created_at, last_seen_at FROM login_sessions WHERE user_id=? ORDER BY last_seen_at DESC`, id.User.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LoginSession{}
	for rows.Next() {
		var h []byte
		var l LoginSession
		if err := rows.Scan(&h, &l.IP, &l.UserAgent, &l.CreatedAt, &l.LastSeenAt); err != nil {
			return nil, err
		}
		l.ID = fmt.Sprintf("%x", h)
		l.Current = hmac.Equal(h, id.sessHash)
		out = append(out, l)
	}
	return out, rows.Err()
}

// RevokeLoginSession ends one of the acting user's logins (their own included).
func (s *Service) RevokeLoginSession(id *Identity, sessID string) error {
	h, err := hex.DecodeString(sessID)
	if err != nil || len(h) == 0 {
		return ErrNotFound
	}
	res, err := s.db.Exec(`DELETE FROM login_sessions WHERE user_id=? AND token_hash=?`, id.User.ID, h)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	s.audit(&id.User, id.User.Username, id.IP, "login_session_revoked", sessID[:min(16, len(sessID))], "ok", "")
	return nil
}

// ---- administration (docs/M6 第 10 节). Every write needs a reverified admin. ----

func (s *Service) admin(actor *Identity) error {
	if !actor.User.IsAdmin() {
		return ErrForbidden
	}
	if !actor.Reverified {
		return ErrReverifyRequired
	}
	return nil
}

func (s *Service) ListUsers(actor *Identity) ([]User, error) {
	if !actor.User.IsAdmin() {
		return nil, ErrForbidden
	}
	rows, err := s.db.Query(`SELECT ` + userCols + ` FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// CreateUser returns a temporary password, shown once. The user must change
// it and enrol TOTP at first login.
func (s *Service) CreateUser(actor *Identity, username, displayName, role string) (tempPassword string, err error) {
	if err := s.admin(actor); err != nil {
		return "", err
	}
	if err := checkUsername(username); err != nil {
		return "", err
	}
	if role != "admin" && role != "user" {
		return "", ErrBadRequest
	}
	tempPassword = newRecoveryCode() + "-" + newRecoveryCode()[:4]
	_, err = s.db.Exec(`INSERT INTO users(username, display_name, role, password_hash, must_change_password, created_at) VALUES (?,?,?,?,1,?)`,
		username, displayName, role, s.hash(tempPassword), s.now())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return "", ErrConflict
		}
		return "", err
	}
	s.audit(&actor.User, actor.User.Username, actor.IP, "user_created", username, "ok", "role="+role)
	return tempPassword, nil
}

// CreateUserLocal is CreateUser for the command line on the Hub host (no web
// identity): the same checks and the same temporary-password flow, audited
// as "local". Whoever can run the binary owns the data directory anyway.
func (s *Service) CreateUserLocal(username, role string) (tempPassword string, err error) {
	if err := checkUsername(username); err != nil {
		return "", err
	}
	if role != "admin" && role != "user" {
		return "", ErrBadRequest
	}
	tempPassword = newRecoveryCode() + "-" + newRecoveryCode()[:4]
	_, err = s.db.Exec(`INSERT INTO users(username, display_name, role, password_hash, must_change_password, created_at) VALUES (?,?,?,?,1,?)`,
		username, "", role, s.hash(tempPassword), s.now())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return "", ErrConflict
		}
		return "", err
	}
	s.audit(nil, "local", "local", "user_created", username, "ok", "role="+role)
	return tempPassword, nil
}

// SetStatus enables or disables an account. Users are never deleted, so the
// audit trail stays meaningful.
func (s *Service) SetStatus(actor *Identity, userID int64, status string) error {
	if err := s.admin(actor); err != nil {
		return err
	}
	if status != "active" && status != "disabled" {
		return ErrBadRequest
	}
	u, err := s.userByID(userID)
	if err != nil {
		return err
	}
	// Check the current row and the other admins in the same write (#7 F07).
	res, err := s.db.Exec(`UPDATE users SET status=? WHERE id=? AND
		(?<>'disabled' OR role<>'admin' OR status<>'active' OR EXISTS
		(SELECT 1 FROM users other WHERE other.id<>users.id AND other.role='admin' AND other.status='active'))`, status, userID, status)
	if err != nil {
		return err
	}
	if changed, _ := res.RowsAffected(); changed == 0 {
		return ErrLastAdmin
	}
	if status == "disabled" {
		s.revokeAll(userID, nil)
	}
	s.audit(&actor.User, actor.User.Username, actor.IP, "user_status", u.Username, "ok", status)
	return nil
}

func (s *Service) SetRole(actor *Identity, userID int64, role string) error {
	if err := s.admin(actor); err != nil {
		return err
	}
	if role != "admin" && role != "user" {
		return ErrBadRequest
	}
	u, err := s.userByID(userID)
	if err != nil {
		return err
	}
	res, err := s.db.Exec(`UPDATE users SET role=? WHERE id=? AND
		(?<>'user' OR role<>'admin' OR status<>'active' OR EXISTS
		(SELECT 1 FROM users other WHERE other.id<>users.id AND other.role='admin' AND other.status='active'))`, role, userID, role)
	if err != nil {
		return err
	}
	if changed, _ := res.RowsAffected(); changed == 0 {
		return ErrLastAdmin
	}
	s.audit(&actor.User, actor.User.Username, actor.IP, "user_role", u.Username, "ok", role)
	return err
}

// ResetPassword sets a temporary password and logs the user out everywhere.
func (s *Service) ResetPassword(actor *Identity, userID int64) (string, error) {
	if err := s.admin(actor); err != nil {
		return "", err
	}
	u, err := s.userByID(userID)
	if err != nil {
		return "", err
	}
	temp := newRecoveryCode() + "-" + newRecoveryCode()[:4]
	if _, err := s.db.Exec(`UPDATE users SET password_hash=?, must_change_password=1 WHERE id=?`, s.hash(temp), userID); err != nil {
		return "", err
	}
	s.revokeAll(userID, nil)
	s.audit(&actor.User, actor.User.Username, actor.IP, "password_reset", u.Username, "ok", "")
	return temp, nil
}

// ResetTOTP removes a user's second factor. The password is replaced by a
// temporary one at the same time (安全复核 M4): a "lost authenticator" may
// really be a stolen password, and whoever holds only the password must not
// be the one who enrols the new authenticator. The temporary password is
// shown once to the administrator who did the reset.
func (s *Service) ResetTOTP(actor *Identity, userID int64) (tempPassword string, err error) {
	if err := s.admin(actor); err != nil {
		return "", err
	}
	u, err := s.userByID(userID)
	if err != nil {
		return "", err
	}
	return s.resetTOTP(u, &actor.User, actor.IP)
}

// ResetTOTPLocal is the break-glass path of `termhub admin reset-totp`, run
// inside the container by whoever owns the NAS (docs/M6 第 10 节).
func (s *Service) ResetTOTPLocal(username string) (tempPassword string, err error) {
	var id int64
	if err := s.db.QueryRow(`SELECT id FROM users WHERE username=?`, username).Scan(&id); err != nil {
		return "", ErrNotFound
	}
	u, err := s.userByID(id)
	if err != nil {
		return "", err
	}
	return s.resetTOTP(u, nil, "local")
}

func (s *Service) resetTOTP(u User, actor *User, ip string) (string, error) {
	temp := newRecoveryCode() + "-" + newRecoveryCode()[:4]
	if _, err := s.db.Exec(`UPDATE users SET totp_secret=NULL, totp_confirmed_at=NULL, totp_last_step=0,
		password_hash=?, must_change_password=1, mfa_failures=0, mfa_locked_until=0, mfa_lockouts=0, mfa_total=0 WHERE id=?`, s.hash(temp), u.ID); err != nil {
		return "", err
	}
	s.db.Exec(`DELETE FROM recovery_codes WHERE user_id=?`, u.ID)
	s.db.Exec(`DELETE FROM settings WHERE key=?`, fmt.Sprintf("totp_pending:%d", u.ID))
	s.revokeAll(u.ID, nil)
	name := "local"
	if actor != nil {
		name = actor.Username
	}
	s.audit(actor, name, ip, "totp_reset", u.Username, "ok", "")
	return temp, nil
}

func (s *Service) ForceLogout(actor *Identity, userID int64) error {
	if err := s.admin(actor); err != nil {
		return err
	}
	u, err := s.userByID(userID)
	if err != nil {
		return err
	}
	s.revokeAll(userID, nil) // sessions, trusted devices and half-finished logins
	s.audit(&actor.User, actor.User.Username, actor.IP, "force_logout", u.Username, "ok", "")
	return nil
}

// ---- audit (docs/M6 第 11 节) ----

// AuditEntry is one row of the audit log.
type AuditEntry struct {
	ID       int64
	At       int64
	Username string
	IP       string
	Event    string
	Object   string
	Result   string
	Detail   string
}

// Audit is used by the other Hub modules too. Callers must never pass
// passwords, codes, tokens or terminal content.
func (s *Service) Audit(actor *User, ip, event, object, result, detail string) {
	name := ""
	if actor != nil {
		name = actor.Username
	}
	s.audit(actor, name, ip, event, object, result, detail)
}

func (s *Service) audit(actor *User, username, ip, event, object, result, detail string) {
	if len(username) > 64 { // a password typed into the username box must not land here whole
		username = username[:64]
	}
	var uid any
	if actor != nil {
		uid = actor.ID
	}
	s.db.Exec(`INSERT INTO audit_log(at,user_id,username,ip,event,object,result,detail) VALUES (?,?,?,?,?,?,?,?)`,
		s.now(), uid, username, ip, event, object, result, detail)
}

// QueryAudit returns the newest entries first.
func (s *Service) QueryAudit(actor *Identity, event, username string, limit int) ([]AuditEntry, error) {
	if !actor.User.IsAdmin() {
		return nil, ErrForbidden
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.db.Query(`SELECT id,at,username,ip,event,object,result,detail FROM audit_log
		WHERE (?='' OR event=?) AND (?='' OR username=?) ORDER BY id DESC LIMIT ?`, event, event, username, username, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var a AuditEntry
		if err := rows.Scan(&a.ID, &a.At, &a.Username, &a.IP, &a.Event, &a.Object, &a.Result, &a.Detail); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// PruneAudit deletes entries older than keep.
func (s *Service) PruneAudit(keep time.Duration) {
	s.db.Exec(`DELETE FROM audit_log WHERE at<?`, s.cfg.Now().Add(-keep).Unix())
}
