package config

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lhaig/sanholo-cloud/internal/identity"
)

// seedKey is the proxy key every test here uses: 32 bytes of 0x02.
const seedKey = "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI="

func seedPriv() ed25519.PrivateKey {
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = 2
	}
	return ed25519.NewKeyFromSeed(seed)
}

func TestProxySigningKeySources(t *testing.T) {
	dir := t.TempDir()
	der, _ := x509.MarshalPKCS8PrivateKey(seedPriv())
	pemPath := filepath.Join(dir, "proxy.pem")
	if err := os.WriteFile(pemPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	seedPath := filepath.Join(dir, "proxy.seed")
	if err := os.WriteFile(seedPath, []byte(seedKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{"seed inline", nomadEnv("PROXY_SIGNING_KEY", seedKey), ""},
		{"seed file", nomadEnv("PROXY_SIGNING_KEY_FILE", seedPath), ""},
		{"pem file", nomadEnv("PROXY_SIGNING_KEY_FILE", pemPath), ""},
		{"both sources", nomadEnv("PROXY_SIGNING_KEY", seedKey, "PROXY_SIGNING_KEY_FILE", seedPath), "only one of PROXY_SIGNING_KEY"},
		{"missing file", nomadEnv("PROXY_SIGNING_KEY_FILE", "/nonexistent/key"), "PROXY_SIGNING_KEY_FILE"},
		{"garbage", nomadEnv("PROXY_SIGNING_KEY", "definitely-not-a-key-SECRETISH"), "PROXY_SIGNING_KEY"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load(env(tc.env))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				for _, secret := range []string{"SECRETISH", seedKey} {
					if strings.Contains(err.Error(), secret) {
						t.Errorf("the error repeats the key: %v", err)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !c.ProxyKey.Equal(seedPriv()) {
				t.Error("the loaded key is not the configured one")
			}
		})
	}
}

// One source of truth: the public key in the tenant job is derived from the
// signing key, and a token the proxy signs verifies against it.
func TestTenantJobKeyIsDerivedFromTheSigningKey(t *testing.T) {
	c, err := Load(env(nomadEnv("PROXY_SIGNING_KEY", seedKey, "TENANT_PROXY_PUBLIC_KEYS", "")))
	if err != nil {
		t.Fatal(err)
	}
	keys, err := identity.ParsePublicKeys([]byte(c.Nomad.Job.ProxyPublicKeys))
	if err != nil || len(keys) != 1 || !keys[0].Equal(seedPriv().Public()) {
		t.Fatalf("job keys = %d, %v; want the signing key's public half", len(keys), err)
	}
	if !strings.Contains(c.Nomad.Job.ProxyPublicKeys, "BEGIN PUBLIC KEY") {
		t.Error("the job key is not PEM")
	}
}

// A rotation: the next public key is accepted by tenants alongside the
// current one, and the signing key's public half always stays first, however
// the extra key is written (raw base64 is re-encoded as PEM so the client's
// all-PEM-or-all-raw parser accepts the pair).
func TestRotationAddsTheNextKey(t *testing.T) {
	next := ed25519.NewKeyFromSeed([]byte(strings.Repeat("n", 32)))
	nextPub := next.Public().(ed25519.PublicKey)
	nextRaw := base64.StdEncoding.EncodeToString(nextPub)
	nextPEM, _ := identity.PublicKeyPEM(nextPub)
	for name, extra := range map[string]string{"raw": nextRaw, "pem": nextPEM} {
		c, err := Load(env(nomadEnv("PROXY_SIGNING_KEY", seedKey, "TENANT_PROXY_PUBLIC_KEYS", extra)))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		keys, err := identity.ParsePublicKeys([]byte(c.Nomad.Job.ProxyPublicKeys))
		if err != nil || len(keys) != 2 || !keys[0].Equal(seedPriv().Public()) || !keys[1].Equal(nextPub) {
			t.Errorf("%s: keys = %d, %v", name, len(keys), err)
		}
		if strings.Contains(c.Nomad.Job.ProxyPublicKeys, "AQEB") {
			t.Errorf("%s: raw text leaked into the PEM file", name)
		}
	}
	// The same key listed twice is one key.
	same, _ := identity.PublicKeyPEM(seedPriv().Public().(ed25519.PublicKey))
	c, err := Load(env(nomadEnv("PROXY_SIGNING_KEY", seedKey, "TENANT_PROXY_PUBLIC_KEYS", same)))
	if err != nil {
		t.Fatal(err)
	}
	if keys, _ := identity.ParsePublicKeys([]byte(c.Nomad.Job.ProxyPublicKeys)); len(keys) != 1 {
		t.Errorf("duplicate key gave %d keys", len(keys))
	}
	// Two extra keys on top of the signing key are too many.
	k2, _ := identity.PublicKeyPEM(ed25519.NewKeyFromSeed([]byte(strings.Repeat("m", 32))).Public().(ed25519.PublicKey))
	if _, err := Load(env(nomadEnv("PROXY_SIGNING_KEY", seedKey, "TENANT_PROXY_PUBLIC_KEYS", nextPEM+k2))); err == nil {
		t.Error("three distinct keys accepted")
	}
	if _, err := Load(env(nomadEnv("PROXY_SIGNING_KEY", seedKey, "TENANT_PROXY_PUBLIC_KEYS", "junk!"))); err == nil {
		t.Error("a garbage extra key accepted")
	}
}

func TestTenantDomain(t *testing.T) {
	c, err := Load(env(nomadEnv("PROXY_SIGNING_KEY", seedKey, "TENANT_PROXY_PUBLIC_KEYS", "")))
	if err != nil || c.TenantDomain != "sanholo.space" {
		t.Fatalf("default: %q, %v", c.TenantDomain, err)
	}
	c, err = Load(env(nomadEnv("PROXY_SIGNING_KEY", seedKey, "TENANT_PROXY_PUBLIC_KEYS", "", "TENANT_DOMAIN", "staging.example.org")))
	if err != nil || c.TenantDomain != "staging.example.org" || c.Nomad.Job.TenantDomain != "staging.example.org" {
		t.Fatalf("override: %q, %v", c.TenantDomain, err)
	}
	// Without Nomad the proxy can still run in development.
	c, err = Load(env(map[string]string{"DATABASE_URL": "postgres://u:p@h/d", "PROXY_SIGNING_KEY": seedKey, "TENANT_DOMAIN": "localhost.test"}))
	if err != nil || c.TenantDomain != "localhost.test" || c.ProxyKey == nil {
		t.Fatalf("development: %q, %v", c.TenantDomain, err)
	}
	for name, kv := range map[string]map[string]string{
		"key without a domain": {"DATABASE_URL": "postgres://u:p@h/d", "PROXY_SIGNING_KEY": seedKey},
		"bad domain":           {"DATABASE_URL": "postgres://u:p@h/d", "TENANT_DOMAIN": "Bad Domain"},
	} {
		if _, err := Load(env(kv)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// Development without a key leaves the proxy off.
	c, err = Load(env(map[string]string{"DATABASE_URL": "postgres://u:p@h/d"}))
	if err != nil || c.ProxyKey != nil || c.TenantDomain != "" {
		t.Errorf("off by default: %+v %v", c.ProxyKey != nil, err)
	}
}
