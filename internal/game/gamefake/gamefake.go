// Package gamefake is an in-memory stand-in for the Replicant Space API, for
// tests. It serves GET /v1/accounts/me for the tokens a test registers and
// records every request so tests can search for leaks.
package gamefake

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Request is one recorded request.
type Request struct {
	Method string
	URL    string
	Header http.Header
	Body   string
}

// Replicant is one replicant in an account.
type Replicant struct {
	Code, Name, CreatedAt string
}

// Fake is the server and its state.
type Fake struct {
	srv *httptest.Server

	mu       sync.Mutex
	accounts map[string][]Replicant // bearer token -> replicants
	status   int                    // when non-zero, every request gets this
	reqs     []Request
}

// New starts a fake. It stops with the test.
func New(t testing.TB) *Fake {
	t.Helper()
	f := &Fake{accounts: map[string][]Replicant{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

// URL is the address to give game.Config.
func (f *Fake) URL() string { return f.srv.URL }

// Close stops the server early (to test an unreachable game).
func (f *Fake) Close() { f.srv.Close() }

// AddAccount makes token valid for an account holding reps.
func (f *Fake) AddAccount(token string, reps ...Replicant) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accounts[token] = reps
}

// SetStatus makes every request answer with status (0 restores normal).
func (f *Fake) SetStatus(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = status
}

// Requests returns a copy of everything received so far.
func (f *Fake) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.reqs...)
}

func (f *Fake) handle(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, Request{Method: r.Method, URL: r.URL.RequestURI(), Header: r.Header.Clone(), Body: string(b)})
	w.Header().Set("Content-Type", "application/json")
	if f.status != 0 {
		w.WriteHeader(f.status)
		_, _ = io.WriteString(w, `{"error":"forced"}`)
		return
	}
	reps, ok := f.accounts[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	if r.Method != http.MethodGet || r.URL.Path != "/v1/accounts/me" || !ok {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"invalid token"}`)
		return
	}
	out := make([]map[string]any, 0, len(reps))
	for _, rp := range reps {
		out = append(out, map[string]any{"replicant_code": rp.Code, "name": rp.Name, "created_at": rp.CreatedAt, "device_count": 3})
	}
	body, _ := json.Marshal(map[string]any{"email": "player@example.org", "name": "Player", "status": "active", "replicants": out})
	_, _ = w.Write(body)
}
