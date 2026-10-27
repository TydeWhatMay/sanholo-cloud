// Package qmfake is an in-memory stand-in for qm, for tests. It mirrors the
// integration contract in prds/research/qm-contract.md and the wire in
// secrets/wire.go (change both together):
//
//   - the provisioner policy is write-only: create and update on data, read
//     of metadata, nothing else. A GET of a data path, a list and a destroy
//     are 403 `forbidden`, as is any wrong or expired token (qm never
//     answers 401);
//   - AppRole login issues tokens with a lease, renew-self extends them, and
//     ExpireTokens makes the next request 403 so tests can force a re-login;
//   - a sealed qm answers 503 `vault_sealed` on every barrier request while
//     sys/health stays 200 with sealed:true;
//   - a write keeps at most 16 versions and prunes the oldest;
//   - the Part E deletions (DELETE metadata/<path>, DELETE purge/<prefix>)
//     are implemented; SetDeleteAllowed(false) models the interim policy,
//     where the provisioner holds no delete grant (403).
package qmfake

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const maxVersions = 16

// Request is one recorded request, complete enough to search for leaks.
type Request struct {
	Method string
	URL    string // path and query
	Header http.Header
	Body   string
}

// Override lets a test replace the answer for a request. Return ok=false to
// fall through to the normal behaviour.
type Override func(r Request) (status int, body string, ok bool)

type version struct {
	n       int
	data    map[string]string
	created time.Time
}

type entry struct {
	current int // the highest version number ever written
	vers    []version
}

// Fake is the server and its state.
type Fake struct {
	srv   *httptest.Server
	token string // static token, always valid (development mode)

	mu            sync.Mutex
	initialized   bool
	sealed        bool
	deleteAllowed bool
	renewFails    bool
	leaseSeconds  int
	roleID        string
	secretID      string
	issued        map[string]bool // client tokens from login
	lastIssued    string
	issuedCount   int
	override      Override
	secrets       map[string]*entry
	reqs          []Request
}

// New starts a fake that accepts the static clientToken (the development
// mode). Use AddAppRole for the AppRole flow. It stops with the test.
func New(t testing.TB, clientToken string) *Fake {
	t.Helper()
	f := &Fake{
		token: clientToken, initialized: true, deleteAllowed: true, leaseSeconds: 900,
		issued: map[string]bool{}, secrets: map[string]*entry{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

// URL is the address to give secrets.Config.
func (f *Fake) URL() string { return f.srv.URL }

// Close stops the server early (to test an unreachable qm).
func (f *Fake) Close() { f.srv.Close() }

// Requests returns a copy of everything received so far.
func (f *Fake) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.reqs...)
}

// Count returns how many recorded requests used method on a URL that starts
// with urlPrefix.
func (f *Fake) Count(method, urlPrefix string) int {
	n := 0
	for _, r := range f.Requests() {
		if r.Method == method && strings.HasPrefix(r.URL, urlPrefix) {
			n++
		}
	}
	return n
}

// AddAppRole makes login accept roleID and secretID.
func (f *Fake) AddAppRole(roleID, secretID string) {
	f.lock(func() { f.roleID, f.secretID = roleID, secretID })
}

// SetLease sets the lease (seconds) that login and renew-self report.
func (f *Fake) SetLease(seconds int) { f.lock(func() { f.leaseSeconds = seconds }) }

// ExpireTokens invalidates every token login has issued; the next request
// with one is 403.
func (f *Fake) ExpireTokens() { f.lock(func() { f.issued = map[string]bool{} }) }

// SetRenewFails makes renew-self answer 403.
func (f *Fake) SetRenewFails(v bool) { f.lock(func() { f.renewFails = v }) }

// LastIssuedToken is the client token the latest login returned.
func (f *Fake) LastIssuedToken() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastIssued
}

// SetSealed makes every barrier request 503 vault_sealed (sys/health stays
// 200 and reports it).
func (f *Fake) SetSealed(v bool) { f.lock(func() { f.sealed = v }) }

// SetInitialized sets the initialized flag sys/health reports.
func (f *Fake) SetInitialized(v bool) { f.lock(func() { f.initialized = v }) }

// SetDeleteAllowed grants (default) or withdraws the Part E delete grant.
func (f *Fake) SetDeleteAllowed(v bool) { f.lock(func() { f.deleteAllowed = v }) }

// SetOverride installs (or with nil removes) an Override.
func (f *Fake) SetOverride(o Override) { f.lock(func() { f.override = o }) }

// Seed adds a version straight into the store, bypassing the API, for paths
// a test needs that the client never writes.
func (f *Fake) Seed(path string, fields map[string]string) {
	f.lock(func() { f.write(path, fields) })
}

// Live counts the versions still held under prefix.
func (f *Fake) Live(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for p, e := range f.secrets {
		if strings.HasPrefix(p, prefix) {
			n += len(e.vers)
		}
	}
	return n
}

// Stored returns the newest version of path.
func (f *Fake) Stored(path string) (map[string]string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.secrets[path]
	if e == nil || len(e.vers) == 0 {
		return nil, false
	}
	return e.vers[len(e.vers)-1].data, true
}

// Versions returns the number of versions ever written to path (the current
// version number); 0 when the path is not present.
func (f *Fake) Versions(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e := f.secrets[path]; e != nil {
		return e.current
	}
	return 0
}

func (f *Fake) lock(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

const (
	healthPath   = "/qm/v1/sys/health"
	loginPath    = "/qm/v1/auth/approle/login"
	renewPath    = "/qm/v1/auth/token/renew-self"
	dataPrefix   = "/qm/v1/secret/data/"
	metaPrefix   = "/qm/v1/secret/metadata/"
	purgePrefix  = "/qm/v1/secret/purge/"
	forbidden    = `{"error":{"code":"forbidden","message":"permission denied"}}`
	sealedAnswer = `{"error":{"code":"vault_sealed","message":"vault is sealed"}}`
)

func (f *Fake) handle(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	rec := Request{Method: r.Method, URL: r.URL.RequestURI(), Header: r.Header.Clone(), Body: string(b)}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, rec)

	if f.override != nil {
		if status, body, ok := f.override(rec); ok {
			reply(w, status, body)
			return
		}
	}
	p := r.URL.Path
	if p == healthPath {
		out, _ := json.Marshal(map[string]bool{"initialized": f.initialized, "sealed": f.sealed})
		reply(w, http.StatusOK, string(out))
		return
	}
	if f.sealed {
		reply(w, http.StatusServiceUnavailable, sealedAnswer)
		return
	}
	if p == loginPath && r.Method == http.MethodPost {
		f.login(w, b)
		return
	}
	tok := r.Header.Get("X-QM-Token")
	if tok == "" || (tok != f.token && !f.issued[tok]) {
		reply(w, http.StatusForbidden, forbidden)
		return
	}
	write := r.Method == http.MethodPost || r.Method == http.MethodPut
	switch {
	case p == renewPath && r.Method == http.MethodPost:
		if f.renewFails || tok == f.token {
			reply(w, http.StatusForbidden, forbidden)
			return
		}
		f.auth(w, tok)
	case strings.HasPrefix(p, dataPrefix) && write:
		f.put(w, strings.TrimPrefix(p, dataPrefix), b)
	case strings.HasPrefix(p, metaPrefix) && r.Method == http.MethodGet && r.URL.Query().Get("list") == "":
		f.metadata(w, strings.TrimPrefix(p, metaPrefix))
	case strings.HasPrefix(p, metaPrefix) && r.Method == http.MethodDelete && f.deleteAllowed:
		f.deleteTree(w, strings.TrimPrefix(p, metaPrefix), false)
	case strings.HasPrefix(p, purgePrefix) && r.Method == http.MethodDelete && f.deleteAllowed:
		f.deleteTree(w, strings.TrimPrefix(p, purgePrefix), true)
	default:
		// Includes GET of a data path, any list and any destroy: the
		// policy has no read, no list and no destroy.
		reply(w, http.StatusForbidden, forbidden)
	}
}

func (f *Fake) login(w http.ResponseWriter, body []byte) {
	var in struct {
		RoleID   string `json:"role_id"`
		SecretID string `json:"secret_id"`
	}
	if json.Unmarshal(body, &in) != nil || f.roleID == "" || in.RoleID != f.roleID || in.SecretID != f.secretID {
		reply(w, http.StatusForbidden, forbidden)
		return
	}
	f.issuedCount++
	tok := "qmc-" + strconv.Itoa(f.issuedCount) + "-" + strings.Repeat("x", 8)
	f.issued[tok] = true
	f.lastIssued = tok
	f.auth(w, tok)
}

func (f *Fake) auth(w http.ResponseWriter, tok string) {
	out, _ := json.Marshal(map[string]any{"auth": map[string]any{"client_token": tok, "lease_duration": f.leaseSeconds}})
	reply(w, http.StatusOK, string(out))
}

// write appends a version and prunes to the newest maxVersions.
func (f *Fake) write(path string, data map[string]string) *entry {
	e := f.secrets[path]
	if e == nil {
		e = &entry{}
		f.secrets[path] = e
	}
	e.current++
	e.vers = append(e.vers, version{n: e.current, data: data, created: time.Now().UTC()})
	if len(e.vers) > maxVersions {
		e.vers = e.vers[len(e.vers)-maxVersions:]
	}
	return e
}

func (f *Fake) put(w http.ResponseWriter, path string, body []byte) {
	var in struct {
		Data map[string]string `json:"data"`
	}
	if json.Unmarshal(body, &in) != nil || len(in.Data) == 0 {
		reply(w, http.StatusBadRequest, `{"error":{"code":"bad_request","message":"bad body"}}`)
		return
	}
	e := f.write(path, in.Data)
	out, _ := json.Marshal(map[string]any{
		"version": e.current, "created_time": e.vers[len(e.vers)-1].created.Format(time.RFC3339Nano),
	})
	reply(w, http.StatusOK, string(out))
}

func (f *Fake) metadata(w http.ResponseWriter, path string) {
	e := f.secrets[path]
	if e == nil {
		reply(w, http.StatusNotFound, `{"error":{"code":"not_found","message":"not found"}}`)
		return
	}
	m := map[string]any{}
	for _, v := range e.vers {
		m[strconv.Itoa(v.n)] = map[string]any{"created_time": v.created.Format(time.RFC3339Nano), "destroyed": false}
	}
	first, last := e.vers[0], e.vers[len(e.vers)-1]
	out, _ := json.Marshal(map[string]any{
		"current_version": e.current, "oldest_version": first.n,
		"created_time": first.created.Format(time.RFC3339Nano), "updated_time": last.created.Format(time.RFC3339Nano),
		"versions": m,
	})
	reply(w, http.StatusOK, string(out))
}

// deleteTree removes one path (tree=false) or a whole prefix, matching on a
// segment boundary so that "abc" does not take "abc-d" with it.
func (f *Fake) deleteTree(w http.ResponseWriter, target string, tree bool) {
	target = strings.TrimSuffix(target, "/")
	found := false
	for p := range f.secrets {
		if p == target || (tree && strings.HasPrefix(p, target+"/")) {
			delete(f.secrets, p)
			found = true
		}
	}
	if !found {
		reply(w, http.StatusNotFound, `{"error":{"code":"not_found","message":"not found"}}`)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func reply(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}
