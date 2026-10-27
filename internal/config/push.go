package config

import (
	"crypto/ecdh"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/lhaig/sanholo-cloud/internal/nomad"
)

// Push holds the Web Push and alert-intake settings (US-016). VAPIDPrivate is
// a secret: never log or print a Config.
type Push struct {
	// JWKSURL is where Nomad's signing keys are fetched to verify the bots'
	// workload identity tokens. Empty turns /internal/alerts off.
	JWKSURL string
	// JWTIssuer, when set, must equal the tokens' iss claim. Nomad omits iss
	// unless its server sets oidc_issuer, so it is optional.
	JWTIssuer string
	// VAPIDPublic is the one source of the public key: the control plane
	// signs with its private half and each tenant job receives this value.
	VAPIDPublic  string
	VAPIDPrivate string
	// VAPIDSubject is the contact in the VAPID JWT (mailto: or https:).
	VAPIDSubject string
}

// VAPIDEnabled reports whether Web Push can be sent.
func (p Push) VAPIDEnabled() bool { return p.VAPIDPrivate != "" }

// pushSettings reads NOMAD_JWKS_URL, NOMAD_JWT_ISSUER, VAPID_PUBLIC_KEY,
// VAPID_PRIVATE_KEY or VAPID_PRIVATE_KEY_FILE and VAPID_SUBJECT. The VAPID
// values are all or nothing, and the public key must be the private key's.
// Errors never include a key.
func pushSettings(getenv func(string) string, nomadAddr string) (Push, error) {
	p := Push{
		JWTIssuer: getenv("NOMAD_JWT_ISSUER"), VAPIDPublic: getenv("VAPID_PUBLIC_KEY"),
		VAPIDPrivate: getenv("VAPID_PRIVATE_KEY"), VAPIDSubject: getenv("VAPID_SUBJECT"),
	}
	var errs []error
	switch v := getenv("NOMAD_JWKS_URL"); {
	case v != "":
		if err := checkJWKSURL(v); err != nil {
			errs = append(errs, fmt.Errorf("NOMAD_JWKS_URL: %w", err))
		} else {
			p.JWKSURL = v
		}
	case nomadAddr != "":
		p.JWKSURL = nomadAddr + "/.well-known/jwks.json"
	}
	if strings.ContainsAny(p.JWTIssuer, " \t\r\n") {
		errs = append(errs, errors.New("NOMAD_JWT_ISSUER: contains white space"))
	}

	file := getenv("VAPID_PRIVATE_KEY_FILE")
	if p.VAPIDPrivate != "" && file != "" {
		errs = append(errs, errors.New("set only one of VAPID_PRIVATE_KEY and VAPID_PRIVATE_KEY_FILE"))
	} else if file != "" {
		b, err := os.ReadFile(filepath.Clean(file))
		if err != nil {
			errs = append(errs, errors.New("VAPID_PRIVATE_KEY_FILE: cannot be read"))
		}
		p.VAPIDPrivate = strings.TrimSpace(string(b))
	}
	any := p.VAPIDPublic != "" || p.VAPIDPrivate != "" || p.VAPIDSubject != "" || file != ""
	if !any {
		return p, errors.Join(errs...)
	}
	if p.VAPIDPublic == "" || p.VAPIDPrivate == "" || p.VAPIDSubject == "" {
		return p, errors.Join(append(errs, errors.New("VAPID_PUBLIC_KEY, VAPID_PRIVATE_KEY (or _FILE) and VAPID_SUBJECT go together: set all or none"))...)
	}
	if !strings.HasPrefix(p.VAPIDSubject, "mailto:") && !strings.HasPrefix(p.VAPIDSubject, "https://") || strings.ContainsAny(p.VAPIDSubject, " \r\n") {
		errs = append(errs, errors.New("VAPID_SUBJECT: want mailto:address or an https URL"))
	}
	if err := checkVAPIDPair(p.VAPIDPublic, p.VAPIDPrivate); err != nil {
		errs = append(errs, err)
	}
	return p, errors.Join(errs...)
}

func checkJWKSURL(v string) error {
	u, err := url.Parse(v)
	if err != nil || u.Host == "" || u.Path == "" || u.Path == "/" || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("want the full URL of Nomad's JWKS, such as https://nomad.example.org:4646/.well-known/jwks.json")
	}
	if _, err := nomad.ParseAddr(u.Scheme + "://" + u.Host); err != nil {
		return err
	}
	return nil
}

func checkVAPIDPair(public, private string) error {
	pub, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(public, "="))
	if err != nil || len(pub) != 65 {
		return errors.New("VAPID_PUBLIC_KEY: want a base64url P-256 public key (65 bytes, uncompressed)")
	}
	if _, err := ecdh.P256().NewPublicKey(pub); err != nil {
		return errors.New("VAPID_PUBLIC_KEY: not a point on P-256")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(private, "="))
	if err != nil || len(raw) != 32 {
		return errors.New("VAPID private key: want a base64url 32-byte P-256 scalar")
	}
	k, err := ecdh.P256().NewPrivateKey(raw)
	if err != nil {
		return errors.New("VAPID private key: not a valid P-256 scalar")
	}
	if subtle.ConstantTimeCompare(k.PublicKey().Bytes(), pub) != 1 {
		return errors.New("VAPID_PUBLIC_KEY is not the public half of the VAPID private key")
	}
	return nil
}
