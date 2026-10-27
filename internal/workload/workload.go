// Package workload verifies the Nomad workload identity JWT a tenant's bot
// presents to the control plane (audience sanholo.cloud). The token is signed
// by Nomad; the keys come from Nomad's JWKS endpoint. The tenant is taken
// only from the verified nomad_job_id claim.
//
// Findings from a Nomad 2.0.6 dev agent (testdata/nomad-2.0.6.*): tokens are
// RS256 with a kid; the JWKS lists RSA keys with alg RS256; the claims carry
// aud, exp, iat, nbf, jti, nomad_namespace, nomad_job_id, nomad_allocation_id,
// nomad_task and sub, and carry NO iss unless the server sets oidc_issuer.
// EdDSA is accepted as well, since Nomad's keyring can use it.
package workload

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/lhaig/sanholo-cloud/internal/tenant"
)

// Audience is the only audience the control plane accepts.
const Audience = "sanholo.cloud"

// JobPrefix precedes the tenant id in the job id.
const JobPrefix = "sanholo-tenant-"

var (
	// ErrInvalid means the token is not acceptable. The reason is never
	// shown to the caller.
	ErrInvalid = errors.New("workload: token not valid")
	// ErrUnavailable means the keys could not be fetched.
	ErrUnavailable = errors.New("workload: signing keys unavailable")
)

// Config configures a Verifier.
type Config struct {
	JWKSURL   string
	Issuer    string // required to match when set; Nomad omits iss by default
	Namespace string // nomad_namespace must equal this
	// Skew is the clock leeway for exp, nbf and iat. Default 30s.
	Skew time.Duration
	// MinRefresh is the shortest time between two fetches of the JWKS
	// (bounds what unknown key ids can cause). Default 30s.
	MinRefresh time.Duration
	// MaxAge is how long keys are used before a refresh is attempted.
	// Default 10m. StaleMax is how long cached keys outlive a failing
	// endpoint. Default 1h.
	MaxAge   time.Duration
	StaleMax time.Duration
	Client   *http.Client
	Now      func() time.Time
}

// Claims is what a verified token proves.
type Claims struct {
	TenantID     string
	JobID        string
	Namespace    string
	AllocationID string
}

// Verifier checks workload identity tokens.
type Verifier struct {
	cfg Config

	fetchMu sync.Mutex // one fetch at a time
	mu      sync.Mutex
	keys    map[string]crypto.PublicKey
	fetched time.Time // last successful fetch
	tried   time.Time // last attempt
}

const (
	maxJWKSBytes = 256 << 10
	maxKeys      = 32
)

// New returns a Verifier.
func New(cfg Config) (*Verifier, error) {
	if cfg.JWKSURL == "" || cfg.Namespace == "" {
		return nil, errors.New("workload: JWKS URL and namespace are required")
	}
	if cfg.Skew <= 0 {
		cfg.Skew = 30 * time.Second
	}
	if cfg.MinRefresh <= 0 {
		cfg.MinRefresh = 30 * time.Second
	}
	if cfg.MaxAge <= 0 {
		cfg.MaxAge = 10 * time.Minute
	}
	if cfg.StaleMax <= 0 {
		cfg.StaleMax = time.Hour
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{
			Timeout:       5 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	return &Verifier{cfg: cfg}, nil
}

// Verify checks token and returns the tenant it proves.
func (v *Verifier) Verify(ctx context.Context, token string) (Claims, error) {
	if token == "" || len(token) > 8192 {
		return Claims{}, ErrInvalid
	}
	opts := []jwt.ParserOption{
		jwt.WithValidMethods([]string{"RS256", "EdDSA"}),
		jwt.WithLeeway(v.cfg.Skew),
		jwt.WithTimeFunc(v.cfg.Now),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
	}
	if v.cfg.Issuer != "" {
		opts = append(opts, jwt.WithIssuer(v.cfg.Issuer))
	}
	var unavailable bool
	var claims nomadClaims
	_, err := jwt.NewParser(opts...).ParseWithClaims(token, &claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		key, err := v.key(ctx, kid)
		if errors.Is(err, ErrUnavailable) {
			unavailable = true
		}
		if err != nil {
			return nil, err
		}
		switch key.(type) {
		case *rsa.PublicKey:
			if t.Method.Alg() != "RS256" {
				return nil, errors.New("key and algorithm differ")
			}
		case ed25519.PublicKey:
			if t.Method.Alg() != "EdDSA" {
				return nil, errors.New("key and algorithm differ")
			}
		}
		return key, nil
	})
	if unavailable {
		return Claims{}, ErrUnavailable
	}
	if err != nil {
		return Claims{}, ErrInvalid
	}
	if claims.IssuedAt == nil || claims.NotBefore == nil || claims.ExpiresAt == nil {
		return Claims{}, ErrInvalid
	}
	if len(claims.Audience) != 1 || claims.Audience[0] != Audience {
		return Claims{}, ErrInvalid
	}
	if claims.Namespace != v.cfg.Namespace {
		return Claims{}, ErrInvalid
	}
	id, ok := strings.CutPrefix(claims.JobID, JobPrefix)
	if !ok || !tenant.Valid(id) {
		return Claims{}, ErrInvalid
	}
	return Claims{TenantID: id, JobID: claims.JobID, Namespace: claims.Namespace, AllocationID: claims.AllocationID}, nil
}

type nomadClaims struct {
	jwt.RegisteredClaims
	Namespace    string `json:"nomad_namespace"`
	JobID        string `json:"nomad_job_id"`
	AllocationID string `json:"nomad_allocation_id"`
}

// key returns the key for kid, refreshing the JWKS when the id is unknown or
// the cache is old.
func (v *Verifier) key(ctx context.Context, kid string) (crypto.PublicKey, error) {
	if kid == "" {
		return nil, errors.New("no key id")
	}
	now := v.cfg.Now()
	v.mu.Lock()
	k, ok := v.keys[kid]
	fresh := ok && now.Sub(v.fetched) < v.cfg.MaxAge
	v.mu.Unlock()
	if fresh {
		return k, nil
	}
	v.refresh(ctx)
	v.mu.Lock()
	defer v.mu.Unlock()
	k, ok = v.keys[kid]
	age := v.cfg.Now().Sub(v.fetched)
	switch {
	case ok && age < v.cfg.StaleMax:
		return k, nil
	case v.keys == nil || age >= v.cfg.StaleMax:
		return nil, ErrUnavailable
	}
	return nil, errors.New("unknown key id")
}

// refresh fetches the JWKS unless one was attempted within MinRefresh.
func (v *Verifier) refresh(ctx context.Context) {
	v.fetchMu.Lock()
	defer v.fetchMu.Unlock()
	now := v.cfg.Now()
	v.mu.Lock()
	recent := !v.tried.IsZero() && now.Sub(v.tried) < v.cfg.MinRefresh
	v.mu.Unlock()
	if recent {
		return
	}
	keys, err := v.fetch(ctx)
	v.mu.Lock()
	defer v.mu.Unlock()
	v.tried = now
	if err == nil {
		v.keys, v.fetched = keys, now
	}
}

func (v *Verifier) fetch(ctx context.Context) (map[string]crypto.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.cfg.JWKSURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := v.cfg.Client.Do(req)
	if err != nil {
		return nil, errors.New("jwks request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jwks status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSBytes+1))
	if err != nil || len(body) > maxJWKSBytes {
		return nil, errors.New("jwks body")
	}
	return ParseJWKS(body)
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Crv string `json:"crv"`
	N   string `json:"n"`
	E   string `json:"e"`
	X   string `json:"x"`
}

// ParseJWKS reads RSA (2048 bits or more) and Ed25519 signing keys, ignoring
// others.
func ParseJWKS(body []byte) (map[string]crypto.PublicKey, error) {
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, errors.New("jwks does not parse")
	}
	out := map[string]crypto.PublicKey{}
	for _, k := range set.Keys {
		if k.Kid == "" || (k.Use != "" && k.Use != "sig") || len(out) >= maxKeys {
			continue
		}
		switch {
		case k.Kty == "RSA":
			n, e := b64int(k.N), b64int(k.E)
			if n == nil || e == nil || n.BitLen() < 2048 || !e.IsInt64() || e.Int64() < 3 || e.Int64() > 1<<31 {
				continue
			}
			out[k.Kid] = &rsa.PublicKey{N: n, E: int(e.Int64())}
		case k.Kty == "OKP" && k.Crv == "Ed25519":
			x, err := base64.RawURLEncoding.DecodeString(k.X)
			if err != nil || len(x) != ed25519.PublicKeySize {
				continue
			}
			out[k.Kid] = ed25519.PublicKey(x)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("jwks has no usable key")
	}
	return out, nil
}

func b64int(s string) *big.Int {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) == 0 {
		return nil
	}
	return new(big.Int).SetBytes(b)
}
