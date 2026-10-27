// Package identity signs the per-request identity the proxy attaches to every
// request it forwards to a tenant's Bridge (header X-SanHolo-Identity), and
// handles the proxy's Ed25519 key.
//
// The wire format is the SanHolo client's, defined in
// san-holo/internal/identity/identity.go (Sign, Verify, domain,
// MaxAhead); the client's code wins over any note:
//
//	token     = payload "." signature
//	payload   = base64url(JSON {"tenant":"...","aud":"...","exp":<unix seconds>}), no padding
//	signature = base64url(Ed25519(priv, "sanholo-identity/v1." + payload)), no padding
//
// The signed message is the payload exactly as transmitted. The instance
// accepts a token only when tenant and aud both equal its own tenant id and
// exp is in the future and at most 5 minutes ahead.
package identity

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Header is the request header carrying the token.
const Header = "X-SanHolo-Identity"

// TTL is how far ahead the proxy sets exp. The client allows at most five
// minutes; two leaves room for a client clock three minutes behind this
// one, and a clock two minutes ahead still accepts the token.
const TTL = 2 * time.Minute

// MaxKeys is how many public keys an instance accepts at once.
const MaxKeys = 2

// domain separates this key's signatures from any other use of the key.
const domain = "sanholo-identity/v1."

type claims struct {
	Tenant string `json:"tenant"`
	Aud    string `json:"aud"`
	Exp    int64  `json:"exp"`
}

// Sign returns the token for tenant, valid until now+TTL. The audience is
// the tenant: the instance accepts a token only when both equal its own id.
func Sign(priv ed25519.PrivateKey, tenant string, now time.Time) (string, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return "", errors.New("identity: no signing key")
	}
	if tenant == "" {
		return "", errors.New("identity: empty tenant")
	}
	raw, err := json.Marshal(claims{Tenant: tenant, Aud: tenant, Exp: now.Add(TTL).Unix()})
	if err != nil {
		return "", fmt.Errorf("identity: encode claims: %w", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	sig := ed25519.Sign(priv, []byte(domain+payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// ParseSigningKey reads the proxy's private key: a PKCS#8 "PRIVATE KEY" PEM
// block, or base64 (standard or URL alphabet, padded or not) of the 32-byte
// seed or the 64-byte Ed25519 private key. Errors never include the input.
func ParseSigningKey(data []byte) (ed25519.PrivateKey, error) {
	text := strings.TrimSpace(string(data))
	if text == "" {
		return nil, errors.New("signing key is empty")
	}
	if strings.Contains(text, "-----BEGIN") {
		blk, rest := pem.Decode([]byte(text))
		if blk == nil || blk.Type != "PRIVATE KEY" || strings.TrimSpace(string(rest)) != "" {
			return nil, errors.New("signing key: want one PKCS#8 PRIVATE KEY PEM block")
		}
		k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
		if err != nil {
			return nil, errors.New("signing key: cannot parse the PKCS#8 block")
		}
		priv, ok := k.(ed25519.PrivateKey)
		if !ok {
			return nil, errors.New("signing key is not Ed25519")
		}
		return priv, nil
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		b, err := enc.DecodeString(text)
		if err != nil {
			continue
		}
		switch len(b) {
		case ed25519.SeedSize:
			return ed25519.NewKeyFromSeed(b), nil
		case ed25519.PrivateKeySize:
			priv := ed25519.PrivateKey(b)
			// A 64-byte key whose second half is not the public key of the
			// first would sign with a key nobody can verify.
			if !priv.Public().(ed25519.PublicKey).Equal(ed25519.NewKeyFromSeed(b[:ed25519.SeedSize]).Public()) {
				return nil, errors.New("signing key: the 64 bytes are not a consistent Ed25519 key")
			}
			return priv, nil
		}
	}
	return nil, fmt.Errorf("signing key: want a PKCS#8 PEM, or base64 of a %d-byte seed", ed25519.SeedSize)
}

// PublicKeyPEM is the PKIX "PUBLIC KEY" PEM of priv's public key: the form
// the tenant job carries (proxy-pubkey.pem).
func PublicKeyPEM(pub ed25519.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", fmt.Errorf("identity: encode public key: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), nil
}

// ParsePublicKeys reads public keys the way the client does (san-holo
// internal/identity ParsePublicKeys): PEM blocks, or one base64 raw 32-byte
// key per line with blank lines and # comments skipped.
func ParsePublicKeys(data []byte) ([]ed25519.PublicKey, error) {
	var keys []ed25519.PublicKey
	if strings.Contains(string(data), "-----BEGIN") {
		rest := data
		for {
			var blk *pem.Block
			blk, rest = pem.Decode(rest)
			if blk == nil {
				break
			}
			if blk.Type != "PUBLIC KEY" {
				return nil, errors.New("public key PEM block: want PUBLIC KEY")
			}
			pub, err := x509.ParsePKIXPublicKey(blk.Bytes)
			if err != nil {
				return nil, errors.New("public key: cannot parse")
			}
			k, ok := pub.(ed25519.PublicKey)
			if !ok {
				return nil, errors.New("public key is not Ed25519")
			}
			keys = append(keys, k)
		}
		if strings.TrimSpace(string(rest)) != "" {
			return nil, errors.New("public keys: unparsable data after the last PEM block")
		}
	} else {
		for line := range strings.SplitSeq(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			k, err := parseRawKey(line)
			if err != nil {
				return nil, err
			}
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil, errors.New("no public key found")
	}
	return keys, nil
}

func parseRawKey(s string) (ed25519.PublicKey, error) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			if len(b) != ed25519.PublicKeySize {
				return nil, fmt.Errorf("public key is %d bytes, want %d", len(b), ed25519.PublicKeySize)
			}
			return ed25519.PublicKey(b), nil
		}
	}
	return nil, errors.New("public key is not base64")
}

// TenantKeyFile is what the tenant job carries: the PEM of the key the proxy
// signs with, then any extra keys (the next key during a rotation), each as
// its own PEM block. At most MaxKeys keys in all, duplicates dropped. The
// signing key's public half is always first, so the job can never carry a
// key set that excludes the key the proxy actually signs with.
func TenantKeyFile(priv ed25519.PrivateKey, extra ...ed25519.PublicKey) (string, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return "", errors.New("identity: no signing key")
	}
	keys := []ed25519.PublicKey{priv.Public().(ed25519.PublicKey)}
	for _, k := range extra {
		dup := false
		for _, have := range keys {
			dup = dup || have.Equal(k)
		}
		if !dup {
			keys = append(keys, k)
		}
	}
	if len(keys) > MaxKeys {
		return "", fmt.Errorf("identity: %d distinct public keys; at most %d are accepted", len(keys), MaxKeys)
	}
	var b strings.Builder
	for _, k := range keys {
		p, err := PublicKeyPEM(k)
		if err != nil {
			return "", err
		}
		b.WriteString(p)
	}
	return b.String(), nil
}
