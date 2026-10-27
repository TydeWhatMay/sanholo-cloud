// Package instancefake is a stand-in for a SanHolo instance's
// GET /hosted/export, for tests of the data export. It writes its archive
// the way the client does (internal/web/hosted_export.go in san-holo): an
// archive/zip writer on a plain response stream, so every entry carries a
// data descriptor, and a 429 with Retry-After while another export runs. It
// sits behind the client's identity guard (clientfake.Guard). It is not used
// by the service.
package instancefake

import (
	"archive/zip"
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/lhaig/sanholo-cloud/internal/identity/clientfake"
)

// Entry is one file of the archive.
type Entry struct {
	Name string
	Data []byte
}

// Fake is the server and its script.
type Fake struct {
	srv *httptest.Server

	mu       sync.Mutex
	entries  []Entry
	busy     int // answer 429 this many times first
	status   int // answer this status (not 200) instead of the archive
	cutAfter int // -1: write everything; n >= 0: break the stream after n entries
	gate     chan struct{}
	gateAt   int
	requests int
	ids      []string
}

// New starts a fake instance for tenant that accepts identities signed by
// the key pub. It stops with the test.
func New(t *testing.T, pub ed25519.PublicKey, tenant string) *Fake {
	t.Helper()
	f := &Fake{cutAfter: -1, gateAt: -1}
	guard := clientfake.Guard(http.HandlerFunc(f.serve), clientfake.New([]ed25519.PublicKey{pub}, tenant, nil))
	f.srv = httptest.NewServer(guard)
	t.Cleanup(f.srv.Close)
	return f
}

// Addr is host:port, what a Resolver returns.
func (f *Fake) Addr() string { return f.srv.Listener.Addr().String() }

// SetEntries sets the files of the archive.
func (f *Fake) SetEntries(e ...Entry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = e
}

// Busy makes the next n requests answer 429 with Retry-After: 1.
func (f *Fake) Busy(n int) { f.set(func() { f.busy = n }) }

// Fail makes every request answer status (not 200).
func (f *Fake) Fail(status int) { f.set(func() { f.status = status }) }

// CutAfter breaks the stream (closes the connection) after n entries.
func (f *Fake) CutAfter(n int) { f.set(func() { f.cutAfter = n }) }

// Gate holds the stream before entry n until the returned function is
// called, flushing what was written so far. It lets a test see that bytes
// reach the reader before the instance has finished.
func (f *Fake) Gate(n int) (release func()) {
	ch := make(chan struct{})
	f.set(func() { f.gate, f.gateAt = ch, n })
	var once sync.Once
	return func() { once.Do(func() { close(ch) }) }
}

// Requests is how many requests reached the handler (past the guard).
func (f *Fake) Requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

// Identities are the identity headers of those requests.
func (f *Fake) Identities() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ids...)
}

func (f *Fake) set(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/hosted/export" || r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	f.mu.Lock()
	f.requests++
	f.ids = append(f.ids, r.Header.Get(clientfake.Header))
	busy, status := f.busy, f.status
	if f.busy > 0 {
		f.busy--
	}
	entries := append([]Entry(nil), f.entries...)
	cut, gate, gateAt := f.cutAfter, f.gate, f.gateAt
	f.mu.Unlock()

	if busy > 0 {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	if status != 0 {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="sanholo-export-test.zip"`)
	zw := zip.NewWriter(w)
	flush := func() {
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
	}
	for i, e := range entries {
		if i == cut {
			flush()
			panic(http.ErrAbortHandler) // closes the connection mid-stream
		}
		if gate != nil && i == gateAt {
			_ = zw.Flush()
			flush()
			<-gate
		}
		ew, err := zw.CreateHeader(&zip.FileHeader{Name: e.Name, Method: zip.Deflate, Modified: time.Unix(1_700_000_000, 0)})
		if err != nil {
			return
		}
		if _, err := ew.Write(e.Data); err != nil {
			return
		}
	}
	if cut >= len(entries) && cut >= 0 {
		flush()
		panic(http.ErrAbortHandler)
	}
	_ = zw.Close()
}
