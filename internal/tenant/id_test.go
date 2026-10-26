package tenant

import (
	"strings"
	"testing"
)

func TestValid(t *testing.T) {
	cases := []struct {
		id   string
		want bool
	}{
		{"a", true},
		{"0", true},
		{"ab-cd", true},
		{"a1b2", true},
		{strings.Repeat("a", 63), true},
		{strings.Repeat("a", 64), false},
		{"", false},
		{"-a", false},
		{"a-", false},
		{"A", false},
		{"a_b", false},
		{"a.b", false},
		{"a b", false},
		{"a\n", false},
		{"é", false},
	}
	for _, tc := range cases {
		if got := Valid(tc.id); got != tc.want {
			t.Errorf("Valid(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

func TestNewID(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		id := NewID()
		if !Valid(id) {
			t.Fatalf("NewID() = %q does not match the tenant rule", id)
		}
		if len(id) != idLen {
			t.Fatalf("NewID() = %q, want length %d", id, idLen)
		}
		if seen[id] {
			t.Fatalf("NewID() repeated %q", id)
		}
		seen[id] = true
	}
}
