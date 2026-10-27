package config

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func vapidPair(t *testing.T) (pub, priv string) {
	t.Helper()
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()), base64.RawURLEncoding.EncodeToString(k.Bytes())
}

func TestPushDefaultsFromNomad(t *testing.T) {
	c, err := Load(env(nomadEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if c.Push.JWKSURL != "https://nomad.sanholo.space:4646/.well-known/jwks.json" || c.Push.VAPIDEnabled() || c.Push.JWTIssuer != "" {
		t.Fatalf("push = %+v", c.Push)
	}
	// No Nomad, no JWKS: the alert endpoint stays off.
	c, err = Load(env(map[string]string{"DATABASE_URL": "postgres://u:p@h/d"}))
	if err != nil || c.Push.JWKSURL != "" {
		t.Fatalf("%v %+v", err, c.Push)
	}
}

func TestVAPIDOneSourceForServerAndJob(t *testing.T) {
	pub, priv := vapidPair(t)
	c, err := Load(env(nomadEnv("VAPID_PUBLIC_KEY", pub, "VAPID_PRIVATE_KEY", priv, "VAPID_SUBJECT", "mailto:ops@tydewhatmay.eu",
		"NOMAD_JWKS_URL", "https://nomad.internal.example:4646/.well-known/jwks.json", "NOMAD_JWT_ISSUER", "https://nomad.example")))
	if err != nil {
		t.Fatal(err)
	}
	if c.Push.VAPIDPublic != pub || c.Nomad.Job.VAPIDPublicKey != pub || !c.Push.VAPIDEnabled() {
		t.Fatalf("job key %q, push key %q, want %q in both", c.Nomad.Job.VAPIDPublicKey, c.Push.VAPIDPublic, pub)
	}
	if c.Push.JWKSURL != "https://nomad.internal.example:4646/.well-known/jwks.json" || c.Push.JWTIssuer != "https://nomad.example" {
		t.Fatalf("push = %+v", c.Push)
	}
	// The private key from a file.
	f := filepath.Join(t.TempDir(), "vapid")
	if err := os.WriteFile(f, []byte(priv+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err = Load(env(nomadEnv("VAPID_PUBLIC_KEY", pub, "VAPID_PRIVATE_KEY_FILE", f, "VAPID_SUBJECT", "https://tydewhatmay.eu/contact")))
	if err != nil || c.Push.VAPIDPrivate != priv {
		t.Fatalf("file: %v", err)
	}
}

func TestPushSettingsRefusals(t *testing.T) {
	pub, priv := vapidPair(t)
	otherPub, _ := vapidPair(t)
	f := filepath.Join(t.TempDir(), "vapid")
	_ = os.WriteFile(f, []byte(priv), 0o600)
	good := []string{"VAPID_PUBLIC_KEY", pub, "VAPID_PRIVATE_KEY", priv, "VAPID_SUBJECT", "mailto:a@b.eu"}
	cases := map[string][]string{
		"only the public key":     {"VAPID_PUBLIC_KEY", pub},
		"no subject":              {"VAPID_PUBLIC_KEY", pub, "VAPID_PRIVATE_KEY", priv},
		"public of another pair":  {"VAPID_PUBLIC_KEY", otherPub, "VAPID_PRIVATE_KEY", priv, "VAPID_SUBJECT", "mailto:a@b.eu"},
		"key and file":            append(append([]string{}, good...), "VAPID_PRIVATE_KEY_FILE", f),
		"missing file":            {"VAPID_PUBLIC_KEY", pub, "VAPID_PRIVATE_KEY_FILE", "/nonexistent/vapid", "VAPID_SUBJECT", "mailto:a@b.eu"},
		"subject without scheme":  {"VAPID_PUBLIC_KEY", pub, "VAPID_PRIVATE_KEY", priv, "VAPID_SUBJECT", "a@b.eu"},
		"subject http":            {"VAPID_PUBLIC_KEY", pub, "VAPID_PRIVATE_KEY", priv, "VAPID_SUBJECT", "http://x"},
		"short private":           {"VAPID_PUBLIC_KEY", pub, "VAPID_PRIVATE_KEY", "AAAA", "VAPID_SUBJECT", "mailto:a@b.eu"},
		"public not a point":      {"VAPID_PUBLIC_KEY", base64.RawURLEncoding.EncodeToString(append([]byte{4}, make([]byte, 64)...)), "VAPID_PRIVATE_KEY", priv, "VAPID_SUBJECT", "mailto:a@b.eu"},
		"jwks not a url":          {"NOMAD_JWKS_URL", "nomad"},
		"jwks plain http remote":  {"NOMAD_JWKS_URL", "http://nomad.example/.well-known/jwks.json"},
		"jwks without path":       {"NOMAD_JWKS_URL", "https://nomad.example"},
		"issuer with white space": {"NOMAD_JWT_ISSUER", "a b"},
	}
	for name, kv := range cases {
		_, err := Load(env(nomadEnv(kv...)))
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		for _, secret := range []string{priv} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%s: the error carries the private key", name)
			}
		}
	}
}
