package auth

import (
	"net/netip"
	"testing"
)

func TestClientAddr(t *testing.T) {
	edge := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	tests := []struct {
		name, peer, xff, want string
	}{
		{"direct peer, header ignored", "198.51.100.9", "203.0.113.5", "198.51.100.9"},
		{"edge forwards the client", "10.0.0.2", "203.0.113.5", "203.0.113.5"},
		{"client-supplied entry is skipped", "10.0.0.2", "1.2.3.4, 203.0.113.5", "203.0.113.5"},
		{"trusted hops are skipped", "10.0.0.2", "203.0.113.5, 10.0.0.9", "203.0.113.5"},
		{"garbage falls back to the peer", "10.0.0.2", "not-an-ip", "10.0.0.2"},
		{"ipv6 is not masked", "10.0.0.2", "2001:db8:1:2:3:4:5:6", "2001:db8:1:2:3:4:5:6"},
	}
	for _, tc := range tests {
		got := ClientAddr(netip.MustParseAddr(tc.peer), tc.xff, edge)
		if got.String() != tc.want {
			t.Errorf("%s: %v, want %s", tc.name, got, tc.want)
		}
	}
}
