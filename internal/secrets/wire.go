package secrets

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// THE QM WIRE CONTRACT (control plane side) -- every detail of how the
// control plane talks to qm is in this file, so the day qm publishes its
// integration contract the change is confined here (plus the fake in
// qmfake, which mirrors this file and must change with it).
//
// PROVISIONAL: qm's contract is not published yet. The client side
// (san-holo internal/qmcreds/wire.go) only READS, with these shapes:
//
//	read: GET <addr>/qm/v1/secret/data/<path>   header X-Qm-Token
//	      -> {"data":{"data":{"api_token":...,"replicant_code":...}}}
//
// The write shapes below are the KV v2 (Vault-compatible) counterparts, an
// assumption on top of that, not something the client confirms:
//
//	put:      POST <addr>/qm/v1/secret/data/<path>
//	          {"data":{"api_token":...,"replicant_code":...}}
//	          -> {"data":{"version":N}}
//	list:     GET  <addr>/qm/v1/secret/metadata/<prefix>/?list=true
//	          -> {"data":{"keys":["game","sub/"]}}   (404 when empty)
//	metadata: GET  <addr>/qm/v1/secret/metadata/<path>
//	          -> {"data":{"versions":{"1":{"destroyed":false},...}}}
//	destroy:  POST <addr>/qm/v1/secret/destroy/<path>   {"versions":[1,2]}
//	ready:    GET  <addr>/qm/v1/sys/ready   (no auth; 503 when sealed)
//
// Auth is the header X-Qm-Token carrying the control plane's own client
// token. Policy (US-002): create/update on data, list and read of
// metadata, delete plus sudo on destroy, and NO read of data: a GET of a
// data path is 403. This package never issues one.
//
// <path> is sanholo/tenants/<tenant>/game; the "secret/" mount is part of
// the URL prefixes below.
const (
	readyPath      = "/qm/v1/sys/ready"
	dataPrefix     = "/qm/v1/secret/data/"
	metaPrefix     = "/qm/v1/secret/metadata/"
	destroyPrefix  = "/qm/v1/secret/destroy/"
	authHeaderName = "X-Qm-Token"

	tenantsRoot = "sanholo/tenants/"
	gameLeaf    = "game"

	// Bounds on what a (possibly misbehaving) qm may make us do.
	maxBody  = 1 << 20
	maxDepth = 4
	maxKeys  = 200
)

// call is one request, described without any transport detail.
type call struct {
	method string
	path   string
	query  url.Values
	body   []byte
	// okNotFound makes a 404 a normal answer (an empty listing).
	okNotFound bool
	// noAuth omits the token header (the ready probe).
	noAuth bool
}

// segmentRE is what a path segment returned by qm must look like before we
// build another request from it.
var segmentRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// tenantPrefix is the directory holding a tenant's secrets, with the
// trailing slash that keeps tenant "a" from matching tenant "a-b".
func tenantPrefix(tenantID string) string { return tenantsRoot + tenantID + "/" }

func gamePath(tenantID string) string { return tenantPrefix(tenantID) + gameLeaf }

func readyCall() call { return call{method: http.MethodGet, path: readyPath, noAuth: true} }

func putCall(path string, c Credentials) (call, error) {
	body, err := json.Marshal(map[string]any{"data": map[string]string{
		"api_token": c.APIToken, "replicant_code": c.ReplicantCode,
	}})
	if err != nil {
		return call{}, errors.New("encode credentials")
	}
	return call{method: http.MethodPost, path: dataPrefix + path, body: body}, nil
}

func listCall(prefix string) call {
	return call{
		method: http.MethodGet, path: metaPrefix + prefix,
		query: url.Values{"list": {"true"}}, okNotFound: true,
	}
}

func metaCall(path string) call {
	return call{method: http.MethodGet, path: metaPrefix + path, okNotFound: true}
}

func destroyCall(path string, versions []int) call {
	body, _ := json.Marshal(map[string][]int{"versions": versions})
	return call{method: http.MethodPost, path: destroyPrefix + path, body: body}
}

// decodePutVersion returns the version number the put created.
func decodePutVersion(body []byte) (int, error) {
	var r struct {
		Data struct {
			Version int `json:"version"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil || r.Data.Version < 1 {
		return 0, errors.New("malformed put response")
	}
	return r.Data.Version, nil
}

// decodeKeys returns the entries of a listing. Each must be one safe path
// segment, optionally with a trailing slash (a directory); anything else is
// refused rather than turned into a request path.
func decodeKeys(body []byte) ([]string, error) {
	var r struct {
		Data struct {
			Keys []string `json:"keys"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, errors.New("malformed list response")
	}
	if len(r.Data.Keys) > maxKeys {
		return nil, errors.New("list response has too many keys")
	}
	for _, k := range r.Data.Keys {
		if !segmentRE.MatchString(strings.TrimSuffix(k, "/")) {
			return nil, errors.New("list response has an unsafe key")
		}
	}
	return r.Data.Keys, nil
}

// decodeLiveVersions returns the version numbers that still hold data
// (not destroyed), ascending.
func decodeLiveVersions(body []byte) ([]int, error) {
	var r struct {
		Data struct {
			Versions map[string]struct {
				Destroyed bool `json:"destroyed"`
			} `json:"versions"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, errors.New("malformed metadata response")
	}
	if len(r.Data.Versions) > maxKeys {
		return nil, errors.New("metadata response has too many versions")
	}
	var live []int
	for k, v := range r.Data.Versions {
		n, err := strconv.Atoi(k)
		if err != nil || n < 1 {
			return nil, errors.New("metadata response has a bad version number")
		}
		if !v.Destroyed {
			live = append(live, n)
		}
	}
	sort.Ints(live)
	return live, nil
}

// statusError maps a non-success HTTP status to a typed error. It never
// looks at the response body.
func statusError(status int) error {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return ErrDenied
	case status == http.StatusTooManyRequests || status >= 500:
		return ErrUnavailable
	default:
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
