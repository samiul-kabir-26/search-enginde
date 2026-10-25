package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// PasswordParams are argon2id parameters (PLAN.md R7: OWASP baseline m = 46 MiB, t = 1, p = 1).
type PasswordParams struct {
	MemoryKiB uint32 `json:"memory_kib"`
	Time      uint32 `json:"time"`
	Threads   uint8  `json:"threads"`
}

// DefaultPasswordParams is the OWASP argon2id baseline.
func DefaultPasswordParams() PasswordParams {
	return PasswordParams{MemoryKiB: 47104, Time: 1, Threads: 1}
}

const (
	saltLen = 16
	keyLen  = 32
	// MaxPasswordLength bounds hashing cost from oversized inputs.
	MaxPasswordLength = 256
)

// ErrWeakPassword is returned by CheckPasswordPolicy.
var ErrWeakPassword = errors.New("password does not meet the policy")

// CheckPasswordPolicy enforces the minimum and maximum length.
func CheckPasswordPolicy(pw string, minLength int) error {
	n := utf8.RuneCountInString(pw)
	if n < minLength {
		return fmt.Errorf("%w: at least %d characters", ErrWeakPassword, minLength)
	}
	if len(pw) > MaxPasswordLength {
		return fmt.Errorf("%w: at most %d bytes", ErrWeakPassword, MaxPasswordLength)
	}
	return nil
}

// HashPassword returns an argon2id PHC string; the parameters travel with the hash.
func HashPassword(pw string, p PasswordParams) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("salt: %w", err)
	}
	key := argon2.IDKey([]byte(pw), salt, p.Time, p.MemoryKiB, p.Threads, keyLen)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.MemoryKiB, p.Time, p.Threads, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

// ErrBadHash means a stored hash is not a parseable argon2id PHC string.
var ErrBadHash = errors.New("invalid password hash")

// VerifyPassword checks pw against a PHC string in constant time. needsRehash reports that the
// hash was made with parameters other than want, so the caller can upgrade it after login.
func VerifyPassword(pw, phc string, want PasswordParams) (ok, needsRehash bool, err error) {
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, false, ErrBadHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, false, ErrBadHash
	}
	var p PasswordParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.MemoryKiB, &p.Time, &p.Threads); err != nil {
		return false, false, ErrBadHash
	}
	enc := base64.RawStdEncoding
	salt, err1 := enc.DecodeString(parts[4])
	key, err2 := enc.DecodeString(parts[5])
	if err1 != nil || err2 != nil || len(key) == 0 || p.Time == 0 || p.Threads == 0 {
		return false, false, ErrBadHash
	}
	got := argon2.IDKey([]byte(pw), salt, p.Time, p.MemoryKiB, p.Threads, uint32(len(key))) //nolint:gosec // len(key) is small
	if subtle.ConstantTimeCompare(got, key) != 1 {
		return false, false, nil
	}
	return true, p != want, nil
}
