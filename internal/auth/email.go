// Package auth holds the account lifecycle of SanHolo Hosted: waitlist,
// invites, sign-up under the tenant cap, magic-link sign-in, sessions and
// the rate limits in front of them. Secrets are random, at least 128 bits,
// and stored only as SHA-256 hashes.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"unicode"
)

const maxEmailLen = 254

// ValidateEmail checks an address from a form. It rejects, anywhere in the
// string, control characters (CR and LF included, so a value can never reach
// a mail header), whitespace and the characters of address-list syntax, and
// requires exactly one @ with a dotted domain. It does not trim: trimming
// would strip an edge CR or LF and hide the attempt.
func ValidateEmail(s string) (string, error) {
	if s == "" || len(s) > maxEmailLen {
		return "", errors.New("email: empty or too long")
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.IsSpace(r) || strings.ContainsRune(`<>,;:"()[]\`, r) {
			return "", errors.New("email: contains a character that is not allowed")
		}
	}
	if strings.Count(s, "@") != 1 {
		return "", errors.New("email: want exactly one @")
	}
	local, domain, _ := strings.Cut(s, "@")
	if local == "" || domain == "" || !strings.Contains(domain, ".") ||
		strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") || strings.Contains(domain, "..") {
		return "", errors.New("email: not an address")
	}
	return s, nil
}

// HashEmail is the case-insensitive key of an address: SHA-256 hex of the
// lower-cased text. SQL computes the same value for accounts (migration 0002).
func HashEmail(email string) string {
	return HashSecret(strings.ToLower(email))
}

// NewSecret returns 256 bits from crypto/rand as base64url text. Invite
// codes, link tokens, session ids and CSRF tokens all come from here.
func NewSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand.Read never fails on supported platforms (Go 1.24+)
	return base64.RawURLEncoding.EncodeToString(b)
}

// HashSecret is the stored form of a secret: SHA-256 hex.
func HashSecret(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
