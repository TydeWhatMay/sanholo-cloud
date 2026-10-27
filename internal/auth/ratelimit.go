package auth

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Allow counts one hit on key in the current fixed window and reports
// whether the count is still within limit. Counters live in PostgreSQL, so
// every instance shares them. A counter stops at limit+1: once a key is over
// its limit further hits write nothing.
func (s *Service) Allow(ctx context.Context, key string, limit int, window time.Duration) (bool, error) {
	start := s.now().Truncate(window)
	var n int
	err := s.pool.QueryRow(ctx, `
		INSERT INTO rate_counters (key, window_start, count) VALUES ($1, $2, 1)
		ON CONFLICT (key, window_start) DO UPDATE SET count = rate_counters.count + 1
		WHERE rate_counters.count <= $3
		RETURNING count`, key, start, limit).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // already over the limit; nothing was counted
	}
	if err != nil {
		return false, fmt.Errorf("auth: rate limit: %w", err)
	}
	return n <= limit, nil
}

// ClientIP returns the address to key a rate limit on. X-Forwarded-For is
// believed only when the immediate peer is in trusted (the edge); then the
// right-most entry that is not itself a trusted proxy is the client. From any
// other peer the header is attacker-controlled and ignored. Behind the edge
// the peer is always the edge, so keying on it would be one global counter.
// IPv6 clients are keyed on their /64, the smallest block a customer holds,
// so one host cannot take a fresh key per address; an IPv4-mapped IPv6
// address is an IPv4 address.
func ClientIP(peer netip.Addr, xff string, trusted []netip.Prefix) string {
	return limitKey(ClientAddr(peer, xff, trusted))
}

// ClientAddr is the address ClientIP keys on, unmasked: the real client
// address behind the edge, by the same trust rule. The proxy sends it to a
// Bridge as X-Forwarded-For.
func ClientAddr(peer netip.Addr, xff string, trusted []netip.Prefix) netip.Addr {
	peer = peer.Unmap()
	if !inAny(peer, trusted) || xff == "" {
		return peer
	}
	parts := strings.Split(xff, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(parts[i]))
		if err != nil {
			return peer
		}
		a = a.Unmap()
		if !inAny(a, trusted) {
			return a
		}
	}
	return peer
}

// limitKey is the rate-limit identity of an address: IPv4 as is, IPv6 as its
// /64.
func limitKey(a netip.Addr) string {
	if a.Is6() {
		return netip.PrefixFrom(a.WithZone(""), 64).Masked().String()
	}
	return a.String()
}

func inAny(a netip.Addr, prefixes []netip.Prefix) bool {
	for _, p := range prefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
