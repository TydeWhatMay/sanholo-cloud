package auth

import (
	"strings"
	"testing"
)

func TestValidateEmail(t *testing.T) {
	long := strings.Repeat("a", 250) + "@b.co"
	cases := []struct {
		in   string
		want string // normalised result; empty means rejected
	}{
		{"Lance@Example.org", "Lance@Example.org"},
		{"a.b+c@sub.example.org", "a.b+c@sub.example.org"},
		{"", ""},
		{"no-at.example.org", ""},
		{"two@@example.org", ""},
		{"a@b@example.org", ""},
		{"@example.org", ""},
		{"a@", ""},
		{"a@nodot", ""},
		{"a@.example.org", ""},
		{"a@example..org", ""},
		{"a@example.org.", ""},
		{"a b@example.org", ""},
		{" a@example.org", ""},
		{"a@example.org ", ""},
		{"a\tb@example.org", ""},
		{"a@example.org\r\nBcc: x@y.z", ""},
		{"a@example.org\n", ""},
		{"a\r@example.org", ""},
		{"a\x00@example.org", ""},
		{"a\x7f@example.org", ""},
		{"a\u0085@example.org", ""},
		{"a @example.org", ""},
		{"a<b>@example.org", ""},
		{"a,b@example.org", ""},
		{"\"a\"@example.org", ""},
		{long, ""},
	}
	for _, tc := range cases {
		got, err := ValidateEmail(tc.in)
		if tc.want == "" {
			if err == nil {
				t.Errorf("ValidateEmail(%q) accepted as %q", tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("ValidateEmail(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

func TestEmailHashIgnoresCase(t *testing.T) {
	if HashEmail("A@B.co") != HashEmail("a@b.co") {
		t.Fatal("hash differs by case")
	}
	if HashEmail("a@b.co") == HashEmail("c@b.co") {
		t.Fatal("hash collides")
	}
}

func TestNewSecret(t *testing.T) {
	a, b := NewSecret(), NewSecret()
	if a == b || len(a) < 22 { // 16 bytes would be 22 base64url characters
		t.Fatalf("secrets %q %q", a, b)
	}
	if len(a) < 43 { // 32 bytes
		t.Fatalf("secret too short: %d", len(a))
	}
	if HashSecret(a) == a || len(HashSecret(a)) != 64 {
		t.Fatal("hash is not sha256 hex")
	}
}
