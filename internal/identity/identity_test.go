package identity

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lhaig/sanholo-cloud/internal/identity/clientfake"
)

var epoch = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

func keyFromByte(b byte) ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed([]byte(strings.Repeat(string(rune('a'+b)), 32)))
}

func clientFor(tenant string, now time.Time, keys ...ed25519.PublicKey) *clientfake.Verifier {
	return clientfake.New(keys, tenant, func() time.Time { return now })
}

// The token the proxy produces verifies with the client's own algorithm.
func TestSignVerifiesWithTheClientsAlgorithm(t *testing.T) {
	priv := keyFromByte(0)
	pub := priv.Public().(ed25519.PublicKey)
	tok, err := Sign(priv, "acme", epoch)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := clientFor("acme", epoch, pub).Verify(tok); v != clientfake.OK {
		t.Fatalf("client verdict %d: %v", v, err)
	}
	// exp is no later than the client's bound at the moment of signing.
	if v, err := clientFor("acme", epoch.Add(TTL-time.Second), pub).Verify(tok); v != clientfake.OK {
		t.Errorf("just before expiry: %d %v", v, err)
	}
	for name, tc := range map[string]struct {
		v    *clientfake.Verifier
		want clientfake.Verdict
	}{
		"other tenant":         {clientFor("other", epoch, pub), clientfake.Forbidden},
		"at expiry":            {clientFor("acme", epoch.Add(TTL), pub), clientfake.Unauthorized},
		"other key":            {clientFor("acme", epoch, keyFromByte(1).Public().(ed25519.PublicKey)), clientfake.Unauthorized},
		"rotation, second key": {clientFor("acme", epoch, keyFromByte(1).Public().(ed25519.PublicKey), pub), clientfake.OK},
	} {
		if got, _ := tc.v.Verify(tok); got != tc.want {
			t.Errorf("%s: verdict %d, want %d", name, got, tc.want)
		}
	}
}

// The wire format byte for byte: the payload is base64url(no padding) of
// exactly {"tenant","aud","exp"} in that order, and the signature covers
// "sanholo-identity/v1." followed by the payload as transmitted.
func TestSignWireFormat(t *testing.T) {
	priv := keyFromByte(2)
	tok, err := Sign(priv, "acme", epoch)
	if err != nil {
		t.Fatal(err)
	}
	payload, sigPart, ok := strings.Cut(tok, ".")
	if !ok || strings.ContainsAny(tok, "=+/") {
		t.Fatalf("token %q: want payload.signature, base64url without padding", tok)
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"tenant":"acme","aud":"acme","exp":` + strconv.FormatInt(epoch.Add(TTL).Unix(), 10) + `}`
	if string(raw) != want {
		t.Errorf("claims = %s, want %s", raw, want)
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigPart)
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(priv.Public().(ed25519.PublicKey), []byte("sanholo-identity/v1."+payload), sig) {
		t.Error("signature is not over \"sanholo-identity/v1.\" + payload")
	}
	if ed25519.Verify(priv.Public().(ed25519.PublicKey), []byte(payload), sig) {
		t.Error("signature also verifies without the domain prefix")
	}
	if TTL > 5*time.Minute || TTL <= 0 {
		t.Errorf("TTL %v outside the client's 5 minute bound", TTL)
	}
}

func TestSignRefusesBadInput(t *testing.T) {
	if _, err := Sign(nil, "acme", epoch); err == nil {
		t.Error("signed without a key")
	}
	if _, err := Sign(keyFromByte(0), "", epoch); err == nil {
		t.Error("signed an empty tenant")
	}
}

func pkcs8PEM(t *testing.T, priv ed25519.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func TestParseSigningKey(t *testing.T) {
	priv := keyFromByte(3)
	seed := priv.Seed()
	bad64 := append([]byte{}, priv...)
	bad64[40] ^= 1
	tests := []struct {
		name string
		in   string
		ok   bool
	}{
		{"pkcs8 pem", pkcs8PEM(t, priv), true},
		{"pem with trailing newline", pkcs8PEM(t, priv) + "\n\n", true},
		{"seed std", base64.StdEncoding.EncodeToString(seed), true},
		{"seed raw url", base64.RawURLEncoding.EncodeToString(seed) + "\n", true},
		{"64 byte key", base64.StdEncoding.EncodeToString(priv), true},
		{"64 byte key with wrong public half", base64.StdEncoding.EncodeToString(bad64), false},
		{"31 bytes", base64.StdEncoding.EncodeToString(seed[:31]), false},
		{"empty", "  \n", false},
		{"garbage", "not a key!", false},
		{"public key pem", "-----BEGIN PUBLIC KEY-----\nAAAA\n-----END PUBLIC KEY-----\n", false},
	}
	for _, tc := range tests {
		got, err := ParseSigningKey([]byte(tc.in))
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", tc.name, err, tc.ok)
			continue
		}
		if tc.ok && !got.Equal(priv) {
			t.Errorf("%s: parsed a different key", tc.name)
		}
		if err != nil && strings.Contains(err.Error(), tc.in) && len(strings.TrimSpace(tc.in)) > 8 {
			t.Errorf("%s: the error repeats the input", tc.name)
		}
	}
}

// One source of truth: the key file the tenant job carries is derived from
// the signing key, so a token the proxy signs always verifies against it,
// including through the client's own public-key parser rules.
func TestTenantKeyFileMatchesTheSigningKey(t *testing.T) {
	priv, next := keyFromByte(4), keyFromByte(5)
	nextPub := next.Public().(ed25519.PublicKey)

	one, err := TenantKeyFile(priv)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := ParsePublicKeys([]byte(one))
	if err != nil || len(keys) != 1 || !keys[0].Equal(priv.Public()) {
		t.Fatalf("one key: %v %v", keys, err)
	}

	// Rotation: the next key is added, the signing key stays first, and a
	// token signed with either verifies.
	two, err := TenantKeyFile(priv, nextPub, nextPub, priv.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	keys, err = ParsePublicKeys([]byte(two))
	if err != nil || len(keys) != 2 || !keys[0].Equal(priv.Public()) || !keys[1].Equal(nextPub) {
		t.Fatalf("rotation keys: %v %v", len(keys), err)
	}
	for _, signer := range []ed25519.PrivateKey{priv, next} {
		tok, _ := Sign(signer, "acme", epoch)
		if v, err := clientFor("acme", epoch, keys...).Verify(tok); v != clientfake.OK {
			t.Errorf("token from a rotation key refused: %v", err)
		}
	}
	// Three distinct keys are more than an instance accepts.
	if _, err := TenantKeyFile(priv, nextPub, keyFromByte(6).Public().(ed25519.PublicKey)); err == nil {
		t.Error("three keys accepted")
	}
	if _, err := TenantKeyFile(nil); err == nil {
		t.Error("key file without a signing key")
	}
}

func TestParsePublicKeys(t *testing.T) {
	pub := keyFromByte(7).Public().(ed25519.PublicKey)
	pemText, _ := PublicKeyPEM(pub)
	raw := base64.StdEncoding.EncodeToString(pub)
	for name, in := range map[string]string{"pem": pemText, "raw": "# comment\n" + raw + "\n"} {
		keys, err := ParsePublicKeys([]byte(in))
		if err != nil || len(keys) != 1 || !keys[0].Equal(pub) {
			t.Errorf("%s: %v %v", name, keys, err)
		}
	}
	for name, in := range map[string]string{"empty": "", "short": base64.StdEncoding.EncodeToString(pub[:10]), "junk after pem": pemText + "junk"} {
		if _, err := ParsePublicKeys([]byte(in)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
