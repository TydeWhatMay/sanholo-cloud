package secrets

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// THE QM WIRE CONTRACT (control plane side) -- every detail of how the
// control plane talks to qm is in this file, so a change in qm's contract is
// confined here (plus the fake in qmfake, which mirrors this file and must
// change with it).
//
// Per qm contract, prds/research/qm-contract.md:
//
//	login:    POST <addr>/qm/v1/auth/approle/login   {"role_id":..,"secret_id":..}
//	          -> {"auth":{"client_token":..,"lease_duration":<seconds>}}
//	renew:    POST <addr>/qm/v1/auth/token/renew-self        (X-QM-Token)
//	          -> {"auth":{"lease_duration":<seconds>}}
//	write:    POST <addr>/qm/v1/secret/data/<path>   {"data":{...}}   (X-QM-Token)
//	          -> {"version":N,"created_time":".."}      (flat, no data envelope)
//	metadata: GET  <addr>/qm/v1/secret/metadata/<path>
//	          -> {"current_version":N,"oldest_version":N,"created_time":..,
//	              "updated_time":..,"versions":{"<n>":{..,"destroyed":bool}}}
//	          404 when the path is not present. Never carries a value.
//	health:   GET  <addr>/qm/v1/sys/health   (no auth; always 200)
//	          -> {"initialized":bool,"sealed":bool}
//
// Errors: every authentication failure and policy denial is 403 `forbidden`
// (qm never answers 401); a sealed qm answers 503 `vault_sealed` on any
// barrier-backed request. A rotation is a plain new write: qm keeps 16
// versions and prunes the oldest.
//
// PROVISIONAL (qm Part E, not shipped; shapes may change). Both need delete
// plus sudo on the matching path, which the interim grants do not include:
//
//	delete one path:  DELETE <addr>/qm/v1/secret/metadata/<path>
//	                  (every version and the metadata; the fake implements it,
//	                  this package does not call it)
//	purge a prefix:   DELETE <addr>/qm/v1/secret/purge/<prefix>
//	                  (a whole tenant prefix in one audited operation)
//	                  -> 200 or 204
//
// The error body shape is not in the contract. errorCode accepts the
// likely ones ({"code":..}, {"error":{"code":..}}, {"error":".."}).
//
// <path> is sanholo/tenants/<tenant>/game; the "secret/" mount is part of
// the URL prefixes below. The policy has no read of data: a GET of a data
// path is 403. This package never issues one.
const (
	healthPath     = "/qm/v1/sys/health"
	loginPath      = "/qm/v1/auth/approle/login"
	renewPath      = "/qm/v1/auth/token/renew-self"
	dataPrefix     = "/qm/v1/secret/data/"
	metaPrefix     = "/qm/v1/secret/metadata/"
	purgePrefix    = "/qm/v1/secret/purge/" // provisional (Part E)
	authHeaderName = "X-QM-Token"

	tenantsRoot = "sanholo/tenants/"
	gameLeaf    = "game"

	codeSealed = "vault_sealed"

	// defaultLease is assumed when qm reports no usable lease duration.
	defaultLease = 15 * time.Minute

	// Bounds on what a (possibly misbehaving) qm may make us do.
	maxBody     = 1 << 20
	maxVersions = 1000
)

// call is one request, described without any transport detail.
type call struct {
	method string
	path   string
	body   []byte
	// okNotFound makes a 404 a normal answer (not present, nothing to purge).
	okNotFound bool
	// noAuth omits the token header (health, login).
	noAuth bool
}

func tenantPrefix(tenantID string) string { return tenantsRoot + tenantID }

func gamePath(tenantID string) string { return tenantPrefix(tenantID) + "/" + gameLeaf }

func healthCall() call { return call{method: http.MethodGet, path: healthPath, noAuth: true} }

func loginCall(roleID, secretID string) (call, error) {
	body, err := json.Marshal(map[string]string{"role_id": roleID, "secret_id": secretID})
	if err != nil {
		return call{}, errors.New("encode login")
	}
	return call{method: http.MethodPost, path: loginPath, body: body, noAuth: true}, nil
}

func renewCall() call { return call{method: http.MethodPost, path: renewPath, body: []byte("{}")} }

func putCall(path string, c Credentials) (call, error) {
	body, err := json.Marshal(map[string]any{"data": map[string]string{
		"api_token": c.APIToken, "replicant_code": c.ReplicantCode,
	}})
	if err != nil {
		return call{}, errors.New("encode credentials")
	}
	return call{method: http.MethodPost, path: dataPrefix + path, body: body}, nil
}

func metaCall(path string) call {
	return call{method: http.MethodGet, path: metaPrefix + path, okNotFound: true}
}

func purgeCall(prefix string) call {
	return call{method: http.MethodDelete, path: purgePrefix + prefix, okNotFound: true}
}

// decodeHealth returns qm's initialized and sealed flags.
func decodeHealth(body []byte) (initialized, sealed bool, err error) {
	var r struct {
		Initialized *bool `json:"initialized"`
		Sealed      *bool `json:"sealed"`
	}
	if err := json.Unmarshal(body, &r); err != nil || r.Initialized == nil || r.Sealed == nil {
		return false, false, errors.New("malformed health response")
	}
	return *r.Initialized, *r.Sealed, nil
}

// decodeAuth returns the client token (empty when the reply carries none,
// as renew-self may) and the lease. A missing or non-positive lease is
// defaultLease.
func decodeAuth(body []byte) (token string, lease time.Duration, err error) {
	var r struct {
		Auth struct {
			ClientToken   string  `json:"client_token"`
			LeaseDuration float64 `json:"lease_duration"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return "", 0, errors.New("malformed auth response")
	}
	lease = defaultLease
	if s := r.Auth.LeaseDuration; s > 0 && s < 1e9 {
		lease = time.Duration(s * float64(time.Second))
	}
	return r.Auth.ClientToken, lease, nil
}

// decodePutVersion returns the version number the write created.
func decodePutVersion(body []byte) (int, error) {
	var r struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(body, &r); err != nil || r.Version < 1 {
		return 0, errors.New("malformed write response")
	}
	return r.Version, nil
}

// decodeCurrentVersion returns the current version number, or 0 when that
// version is destroyed (no value to find).
func decodeCurrentVersion(body []byte) (int, error) {
	var r struct {
		Current  int `json:"current_version"`
		Versions map[string]struct {
			Destroyed bool `json:"destroyed"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(body, &r); err != nil || r.Current < 1 {
		return 0, errors.New("malformed metadata response")
	}
	if len(r.Versions) > maxVersions {
		return 0, errors.New("metadata response has too many versions")
	}
	if v, ok := r.Versions[fmt.Sprint(r.Current)]; ok && v.Destroyed {
		return 0, nil
	}
	return r.Current, nil
}

// errorCode extracts qm's error code from a non-success body, or "". The
// body is matched against known codes only and never kept or echoed.
func errorCode(body []byte) string {
	var r struct {
		Code  string          `json:"code"`
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &r) != nil {
		return ""
	}
	if r.Code != "" {
		return r.Code
	}
	var obj struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(r.Error, &obj) == nil && obj.Code != "" {
		return obj.Code
	}
	var s string
	if json.Unmarshal(r.Error, &s) == nil {
		return s
	}
	return ""
}

// statusError maps a non-success HTTP status (and, for 503, the error code)
// to a typed error. It never keeps the response body.
func statusError(status int, body []byte) error {
	switch {
	case status == http.StatusForbidden:
		return ErrDenied
	case status == http.StatusServiceUnavailable && errorCode(body) == codeSealed:
		return ErrSealed
	case status == http.StatusTooManyRequests || status >= 500:
		return ErrUnavailable
	default:
		// Includes 401, which qm never sends.
		return ErrProtocol
	}
}

// ParseAddr validates QM_ADDR: an origin, https, or http on a loopback host
// (local development, the same rule as APP_BASE_URL). It returns the origin
// without a trailing slash.
func ParseAddr(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", errors.New("want an origin such as https://qm.example.org")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !loopback(u.Hostname()) {
			return "", errors.New("http is allowed for loopback hosts only")
		}
	default:
		return "", fmt.Errorf("scheme must be https")
	}
	return u.Scheme + "://" + u.Host, nil
}
