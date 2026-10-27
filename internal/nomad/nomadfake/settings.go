package nomadfake

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"

	"github.com/lhaig/sanholo-cloud/internal/nomad"
)

// Settings returns valid, deterministic job settings for tests.
func Settings() nomad.JobSettings {
	return nomad.JobSettings{
		Namespace:       "sanholo",
		Datacenters:     []string{"dc1"},
		ImageRepo:       nomad.DefaultImageRepo,
		HostNetwork:     "private",
		QMAddr:          "https://qm.example.org:8200",
		AlertURL:        "https://app.example.org/internal/alerts",
		VAPIDPublicKey:  VAPIDKey(),
		ProxyPublicKeys: ProxyKeyPEM(1),
		TenantDomain:    "sanholo.space",
		MemoryMB:        384,
		CPUMHz:          300,
	}
}

// ProxyKeyPEM is the PEM public key of a deterministic Ed25519 key; n picks
// which one.
func ProxyKeyPEM(n byte) string {
	seed := make([]byte, ed25519.SeedSize)
	seed[0] = n
	pub := ed25519.NewKeyFromSeed(seed).Public()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		panic(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

// VAPIDKey is a well-formed (65 bytes, 0x04 first) base64url key.
func VAPIDKey() string {
	b := make([]byte, 65)
	b[0] = 4
	for i := 1; i < len(b); i++ {
		b[i] = byte(i)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
