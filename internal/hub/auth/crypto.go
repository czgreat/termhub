// Package auth implements docs/M6-账号与认证.md.
package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// HashParams are the argon2id cost parameters (docs/M6 第 2 节).
type HashParams struct {
	MemoryKiB uint32
	Time      uint32
	Threads   uint8
}

// DefaultHashParams: 64 MiB, 3 passes, 2 lanes.
var DefaultHashParams = HashParams{MemoryKiB: 64 << 10, Time: 3, Threads: 2}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("auth: no entropy: " + err.Error())
	}
	return b
}

// newToken returns a 256-bit random token for cookies and tickets.
func newToken() string { return base64.RawURLEncoding.EncodeToString(randomBytes(32)) }

// tokenHash is what the database stores instead of a token.
func tokenHash(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// hashPassword returns a self-describing argon2id hash.
func hashPassword(password string, p HashParams) string {
	salt := randomBytes(16)
	key := argon2.IDKey([]byte(password), salt, p.Time, p.MemoryKiB, p.Threads, 32)
	return fmt.Sprintf("argon2id$%d$%d$%d$%s$%s", p.MemoryKiB, p.Time, p.Threads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
}

// verifyPassword checks password against an encoded hash in constant time.
func verifyPassword(password, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "argon2id" {
		return false
	}
	var mem, t uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[1]+" "+parts[2]+" "+parts[3], "%d %d %d", &mem, &t, &threads); err != nil {
		return false
	}
	// Bound the cost so a corrupted row cannot be turned into a memory bomb.
	if mem == 0 || mem > 1<<21 || t == 0 || t > 16 || threads == 0 {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[4])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[5])
	if err1 != nil || err2 != nil || len(want) == 0 {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, t, mem, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// ---- TOTP, RFC 6238: SHA-1, 6 digits, 30 second steps ----

const totpPeriod = 30

func newTOTPSecret() []byte { return randomBytes(20) }

func totpSecretText(secret []byte) string {
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secret)
}

func totpURI(issuer, account string, secret []byte) string {
	esc := func(s string) string {
		return strings.NewReplacer(" ", "%20", ":", "%3A", "?", "%3F", "&", "%26").Replace(s)
	}
	return "otpauth://totp/" + esc(issuer) + ":" + esc(account) + "?secret=" + totpSecretText(secret) +
		"&issuer=" + esc(issuer) + "&algorithm=SHA1&digits=6&period=30"
}

func totpCode(secret []byte, step int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	mac := hmac.New(sha1.New, secret)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", v%1000000)
}

// totpMatch checks code against the steps around unixTime and returns the
// matching step. Steps at or below lastStep are refused: a code works once.
func totpMatch(secret []byte, code string, unixTime, lastStep int64) (step int64, ok bool) {
	code = strings.TrimSpace(code)
	if len(code) != 6 {
		return 0, false
	}
	now := unixTime / totpPeriod
	for _, s := range []int64{now, now - 1, now + 1} {
		if s > lastStep && subtle.ConstantTimeCompare([]byte(totpCode(secret, s)), []byte(code)) == 1 {
			return s, true
		}
	}
	return 0, false
}

// ---- secrets at rest, AES-256-GCM under the Hub master key (docs/M8 第 3 节) ----

// Sealer encrypts small secrets for storage.
type Sealer struct{ aead cipher.AEAD }

// NewSealer takes the 32-byte master key.
func NewSealer(key []byte) (*Sealer, error) {
	if len(key) != 32 {
		return nil, errors.New("auth: master key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead}, nil
}

// Seal encrypts plain; context binds the ciphertext to its row and column so
// it cannot be moved elsewhere.
func (s *Sealer) Seal(plain []byte, context string) []byte {
	nonce := randomBytes(s.aead.NonceSize())
	return s.aead.Seal(nonce, nonce, plain, []byte(context))
}

// Open reverses Seal.
func (s *Sealer) Open(sealed []byte, context string) ([]byte, error) {
	n := s.aead.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("auth: sealed value too short")
	}
	return s.aead.Open(nil, sealed[:n], sealed[n:], []byte(context))
}

// newRecoveryCode returns a code like "k7qd-9xwm-3f2h": 60 bits, easy to type.
func newRecoveryCode() string {
	const alphabet = "abcdefghjkmnpqrstuvwxyz23456789" // no look-alikes
	b := randomBytes(12)
	var sb strings.Builder
	for i, v := range b {
		if i > 0 && i%4 == 0 {
			sb.WriteByte('-')
		}
		sb.WriteByte(alphabet[int(v)%len(alphabet)])
	}
	return sb.String()
}

func normalizeRecoveryCode(s string) string {
	return strings.ToLower(strings.NewReplacer(" ", "", "\t", "").Replace(strings.TrimSpace(s)))
}

// HashParamsFromEnv reads TH_ARGON_MEM_MB the way the server does, so that
// users made or reset from the command line get the same parameters and the
// dummy hash for unknown names takes the same time (复核第四轮 10).
func HashParamsFromEnv(getenv func(string) string) (HashParams, error) {
	h := DefaultHashParams
	if v := getenv("TH_ARGON_MEM_MB"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 8 || n > 1024 {
			return h, errors.New("TH_ARGON_MEM_MB must be between 8 and 1024")
		}
		h.MemoryKiB = uint32(n) << 10
	}
	return h, nil
}
