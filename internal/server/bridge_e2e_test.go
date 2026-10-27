package server

import (
	"crypto/ed25519"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lhaig/sanholo-cloud/internal/identity/clientfake"
)

// guardedBridge starts a stand-in client (see package clientfake for why it
// is a stand-in) that serves tenant's Bridge behind the client's hosted
// guards, verifying identities against pub.
func guardedBridge(t *testing.T, tenant string, pub ed25519.PublicKey) *httptest.Server {
	t.Helper()
	v := clientfake.New([]ed25519.PublicKey{pub}, tenant, nil)
	srv := httptest.NewServer(clientfake.Guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, "bridge of "+tenant+" csrf="+r.Header.Get("X-Test-CSRF"))
	}), v))
	t.Cleanup(srv.Close)
	return srv
}

func addr(s *httptest.Server) string { return strings.TrimPrefix(s.URL, "http://") }

func direct(t *testing.T, srv *httptest.Server, method, path string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// The proxy against the client's own guards: a request through the proxy is
// accepted, the same request without the proxy's identity is refused, and
// the Bridge's CSRF scheme works through the proxy untouched.
func TestEndToEndAgainstTheClientGuards(t *testing.T) {
	b := newBridgeEnv(t)
	ann := b.person(t, "ann@example.org")
	bob := b.person(t, "bob@example.org")
	pub := testKey.Public().(ed25519.PublicKey)
	annSrv, bobSrv := guardedBridge(t, ann.tenant, pub), guardedBridge(t, bob.tenant, pub)
	b.res.route(ann.tenant, addr(annSrv))
	b.res.route(bob.tenant, addr(bobSrv))

	// Positive controls: the guard really refuses what it should.
	if resp, _ := direct(t, annSrv, "GET", "/", nil); resp.StatusCode != 401 {
		t.Fatalf("direct, no identity: %d, want 401", resp.StatusCode)
	}
	if resp, _ := direct(t, annSrv, "GET", "/", map[string]string{"X-SanHolo-Identity": "x.y"}); resp.StatusCode != 401 {
		t.Fatalf("direct, junk identity: %d, want 401", resp.StatusCode)
	}
	if resp, _ := direct(t, annSrv, "GET", "/healthz", nil); resp.StatusCode != 200 {
		t.Fatalf("direct /healthz: %d, want 200 (exempt)", resp.StatusCode)
	}

	b.enterOK(t, ann)
	r := ann.bridge.get("/", nil)
	if r.status != 200 || !strings.HasPrefix(r.body, "bridge of "+ann.tenant) {
		t.Fatalf("through the proxy: %d %q", r.status, r.body)
	}
	csrf := cookie(r, "__Host-sanholo_csrf")
	if csrf == nil || len(csrf.Value) != 64 || !csrf.HttpOnly || !csrf.Secure {
		t.Fatalf("the client's CSRF cookie did not arrive: %+v", csrf)
	}
	// The browser then sends its cookie and header back, as bridge-csrf.js
	// does; the client accepts.
	ann.bridge.cookies["__Host-sanholo_csrf"] = csrf
	ok := ann.bridge.do("POST", "/bridge/enqueue", nil, map[string]string{"X-CSRF-Token": csrf.Value})
	if ok.status != 200 || !strings.Contains(ok.body, "csrf="+csrf.Value) {
		t.Errorf("POST with the Bridge's CSRF token: %d %q", ok.status, ok.body)
	}
	// Without the header, or with a different value, the client itself says no.
	if bad := ann.bridge.do("POST", "/bridge/enqueue", nil, nil); bad.status != 403 {
		t.Errorf("POST without X-CSRF-Token: %d, want 403 from the client", bad.status)
	}
	if bad := ann.bridge.do("POST", "/bridge/enqueue", nil, map[string]string{"X-CSRF-Token": strings.Repeat("0", 64)}); bad.status != 403 {
		t.Errorf("POST with a wrong token: %d, want 403", bad.status)
	}

	// A browser-supplied identity cannot get around the proxy: the forged one
	// is replaced, so the request is judged on the proxy's.
	forged := ann.bridge.get("/", map[string]string{"X-SanHolo-Identity": "forged.token"})
	if forged.status != 200 {
		t.Errorf("forged identity header: %d, want 200 (replaced by the proxy's)", forged.status)
	}

	// Defence in depth: if routing ever sent Ann's host to Bob's instance,
	// Bob's instance would refuse the identity (aud is Ann's).
	b.res.route(ann.tenant, addr(bobSrv))
	b.rebuild(t, b.res, nil) // drops the proxy's address cache
	ann.bridge.app = b.env.app
	if r := ann.bridge.get("/", nil); r.status != 403 {
		t.Errorf("Ann's identity at Bob's instance: %d, want 403", r.status)
	}
}

// A proxy signing with a key the instance does not know is refused by the
// client: the guard really checks the signature.
func TestEndToEndWrongKeyIsRefused(t *testing.T) {
	b := newBridgeEnv(t)
	ann := b.person(t, "ann@example.org")
	srv := guardedBridge(t, ann.tenant, testKey.Public().(ed25519.PublicKey))
	b.res.route(ann.tenant, addr(srv))
	b.enterOK(t, ann)
	if r := ann.bridge.get("/", nil); r.status != 200 {
		t.Fatalf("right key: %d", r.status)
	}

	b.key = ed25519.NewKeyFromSeed([]byte(strings.Repeat("w", 32)))
	b.rebuild(t, b.res, nil)
	ann.bridge.app = b.env.app
	if r := ann.bridge.get("/", nil); r.status != 401 {
		t.Errorf("wrong key: %d, want 401 from the client", r.status)
	}
}

// Two public keys during a rotation: tokens from either verify.
func TestEndToEndRotation(t *testing.T) {
	b := newBridgeEnv(t)
	ann := b.person(t, "ann@example.org")
	next := ed25519.NewKeyFromSeed([]byte(strings.Repeat("r", 32)))
	v := clientfake.New([]ed25519.PublicKey{testKey.Public().(ed25519.PublicKey), next.Public().(ed25519.PublicKey)}, ann.tenant, nil)
	srv := httptest.NewServer(clientfake.Guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") }), v))
	t.Cleanup(srv.Close)
	b.res.route(ann.tenant, addr(srv))
	b.enterOK(t, ann)
	for name, key := range map[string]ed25519.PrivateKey{"current": testKey, "next": next} {
		b.key = key
		b.rebuild(t, b.res, nil)
		ann.bridge.app = b.env.app
		if r := ann.bridge.get("/", nil); r.status != 200 {
			t.Errorf("%s key: %d", name, r.status)
		}
	}
}

// An identity expires: a captured header is worthless after its exp. The
// proxy signs exp two minutes ahead; the client refuses it once passed.
func TestEndToEndIdentityExpires(t *testing.T) {
	b := newBridgeEnv(t)
	ann := b.person(t, "ann@example.org")
	now := time.Now()
	var seenToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenToken = r.Header.Get("X-SanHolo-Identity")
	}))
	t.Cleanup(srv.Close)
	b.res.route(ann.tenant, addr(srv))
	b.enterOK(t, ann)
	ann.bridge.get("/", nil)
	if seenToken == "" {
		t.Fatal("no identity captured")
	}
	pub := testKey.Public().(ed25519.PublicKey)
	for name, tc := range map[string]struct {
		at   time.Time
		want clientfake.Verdict
	}{
		"now":              {now, clientfake.OK},
		"after one minute": {now.Add(time.Minute), clientfake.OK},
		"after 3 minutes":  {now.Add(3 * time.Minute), clientfake.Unauthorized},
	} {
		at := tc.at
		if got, _ := clientfake.New([]ed25519.PublicKey{pub}, ann.tenant, func() time.Time { return at }).Verify(seenToken); got != tc.want {
			t.Errorf("%s: verdict %d, want %d", name, got, tc.want)
		}
	}
}
