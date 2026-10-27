// Package qmfake is an in-memory stand-in for qm, for tests. It implements
// the endpoints in secrets/wire.go (provisional until qm's contract lands;
// change both together) and enforces the control plane's write-only policy
// the way the real qm would: a read of a data path is 403, so is any use
// of a wrong token or of an endpoint outside the policy.
//
// Like Vault's KV v2, destroying versions leaves the metadata entry, so a
// listing still shows the name; the client must look at the versions.
package qmfake

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

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
	data      map[string]string
	destroyed bool
}

// Fake is the server and its state.
type Fake struct {
	srv   *httptest.Server
	token string

	mu       sync.Mutex
	sealed   bool
	override Override
	secrets  map[string][]version // path -> versions (index 0 is version 1)
	reqs     []Request
}

// New starts a fake that accepts clientToken. It stops with the test.
func New(t testing.TB, clientToken string) *Fake {
	t.Helper()
	f := &Fake{token: clientToken, secrets: map[string][]version{}}
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

// SetSealed makes /sys/ready answer 503.
func (f *Fake) SetSealed(v bool) { f.lock(func() { f.sealed = v }) }

// SetOverride installs (or with nil removes) an Override.
func (f *Fake) SetOverride(o Override) { f.lock(func() { f.override = o }) }

// Seed adds a live version straight into the store, bypassing the API, for
// paths a test needs that the client never writes.
func (f *Fake) Seed(path string, fields map[string]string) {
	f.lock(func() { f.secrets[path] = append(f.secrets[path], version{data: fields}) })
}

// Live counts the versions still holding data under prefix.
func (f *Fake) Live(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for p, vs := range f.secrets {
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		for _, v := range vs {
			if !v.destroyed {
				n++
			}
		}
	}
	return n
}

// Stored returns the newest live version of path.
func (f *Fake) Stored(path string) (map[string]string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	vs := f.secrets[path]
	for i := len(vs) - 1; i >= 0; i-- {
		if !vs[i].destroyed {
			return vs[i].data, true
		}
	}
	return nil, false
}

// Versions returns the total number of versions ever written to path.
func (f *Fake) Versions(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.secrets[path])
}

func (f *Fake) lock(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

const (
	dataPrefix    = "/qm/v1/secret/data/"
	metaPrefix    = "/qm/v1/secret/metadata/"
	destroyPrefix = "/qm/v1/secret/destroy/"
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
	if p == "/qm/v1/sys/ready" {
		if f.sealed {
			reply(w, http.StatusServiceUnavailable, `{"errors":["sealed"]}`)
			return
		}
		reply(w, http.StatusOK, `{}`)
		return
	}
	if r.Header.Get("X-Qm-Token") != f.token {
		reply(w, http.StatusForbidden, `{"errors":["permission denied"]}`)
		return
	}
	write := r.Method == http.MethodPost || r.Method == http.MethodPut
	switch {
	case strings.HasPrefix(p, dataPrefix) && write:
		f.put(w, strings.TrimPrefix(p, dataPrefix), b)
	case strings.HasPrefix(p, metaPrefix) && r.Method == http.MethodGet && r.URL.Query().Get("list") == "true":
		f.list(w, strings.TrimPrefix(p, metaPrefix))
	case strings.HasPrefix(p, metaPrefix) && r.Method == http.MethodGet:
		f.metadata(w, strings.TrimPrefix(p, metaPrefix))
	case strings.HasPrefix(p, destroyPrefix) && write:
		f.destroy(w, strings.TrimPrefix(p, destroyPrefix), b)
	default:
		// Includes GET of a data path: the policy has no read.
		reply(w, http.StatusForbidden, `{"errors":["permission denied"]}`)
	}
}

func (f *Fake) put(w http.ResponseWriter, path string, body []byte) {
	var in struct {
		Data map[string]string `json:"data"`
	}
	if json.Unmarshal(body, &in) != nil || len(in.Data) == 0 {
		reply(w, http.StatusBadRequest, `{"errors":["bad body"]}`)
		return
	}
	f.secrets[path] = append(f.secrets[path], version{data: in.Data})
	reply(w, http.StatusOK, `{"data":{"version":`+strconv.Itoa(len(f.secrets[path]))+`}}`)
}

func (f *Fake) list(w http.ResponseWriter, prefix string) {
	seen := map[string]bool{}
	for p := range f.secrets {
		rest, ok := strings.CutPrefix(p, prefix)
		if !ok {
			continue
		}
		if i := strings.Index(rest, "/"); i >= 0 {
			rest = rest[:i+1]
		}
		seen[rest] = true
	}
	if len(seen) == 0 {
		reply(w, http.StatusNotFound, `{"errors":[]}`)
		return
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out, _ := json.Marshal(map[string]any{"data": map[string]any{"keys": keys}})
	reply(w, http.StatusOK, string(out))
}

func (f *Fake) metadata(w http.ResponseWriter, path string) {
	vs, ok := f.secrets[path]
	if !ok {
		reply(w, http.StatusNotFound, `{"errors":[]}`)
		return
	}
	m := map[string]any{}
	for i, v := range vs {
		m[strconv.Itoa(i+1)] = map[string]any{"destroyed": v.destroyed}
	}
	out, _ := json.Marshal(map[string]any{"data": map[string]any{"current_version": len(vs), "versions": m}})
	reply(w, http.StatusOK, string(out))
}

func (f *Fake) destroy(w http.ResponseWriter, path string, body []byte) {
	var in struct {
		Versions []int `json:"versions"`
	}
	vs, ok := f.secrets[path]
	if json.Unmarshal(body, &in) != nil || !ok {
		reply(w, http.StatusBadRequest, `{"errors":["bad request"]}`)
		return
	}
	for _, n := range in.Versions {
		if n >= 1 && n <= len(vs) {
			vs[n-1] = version{destroyed: true}
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func reply(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}
