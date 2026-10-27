package workload_test

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/lhaig/sanholo-cloud/internal/workload"
)

var t0 = time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)

type clock struct{ t atomic.Int64 }

func (c *clock) now() time.Time          { return time.Unix(0, c.t.Load()).UTC() }
func (c *clock) set(t time.Time)         { c.t.Store(t.UnixNano()) }
func (c *clock) advance(d time.Duration) { c.t.Add(int64(d)) }

func newClock() *clock { c := &clock{}; c.set(t0); return c }

// jwks serves a key set and counts fetches.
type jwks struct {
	srv  *httptest.Server
	body atomic.Value // []byte
	hits atomic.Int32
	fail atomic.Bool
}

func newJWKS(t *testing.T, body []byte) *jwks {
	t.Helper()
	j := &jwks{}
	j.body.Store(body)
	j.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		j.hits.Add(1)
		if j.fail.Load() {
			http.Error(w, "down", 500)
			return
		}
		_, _ = w.Write(j.body.Load().([]byte))
	}))
	t.Cleanup(j.srv.Close)
	return j
}

func rsaJWKS(kid string, k *rsa.PublicKey) []byte {
	b, _ := json.Marshal(map[string]any{"keys": []map[string]string{{
		"use": "sig", "kty": "RSA", "kid": kid, "alg": "RS256",
		"n": base64.RawURLEncoding.EncodeToString(k.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(k.E)).Bytes()),
	}}})
	return b
}

func edJWKS(kid string, k ed25519.PublicKey) []byte {
	b, _ := json.Marshal(map[string]any{"keys": []map[string]string{{
		"use": "sig", "kty": "OKP", "crv": "Ed25519", "kid": kid, "x": base64.RawURLEncoding.EncodeToString(k),
	}}})
	return b
}

func claims(mut func(m jwt.MapClaims)) jwt.MapClaims {
	m := jwt.MapClaims{
		"aud": "sanholo.cloud", "iat": t0.Add(-time.Minute).Unix(), "nbf": t0.Add(-time.Minute).Unix(),
		"exp": t0.Add(time.Hour).Unix(), "nomad_namespace": "sanholo",
		"nomad_job_id": "sanholo-tenant-acme-42", "nomad_allocation_id": "alloc-1",
	}
	if mut != nil {
		mut(m)
	}
	return m
}

func sign(t *testing.T, m jwt.SigningMethod, kid string, key crypto.PrivateKey, c jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(m, c)
	tok.Header["kid"] = kid
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func rsaKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func verifier(t *testing.T, j *jwks, c *clock, mut func(*workload.Config)) *workload.Verifier {
	t.Helper()
	cfg := workload.Config{JWKSURL: j.srv.URL, Namespace: "sanholo", Now: c.now}
	if mut != nil {
		mut(&cfg)
	}
	v, err := workload.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// A token minted by a real Nomad 2.0.6 dev agent (captured from a docker task
// with identity { name = "sanholo_cloud", aud = ["sanholo.cloud"] }), checked
// against the JWKS the same agent served.
func TestRealNomadToken(t *testing.T) {
	tok, err := os.ReadFile("testdata/nomad-2.0.6.jwt")
	if err != nil {
		t.Fatal(err)
	}
	set, err := os.ReadFile("testdata/nomad-2.0.6.jwks.json")
	if err != nil {
		t.Fatal(err)
	}
	j := newJWKS(t, set)
	c := newClock()
	c.set(time.Unix(1793094561, 0).Add(time.Minute))
	v := verifier(t, j, c, nil)
	got, err := v.Verify(context.Background(), strings.TrimSpace(string(tok)))
	if err != nil {
		t.Fatalf("real Nomad token refused: %v", err)
	}
	if got.TenantID != "acme-42" || got.JobID != "sanholo-tenant-acme-42" || got.Namespace != "sanholo" {
		t.Fatalf("claims = %+v", got)
	}
	// After exp it is refused.
	c.set(time.Unix(1793098161, 0).Add(time.Minute))
	if _, err := v.Verify(context.Background(), strings.TrimSpace(string(tok))); !errors.Is(err, workload.ErrInvalid) {
		t.Fatalf("expired real token: %v", err)
	}
}

func TestVerify(t *testing.T) {
	good := rsaKey(t)
	other := rsaKey(t)
	edPub, edPriv, _ := ed25519.GenerateKey(rand.Reader)
	rs := jwt.SigningMethodRS256

	cases := []struct {
		name  string
		token func() string
		cfg   func(*workload.Config)
		want  string // tenant id, or "" for refused
	}{
		{"valid RS256", func() string { return sign(t, rs, "k1", good, claims(nil)) }, nil, "acme-42"},
		{"wrong signing key", func() string { return sign(t, rs, "k1", other, claims(nil)) }, nil, ""},
		{"wrong audience", func() string {
			return sign(t, rs, "k1", good, claims(func(m jwt.MapClaims) { m["aud"] = "qm.sanholo" }))
		}, nil, ""},
		{"two audiences", func() string {
			return sign(t, rs, "k1", good, claims(func(m jwt.MapClaims) { m["aud"] = []string{"sanholo.cloud", "qm.sanholo"} }))
		}, nil, ""},
		{"expired", func() string {
			return sign(t, rs, "k1", good, claims(func(m jwt.MapClaims) { m["exp"] = t0.Add(-time.Minute).Unix() }))
		}, nil, ""},
		{"expired within skew", func() string {
			return sign(t, rs, "k1", good, claims(func(m jwt.MapClaims) { m["exp"] = t0.Add(-10 * time.Second).Unix() }))
		}, nil, "acme-42"},
		{"not yet valid", func() string {
			return sign(t, rs, "k1", good, claims(func(m jwt.MapClaims) { m["nbf"] = t0.Add(5 * time.Minute).Unix() }))
		}, nil, ""},
		{"issued in the future", func() string {
			return sign(t, rs, "k1", good, claims(func(m jwt.MapClaims) { m["iat"] = t0.Add(5 * time.Minute).Unix() }))
		}, nil, ""},
		{"no exp", func() string { return sign(t, rs, "k1", good, claims(func(m jwt.MapClaims) { delete(m, "exp") })) }, nil, ""},
		{"no nbf", func() string { return sign(t, rs, "k1", good, claims(func(m jwt.MapClaims) { delete(m, "nbf") })) }, nil, ""},
		{"no iat", func() string { return sign(t, rs, "k1", good, claims(func(m jwt.MapClaims) { delete(m, "iat") })) }, nil, ""},
		{"other namespace", func() string {
			return sign(t, rs, "k1", good, claims(func(m jwt.MapClaims) { m["nomad_namespace"] = "default" }))
		}, nil, ""},
		{"job id not a tenant", func() string {
			return sign(t, rs, "k1", good, claims(func(m jwt.MapClaims) { m["nomad_job_id"] = "billing-batch" }))
		}, nil, ""},
		{"job id prefix only", func() string {
			return sign(t, rs, "k1", good, claims(func(m jwt.MapClaims) { m["nomad_job_id"] = "sanholo-tenant-" }))
		}, nil, ""},
		{"job id with a path", func() string {
			return sign(t, rs, "k1", good, claims(func(m jwt.MapClaims) { m["nomad_job_id"] = "sanholo-tenant-a/../b" }))
		}, nil, ""},
		{"job id upper case", func() string {
			return sign(t, rs, "k1", good, claims(func(m jwt.MapClaims) { m["nomad_job_id"] = "sanholo-tenant-ACME" }))
		}, nil, ""},
		{"issuer mismatch", func() string {
			return sign(t, rs, "k1", good, claims(func(m jwt.MapClaims) { m["iss"] = "https://evil" }))
		},
			func(c *workload.Config) { c.Issuer = "https://nomad.example" }, ""},
		{"issuer match", func() string {
			return sign(t, rs, "k1", good, claims(func(m jwt.MapClaims) { m["iss"] = "https://nomad.example" }))
		},
			func(c *workload.Config) { c.Issuer = "https://nomad.example" }, "acme-42"},
		{"issuer required when set", func() string { return sign(t, rs, "k1", good, claims(nil)) },
			func(c *workload.Config) { c.Issuer = "https://nomad.example" }, ""},
		{"alg none", func() string {
			s, _ := jwt.NewWithClaims(jwt.SigningMethodNone, claims(nil)).SignedString(jwt.UnsafeAllowNoneSignatureType)
			return s
		}, nil, ""},
		{"HS256 keyed with the public key", func() string {
			return sign(t, jwt.SigningMethodHS256, "k1", good.N.Bytes(), claims(nil))
		}, nil, ""},
		{"unknown kid", func() string { return sign(t, rs, "nope", good, claims(nil)) }, nil, ""},
		{"no kid", func() string { return sign(t, rs, "", good, claims(nil)) }, nil, ""},
		{"garbage", func() string { return "a.b.c" }, nil, ""},
		{"empty", func() string { return "" }, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			j := newJWKS(t, rsaJWKS("k1", &good.PublicKey))
			v := verifier(t, j, newClock(), tc.cfg)
			got, err := v.Verify(context.Background(), tc.token())
			if tc.want == "" {
				if !errors.Is(err, workload.ErrInvalid) {
					t.Fatalf("err = %v (claims %+v), want ErrInvalid", err, got)
				}
				return
			}
			if err != nil || got.TenantID != tc.want {
				t.Fatalf("got %+v, %v; want tenant %s", got, err, tc.want)
			}
		})
	}

	t.Run("valid EdDSA", func(t *testing.T) {
		j := newJWKS(t, edJWKS("e1", edPub))
		v := verifier(t, j, newClock(), nil)
		got, err := v.Verify(context.Background(), sign(t, jwt.SigningMethodEdDSA, "e1", edPriv, claims(nil)))
		if err != nil || got.TenantID != "acme-42" {
			t.Fatalf("got %+v, %v", got, err)
		}
	})
	t.Run("key type and algorithm must agree", func(t *testing.T) {
		// An RSA JWKS entry cannot validate an EdDSA-signed token.
		j := newJWKS(t, rsaJWKS("k1", &good.PublicKey))
		v := verifier(t, j, newClock(), nil)
		if _, err := v.Verify(context.Background(), sign(t, jwt.SigningMethodEdDSA, "k1", edPriv, claims(nil))); !errors.Is(err, workload.ErrInvalid) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestJWKSCaching(t *testing.T) {
	k1, k2 := rsaKey(t), rsaKey(t)
	j := newJWKS(t, rsaJWKS("k1", &k1.PublicKey))
	c := newClock()
	v := verifier(t, j, c, nil)
	ctx := context.Background()
	ok := func(kid string, k *rsa.PrivateKey) error {
		_, err := v.Verify(ctx, sign(t, jwt.SigningMethodRS256, kid, k, claims(nil)))
		return err
	}
	for range 5 {
		if err := ok("k1", k1); err != nil {
			t.Fatal(err)
		}
	}
	if j.hits.Load() != 1 {
		t.Fatalf("fetches = %d, want 1 (cached)", j.hits.Load())
	}

	// A flood of tokens with unknown key ids refreshes at most once per MinRefresh.
	for i := range 20 {
		_ = ok("forged-"+string(rune('a'+i)), k2)
	}
	if got := j.hits.Load(); got > 2 {
		t.Fatalf("fetches after unknown-kid flood = %d, want at most 2", got)
	}

	// Rotation: the new kid is picked up once MinRefresh has passed.
	j.body.Store(rsaJWKS("k2", &k2.PublicKey))
	if err := ok("k2", k2); err == nil {
		t.Fatal("a rotated key was accepted inside the refresh back-off")
	}
	c.advance(31 * time.Second)
	if err := ok("k2", k2); err != nil {
		t.Fatalf("rotated key after back-off: %v", err)
	}
	// The retired key is gone from the set.
	if err := ok("k1", k1); err == nil {
		t.Fatal("a key that left the JWKS is still accepted")
	}
}

func TestJWKSOutage(t *testing.T) {
	k := rsaKey(t)
	tok := func() string { return sign(t, jwt.SigningMethodRS256, "k1", k, claims(nil)) }

	t.Run("never reachable", func(t *testing.T) {
		j := newJWKS(t, nil)
		j.fail.Store(true)
		v := verifier(t, j, newClock(), nil)
		if _, err := v.Verify(context.Background(), tok()); !errors.Is(err, workload.ErrUnavailable) {
			t.Fatalf("err = %v, want ErrUnavailable", err)
		}
	})
	t.Run("stale keys serve through a short outage, not a long one", func(t *testing.T) {
		j := newJWKS(t, rsaJWKS("k1", &k.PublicKey))
		c := newClock()
		v := verifier(t, j, c, nil)
		if _, err := v.Verify(context.Background(), tok()); err != nil {
			t.Fatal(err)
		}
		j.fail.Store(true)
		c.advance(20 * time.Minute) // past MaxAge, within StaleMax; the token is still inside its skewed window
		fresh := func() string {
			return sign(t, jwt.SigningMethodRS256, "k1", k, claims(func(m jwt.MapClaims) {
				m["exp"] = c.now().Add(time.Hour).Unix()
				m["nbf"] = c.now().Add(-time.Minute).Unix()
				m["iat"] = c.now().Add(-time.Minute).Unix()
			}))
		}
		if _, err := v.Verify(context.Background(), fresh()); err != nil {
			t.Fatalf("stale keys during an outage: %v", err)
		}
		c.advance(2 * time.Hour)
		if _, err := v.Verify(context.Background(), fresh()); !errors.Is(err, workload.ErrUnavailable) {
			t.Fatalf("keys older than StaleMax: %v", err)
		}
	})
}

func TestParseJWKSIgnoresWeakAndUnknownKeys(t *testing.T) {
	small, _ := rsa.GenerateKey(rand.Reader, 1024)
	if _, err := workload.ParseJWKS(rsaJWKS("s", &small.PublicKey)); err == nil {
		t.Fatal("a 1024-bit RSA key was accepted")
	}
	if _, err := workload.ParseJWKS([]byte(`{"keys":[{"kty":"oct","kid":"x","k":"AAAA"}]}`)); err == nil {
		t.Fatal("a symmetric key was accepted")
	}
}
