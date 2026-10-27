// Package push stores Web Push subscriptions and delivers alerts to them
// (US-016, US-023). Subscriptions belong to a tenant; the control plane, not
// the tenant's bot, holds them and the VAPID keys.
package push

import (
	"crypto/ecdh"
	"encoding/base64"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
)

// Limits on what a browser may hand us.
const (
	MaxEndpointLen = 2048
	authLen        = 16
	p256dhLen      = 65
)

// ErrInvalid is a subscription that failed validation. The message names the
// field, never the value.
var ErrInvalid = errors.New("push: invalid subscription")

// Subscription is a browser's PushSubscription (toJSON form).
type Subscription struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

func invalid(field string) error { return errors.New("push: invalid subscription: " + field) }

// Validate checks the endpoint (https, a public DNS name, port 443, bounded)
// and the keys (base64url; p256dh a real P-256 point, auth 16 bytes).
func (s Subscription) Validate() error {
	if err := ValidateEndpoint(s.Endpoint); err != nil {
		return err
	}
	p, err := base64.RawURLEncoding.DecodeString(s.Keys.P256dh)
	if err != nil || len(p) != p256dhLen {
		return invalid("p256dh")
	}
	if _, err := ecdh.P256().NewPublicKey(p); err != nil {
		return invalid("p256dh")
	}
	a, err := base64.RawURLEncoding.DecodeString(s.Keys.Auth)
	if err != nil || len(a) != authLen {
		return invalid("auth")
	}
	return nil
}

// blockedSuffixes are names that are never a browser vendor's push service.
var blockedSuffixes = []string{".localhost", ".local", ".internal", ".lan", ".home", ".home.arpa", ".corp", ".intranet", ".private"}

// ValidateEndpoint is the SSRF gate at subscribe time: the control plane will
// POST to this URL, so it must be https on the default port, name a public
// DNS host (no IP literals, no single-label or internal names) and carry no
// credentials. Dial time checks the resolved address again (SafeDialControl).
func ValidateEndpoint(raw string) error {
	if raw == "" || len(raw) > MaxEndpointLen {
		return invalid("endpoint")
	}
	for _, r := range raw {
		if r < 0x21 || r > 0x7e {
			return invalid("endpoint")
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || u.Opaque != "" || u.Host == "" {
		return invalid("endpoint")
	}
	if p := u.Port(); p != "" && p != "443" {
		return invalid("endpoint")
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" || len(host) > 253 || !strings.Contains(host, ".") || host == "localhost" {
		return invalid("endpoint")
	}
	if _, err := netip.ParseAddr(host); err == nil || strings.ContainsAny(host, ":[]") {
		return invalid("endpoint")
	}
	for _, sfx := range blockedSuffixes {
		if strings.HasSuffix(host, sfx) {
			return invalid("endpoint")
		}
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return invalid("endpoint")
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return invalid("endpoint")
			}
		}
	}
	// A numeric last label cannot be a real TLD (and 127.1 style shorthand
	// parses as an address in some resolvers).
	last := host[strings.LastIndex(host, ".")+1:]
	if last[0] >= '0' && last[0] <= '9' {
		return invalid("endpoint")
	}
	return nil
}

// blockedPrefixes are ranges netip's own predicates do not cover.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("2002::/16"),
}

// PublicIP reports whether ip is an address a push service could have.
func PublicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// SafeDialControl is a net.Dialer Control function that refuses to connect to
// a non-public address. It runs on the resolved address of every connection
// attempt, so a name that resolves (or later rebinds) to an internal address
// is refused.
func SafeDialControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("push: bad dial address")
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !PublicIP(ip) {
		return errors.New("push: refusing to connect to a non-public address")
	}
	return nil
}
