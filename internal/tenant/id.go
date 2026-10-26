// Package tenant holds the tenant identity rules. A tenant id becomes part of
// the Nomad job id, the qm path and the subdomain, so the rule is qm's:
// 1 to 63 characters, lowercase letters, digits and hyphens, starting and
// ending with a letter or digit. The same pattern is a CHECK constraint on
// tenants.id (migration 0001).
package tenant

import (
	"crypto/rand"
	"regexp"
	"strings"
)

// Pattern is the tenant id rule.
var Pattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// idLen is 16 characters of base32 (80 bits): short enough for job ids and
// hostnames, long enough that ids cannot be guessed or collide in practice.
const idLen = 16

// Valid reports whether id satisfies the tenant id rule.
func Valid(id string) bool { return Pattern.MatchString(id) }

// NewID returns a random tenant id from crypto/rand. rand.Text yields
// uppercase base32 (A-Z, 2-7), which lowercases into the allowed alphabet.
func NewID() string {
	return strings.ToLower(rand.Text())[:idLen]
}
