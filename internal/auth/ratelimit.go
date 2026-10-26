package auth

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"time"
)

// Allow counts one hit on key in the current fixed window and reports
// whether the count is still within limit. Counters live in PostgreSQL, so
// every instance shares them.
func (s *Service) Allow(ctx context.Context, key string, limit int, window time.Duration) (bool, error) {
	start := s.now().Truncate(window)
	var n int
	err := s.pool.QueryRow(ctx, `
		INSERT INTO rate_counters (key, window_start, count) VALUES ($1, $2, 1)
		ON CONFLICT (key, window_start) DO UPDATE SET count = rate_counters.count + 1
		RETURNING count`, key, start).Scan(&n)
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
func ClientIP(peer netip.Addr, xff string, trusted []netip.Prefix) string {
	peer = peer.Unmap()
	if !inAny(peer, trusted) || xff == "" {
		return peer.String()
	}
	parts := strings.Split(xff, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(parts[i]))
		if err != nil {
			return peer.String()
		}
		a = a.Unmap()
		if !inAny(a, trusted) {
			return a.String()
		}
	}
	return peer.String()
}

func inAny(a netip.Addr, prefixes []netip.Prefix) bool {
	for _, p := range prefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
