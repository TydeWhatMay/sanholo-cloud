// Package clientfake is a stand-in for a SanHolo client in hosted mode, for
// tests of the proxy. It is not used by the service.
//
// Why a fake and not the real client: the client's `serve -fixture` mode
// returns before the hosted guards are installed, so it never checks an
// identity, and `serve -hosted` needs the live game API (its base URL is
// fixed to api.replicant.space) and qm. Neither can run in a test. So this
// package carries the client's own verification code, copied verbatim, and a
// net/http port of its two request guards.
//
// Sources (the client repository, /home/stokvis/dev/TydeWhatMay/san-holo):
//
//	internal/identity/identity.go  Verifier.Verify and the constants (verbatim; only
//	                               the type names changed)
//	internal/web/hosted_guard.go   requireIdentity, csrfGuard, isHealthz (ported from
//	                               Fiber to net/http, same decisions)
//
// If the client's wire format changes, re-copy and the proxy's tests show
// the break.
package clientfake

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

const (
	// Header is identity.Header.
	Header = "X-SanHolo-Identity"

	domain      = "sanholo-identity/v1."
	maxAhead    = 5 * time.Minute
	maxTokenLen = 2048
)

type claims struct {
	Tenant string `json:"tenant"`
	Aud    string `json:"aud"`
	Exp    int64  `json:"exp"`
}

// Verdict is identity.Verdict.
type Verdict int

const (
	OK Verdict = iota
	Unauthorized
	Forbidden
)

// Verifier is identity.Verifier.
type Verifier struct {
	keys   []ed25519.PublicKey
	tenant string
	now    func() time.Time
}

// New builds a Verifier. now may be nil.
func New(keys []ed25519.PublicKey, tenant string, now func() time.Time) *Verifier {
	if now == nil {
		now = time.Now
	}
	return &Verifier{keys: keys, tenant: tenant, now: now}
}

// Verify is identity.Verifier.Verify, verbatim.
func (v *Verifier) Verify(token string) (Verdict, error) {
	if token == "" {
		return Unauthorized, errors.New("missing identity")
	}
	if len(token) > maxTokenLen {
		return Unauthorized, errors.New("identity too long")
	}
	payload, sigPart, ok := strings.Cut(token, ".")
	if !ok || payload == "" || strings.Contains(sigPart, ".") {
		return Unauthorized, errors.New("malformed identity")
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigPart)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return Unauthorized, errors.New("malformed signature")
	}
	msg := []byte(domain + payload)
	valid := false
	for _, k := range v.keys { // no early exit: same work whichever key matches
		if ed25519.Verify(k, msg, sig) {
			valid = true
		}
	}
	if !valid {
		return Unauthorized, errors.New("bad signature")
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return Unauthorized, errors.New("malformed claims")
	}
	var c claims
	if err := json.Unmarshal(raw, &c); err != nil {
		return Unauthorized, errors.New("malformed claims")
	}
	now := v.now()
	switch {
	case c.Exp <= 0:
		return Unauthorized, errors.New("identity has no expiry")
	case !now.Before(time.Unix(c.Exp, 0)):
		return Unauthorized, errors.New("identity expired")
	case time.Unix(c.Exp, 0).After(now.Add(maxAhead)):
		return Unauthorized, errors.New("identity expiry too far ahead")
	}
	if c.Aud != v.tenant || c.Tenant != v.tenant {
		return Forbidden, errors.New("identity is for another tenant")
	}
	return OK, nil
}

const (
	// CSRFCookie and CSRFHeader are hosted_guard.go's csrfCookie and csrfHeader.
	CSRFCookie = "__Host-sanholo_csrf"
	CSRFHeader = "X-CSRF-Token"
)

func newCSRFToken() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func validCSRFToken(t string) bool {
	if len(t) != 64 {
		return false
	}
	_, err := hex.DecodeString(t)
	return err == nil
}

// Guard wraps next in the client's hosted guards: identity first (everything
// but GET/HEAD /healthz needs a valid X-SanHolo-Identity for the tenant: 401
// bad or missing, 403 for another tenant), then the CSRF double-submit
// (cookie issued when absent, X-CSRF-Token required and equal on POST, PUT,
// PATCH and DELETE). The token reaches next in the request header
// "X-Test-CSRF" so a test page can echo it.
func Guard(next http.Handler, v *Verifier) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isHealthz := r.URL.Path == "/healthz" && (r.Method == http.MethodGet || r.Method == http.MethodHead)
		if !isHealthz {
			switch verdict, _ := v.Verify(r.Header.Get(Header)); verdict {
			case OK:
			case Forbidden:
				w.WriteHeader(http.StatusForbidden)
				return
			default:
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}
		if p := r.URL.Path; p == "/healthz" || p == "/metrics" || strings.HasPrefix(p, "/static/") {
			next.ServeHTTP(w, r)
			return
		}
		ck, _ := r.Cookie(CSRFCookie)
		tok, issued := "", false
		if ck != nil {
			tok = ck.Value
		}
		if !validCSRFToken(tok) {
			tok, issued = newCSRFToken(), true
			http.SetCookie(w, &http.Cookie{
				Name: CSRFCookie, Value: tok, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
			})
		}
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			if issued || subtle.ConstantTimeCompare([]byte(r.Header.Get(CSRFHeader)), []byte(tok)) != 1 {
				w.WriteHeader(http.StatusForbidden)
				return
			}
		}
		r.Header.Set("X-Test-CSRF", tok)
		next.ServeHTTP(w, r)
	})
}
