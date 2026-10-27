package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/lhaig/sanholo-cloud/internal/identity"
	"github.com/lhaig/sanholo-cloud/internal/provision"
)

var (
	epoch   = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	signKey = ed25519.NewKeyFromSeed([]byte(strings.Repeat("k", 32)))
)

const (
	sessionName = "__Host-sanholo_session"
	bridgeName  = "__Host-sanholo_bridge"
	publicHost  = "acme.sanholo.test"
)

// resolver is a scripted Resolver that counts its calls.
type resolver struct {
	mu    sync.Mutex
	addr  string
	err   error
	calls int
}

type scripted struct{ r *resolver }

func (s scripted) Upstream(_ context.Context, tenantID string) (string, error) {
	s.r.mu.Lock()
	defer s.r.mu.Unlock()
	s.r.calls++
	return s.r.addr, s.r.err
}

// seen is what the upstream received.
type seen struct {
	method, uri, host string
	header            http.Header
	body              []byte
}

type upstream struct {
	srv  *httptest.Server
	mu   sync.Mutex
	last seen
	n    int
}

func (u *upstream) get() seen {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.last
}

func newUpstream(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) *upstream {
	t.Helper()
	u := &upstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.last = seen{method: r.Method, uri: r.RequestURI, host: r.Host, header: r.Header.Clone(), body: b}
		u.n++
		u.mu.Unlock()
		if h != nil {
			h(w, r)
			return
		}
		_, _ = io.WriteString(w, "bridge")
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *upstream) addr() string { return strings.TrimPrefix(u.srv.URL, "http://") }

type rig struct {
	t   *testing.T
	p   *Proxy
	res *resolver
	url string
	now time.Time
	mu  sync.Mutex
}

func (r *rig) clock() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.now
}

func (r *rig) advance(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.now = r.now.Add(d)
}

// newRig serves a Proxy for tenant "acme" on a real listener (so streaming
// is observable and the peer address is real).
func newRig(t *testing.T, addr string, tweak func(*Config)) *rig {
	t.Helper()
	r := &rig{t: t, res: &resolver{addr: addr}, now: epoch}
	cfg := Config{
		Key: signKey, Resolver: scripted{r.res}, Scheme: "https", AppOrigin: "https://app.sanholo.test",
		Cookies: []string{sessionName, bridgeName}, Now: r.clock,
	}
	if tweak != nil {
		tweak(&cfg)
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.p = p
	app := fiber.New(fiber.Config{DisableStartupMessage: true, Immutable: true})
	app.All("/*", func(c *fiber.Ctx) error { return p.Handle(c, "acme") })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = app.Listener(ln) }()
	t.Cleanup(func() { _ = app.Shutdown() })
	r.url = "http://" + ln.Addr().String()
	return r
}

func (r *rig) do(method, path string, body io.Reader, hdr http.Header) (*http.Response, []byte) {
	r.t.Helper()
	req, err := http.NewRequest(method, r.url+path, body)
	if err != nil {
		r.t.Fatal(err)
	}
	req.Host = publicHost
	for k, v := range hdr {
		req.Header[k] = v
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: &http.Transport{DisableCompression: true}}
	resp, err := c.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

// verifyIdentity checks a token the way the client does, written out from
// the wire format and not through identity.Sign.
func verifyIdentity(t *testing.T, token, tenant string, now time.Time, pub ed25519.PublicKey) {
	t.Helper()
	payload, sigPart, ok := strings.Cut(token, ".")
	if !ok {
		t.Fatalf("identity %q has no signature", token)
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigPart)
	if err != nil || !ed25519.Verify(pub, []byte("sanholo-identity/v1."+payload), sig) {
		t.Fatalf("identity signature does not verify")
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Tenant, Aud string
		Exp         int64
	}
	if err := json.Unmarshal([]byte(strings.NewReplacer(`"tenant"`, `"Tenant"`, `"aud"`, `"Aud"`, `"exp"`, `"Exp"`).Replace(string(raw))), &c); err != nil {
		t.Fatal(err)
	}
	if c.Tenant != tenant || c.Aud != tenant {
		t.Errorf("identity claims %+v, want tenant and aud %q", c, tenant)
	}
	exp := time.Unix(c.Exp, 0)
	if !now.Before(exp) || exp.After(now.Add(5*time.Minute)) {
		t.Errorf("exp %v outside (%v, %v+5m]", exp, now, now)
	}
}

func TestEveryRequestCarriesAFreshSignedIdentity(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up.addr(), nil)
	pub := signKey.Public().(ed25519.PublicKey)

	r.do("GET", "/", nil, nil)
	first := up.get().header.Get(identity.Header)
	verifyIdentity(t, first, "acme", r.clock(), pub)

	r.advance(30 * time.Second)
	r.do("GET", "/", nil, nil)
	second := up.get().header.Get(identity.Header)
	verifyIdentity(t, second, "acme", r.clock(), pub)
	if first == second {
		t.Error("the second request reused the first request's identity")
	}
}

// A browser cannot present an identity of its own or any other header in
// the proxy's namespace, and cannot choose the forwarding headers.
func TestIncomingIdentityAndForwardingHeadersAreReplaced(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up.addr(), nil)
	forged, _ := identity.Sign(ed25519.NewKeyFromSeed([]byte(strings.Repeat("z", 32))), "evil", epoch)

	hdr := http.Header{}
	hdr.Set("X-SanHolo-Identity", forged)
	hdr.Add("x-sanholo-identity", "second copy")
	hdr.Set("X-SanHolo-Tenant", "evil")
	hdr.Set("X-Sanholo-Whatever", "1")
	hdr.Set("X-Forwarded-For", "6.6.6.6")
	hdr.Set("X-Forwarded-Proto", "gopher")
	hdr.Set("X-Forwarded-Host", "evil.example")
	hdr.Set("Forwarded", "for=6.6.6.6")
	hdr.Set("X-Real-IP", "6.6.6.6")
	hdr.Set("Authorization", "Bearer abc")
	hdr.Set("Accept-Language", "nl")
	r.do("GET", "/", nil, hdr)
	got := up.get().header

	ids := got.Values(identity.Header)
	if len(ids) != 1 || ids[0] == forged || ids[0] == "second copy" {
		t.Fatalf("identity headers = %q, want exactly one, signed by the proxy", ids)
	}
	verifyIdentity(t, ids[0], "acme", epoch, signKey.Public().(ed25519.PublicKey))
	for _, name := range []string{"X-Sanholo-Tenant", "X-Sanholo-Whatever", "Forwarded", "X-Real-Ip", "Authorization"} {
		if v := got.Values(name); len(v) != 0 {
			t.Errorf("%s reached the Bridge: %q", name, v)
		}
	}
	if v := got.Values("X-Forwarded-For"); len(v) != 1 || v[0] != "127.0.0.1" {
		t.Errorf("X-Forwarded-For = %q, want the peer 127.0.0.1 and nothing the browser sent", v)
	}
	if got.Get("X-Forwarded-Proto") != "https" || got.Get("X-Forwarded-Host") != publicHost {
		t.Errorf("proto/host = %q/%q", got.Get("X-Forwarded-Proto"), got.Get("X-Forwarded-Host"))
	}
	if got.Get("Accept-Language") != "nl" {
		t.Error("an ordinary header was dropped")
	}
	if up.get().host != publicHost {
		t.Errorf("upstream Host = %q, want the public host", up.get().host)
	}
}

func TestClientAddressBehindTheEdge(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up.addr(), func(c *Config) { c.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")} })
	hdr := http.Header{}
	hdr.Set("X-Forwarded-For", "1.1.1.1, 203.0.113.5")
	r.do("GET", "/", nil, hdr)
	if got := up.get().header.Get("X-Forwarded-For"); got != "203.0.113.5" {
		t.Errorf("X-Forwarded-For = %q, want the right-most untrusted entry", got)
	}
}

func TestHopByHopHeadersAreNotForwarded(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "X-Internal")
		w.Header().Set("X-Internal", "secret")
		w.Header().Set("Keep-Alive", "timeout=5")
		w.Header().Set("X-Fine", "yes")
	})
	r := newRig(t, up.addr(), nil)
	hdr := http.Header{}
	hdr.Set("Connection", "X-Hop, keep-alive")
	hdr.Set("X-Hop", "1")
	hdr.Set("Upgrade", "websocket")
	hdr.Set("Te", "trailers")
	resp, _ := r.do("GET", "/", nil, hdr)
	got := up.get().header
	for _, name := range []string{"X-Hop", "Upgrade", "Te"} {
		if got.Get(name) != "" {
			t.Errorf("request hop-by-hop %s reached the Bridge", name)
		}
	}
	if resp.Header.Get("X-Internal") != "" || resp.Header.Get("Keep-Alive") != "" {
		t.Errorf("response hop-by-hop headers reached the browser: %v", resp.Header)
	}
	if resp.Header.Get("X-Fine") != "yes" {
		t.Error("an ordinary response header was dropped")
	}
}

// The control plane's cookies never reach a Bridge; the Bridge's own CSRF
// cookie and header do, untouched, and the Bridge cannot overwrite ours.
func TestCookiesAndCSRF(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "__Host-sanholo_csrf", Value: "bridge-csrf", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		http.SetCookie(w, &http.Cookie{Name: "lang", Value: "nl", Path: "/"})
		http.SetCookie(w, &http.Cookie{Name: bridgeName, Value: "planted", Path: "/", Secure: true})
		http.SetCookie(w, &http.Cookie{Name: sessionName, Value: "planted", Path: "/", Secure: true})
	})
	r := newRig(t, up.addr(), nil)
	hdr := http.Header{}
	hdr.Set("Cookie", sessionName+"=ACCOUNT-SECRET; __Host-sanholo_csrf=tok123; "+bridgeName+"=BRIDGE-SECRET; lang=en")
	hdr.Set("X-CSRF-Token", "tok123")
	resp, _ := r.do("POST", "/orders", strings.NewReader("a=b"), hdr)

	got := up.get()
	if c := got.header.Get("Cookie"); c != "__Host-sanholo_csrf=tok123; lang=en" {
		t.Errorf("Cookie to the Bridge = %q", c)
	}
	if strings.Contains(fmt.Sprint(got.header), "ACCOUNT-SECRET") || strings.Contains(fmt.Sprint(got.header), "BRIDGE-SECRET") {
		t.Errorf("a control plane secret reached the Bridge: %v", got.header)
	}
	if got.header.Get("X-CSRF-Token") != "tok123" {
		t.Errorf("X-CSRF-Token = %q, want it passed through", got.header.Get("X-CSRF-Token"))
	}
	names := map[string]string{}
	for _, c := range resp.Cookies() {
		names[c.Name] = c.Value
	}
	if names["__Host-sanholo_csrf"] != "bridge-csrf" || names["lang"] != "nl" {
		t.Errorf("the Bridge's cookies were not passed: %v", names)
	}
	if _, ok := names[bridgeName]; ok {
		t.Error("the Bridge set the Bridge-session cookie")
	}
	if _, ok := names[sessionName]; ok {
		t.Error("the Bridge set the account-session cookie")
	}
	for _, c := range resp.Cookies() {
		if c.Name == "__Host-sanholo_csrf" && (!c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Domain != "") {
			t.Errorf("the Bridge's CSRF cookie attributes changed: %+v", c)
		}
	}
}

func TestMethodsBodiesAndQueryStringsSurvive(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) { w.Header().Set("X-Method", r.Method) })
	r := newRig(t, up.addr(), nil)
	for _, m := range []string{"GET", "POST", "PUT", "PATCH", "DELETE"} {
		body := ""
		if m != "GET" && m != "DELETE" {
			body = `{"k":"v"}`
		}
		var rd io.Reader
		hdr := http.Header{}
		if body != "" {
			rd = strings.NewReader(body)
			hdr.Set("Content-Type", "application/json")
		}
		resp, _ := r.do(m, "/bridge/sector.json?a=b%26c&d&q=%E2%9C%93", rd, hdr)
		got := up.get()
		if got.method != m || resp.Header.Get("X-Method") != m {
			t.Errorf("%s arrived as %s", m, got.method)
		}
		if got.uri != "/bridge/sector.json?a=b%26c&d&q=%E2%9C%93" {
			t.Errorf("%s: request URI = %q", m, got.uri)
		}
		if string(got.body) != body {
			t.Errorf("%s: body = %q, want %q", m, got.body, body)
		}
		if body != "" && got.header.Get("Content-Type") != "application/json" {
			t.Errorf("%s: Content-Type = %q", m, got.header.Get("Content-Type"))
		}
	}
	// An escaped slash stays escaped.
	r.do("GET", "/bridge/system/A%2FB", nil, nil)
	if got := up.get().uri; got != "/bridge/system/A%2FB" {
		t.Errorf("escaped path became %q", got)
	}
}

func TestResponseSecurityHeadersReplaceTheBridges(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src *")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		h.Set("Referrer-Policy", "unsafe-url")
		h.Set("Strict-Transport-Security", "max-age=1")
		h.Set("X-Content-Type-Options", "sniff")
		h.Set("Cache-Control", "no-cache")
		h.Set("Content-Type", "text/html")
	})
	r := newRig(t, up.addr(), nil)
	resp, _ := r.do("GET", "/", nil, nil)
	for name, want := range map[string]string{
		"X-Frame-Options": "DENY", "Referrer-Policy": "same-origin", "X-Content-Type-Options": "nosniff",
		"Strict-Transport-Security": "max-age=31536000", "Content-Security-Policy": csp,
	} {
		if v := resp.Header.Values(name); len(v) != 1 || v[0] != want {
			t.Errorf("%s = %q, want exactly %q", name, v, want)
		}
	}
	if resp.Header.Get("Cache-Control") != "no-cache" {
		t.Error("the Bridge's Cache-Control was not passed")
	}
}

func TestCSPPolicyIsSelfOnlyExceptInlineForTheBridge(t *testing.T) {
	for _, bad := range []string{"unsafe-eval", "http:", "https:", "*", "blob:"} {
		if strings.Contains(csp, bad) {
			t.Errorf("CSP contains %q", bad)
		}
	}
	for _, want := range []string{"default-src 'none'", "frame-ancestors 'none'", "base-uri 'none'", "object-src 'none'", "form-action 'self'", "connect-src 'self'", "worker-src 'self'", "font-src 'self'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP lacks %q", want)
		}
	}
}

func TestRedirectsAndStatusesPassThrough(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/go":
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		case "/teapot":
			w.WriteHeader(418)
			_, _ = io.WriteString(w, "short and stout")
		case "/nocontent":
			w.WriteHeader(204)
		case "/head":
			w.Header().Set("Content-Length", "1234")
		}
	})
	r := newRig(t, up.addr(), nil)
	if resp, _ := r.do("GET", "/go", nil, nil); resp.StatusCode != 302 || resp.Header.Get("Location") != "/elsewhere" {
		t.Errorf("redirect: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp, b := r.do("GET", "/teapot", nil, nil); resp.StatusCode != 418 || string(b) != "short and stout" {
		t.Errorf("418: %d %q", resp.StatusCode, b)
	}
	if resp, b := r.do("GET", "/nocontent", nil, nil); resp.StatusCode != 204 || len(b) != 0 {
		t.Errorf("204: %d %q", resp.StatusCode, b)
	}
	if resp, b := r.do("HEAD", "/head", nil, nil); resp.StatusCode != 200 || len(b) != 0 || resp.ContentLength != 1234 {
		t.Errorf("HEAD: %d len=%d cl=%d", resp.StatusCode, len(b), resp.ContentLength)
	}
}

func TestEncodedAndLargeBodiesPassUntouched(t *testing.T) {
	var zipped bytes.Buffer
	zw := gzip.NewWriter(&zipped)
	_, _ = zw.Write([]byte(strings.Repeat(`{"stars":[1,2,3]}`, 200)))
	_ = zw.Close()
	big := bytes.Repeat([]byte("0123456789abcdef"), 64*1024) // 1 MiB
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/big" {
			w.Header().Set("Content-Length", fmt.Sprint(len(big)))
			_, _ = w.Write(big)
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(zipped.Bytes())
	})
	r := newRig(t, up.addr(), nil)
	hdr := http.Header{}
	hdr.Set("Accept-Encoding", "gzip")
	resp, b := r.do("GET", "/bridge/map.json", nil, hdr)
	if resp.Header.Get("Content-Encoding") != "gzip" || !bytes.Equal(b, zipped.Bytes()) {
		t.Errorf("gzip body changed: encoding=%q len=%d want %d", resp.Header.Get("Content-Encoding"), len(b), zipped.Len())
	}
	if up.get().header.Get("Accept-Encoding") != "gzip" {
		t.Error("Accept-Encoding was not passed")
	}
	if _, b := r.do("GET", "/big", nil, nil); !bytes.Equal(b, big) {
		t.Errorf("1 MiB body corrupted: %d bytes", len(b))
	}
}

// A response without a length is sent as it arrives: the browser reads the
// first chunk while the Bridge is still holding the response open.
func TestResponsesStream(t *testing.T) {
	release := make(chan struct{})
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-time.After(10 * time.Second):
		}
		_, _ = io.WriteString(w, "data: two\n\n")
	})
	r := newRig(t, up.addr(), nil)
	req, _ := http.NewRequest("GET", r.url+"/events", nil)
	req.Host = publicHost
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	first := make(chan string, 1)
	go func() {
		buf := make([]byte, 64)
		n, _ := resp.Body.Read(buf)
		first <- string(buf[:n])
	}()
	select {
	case got := <-first:
		if !strings.Contains(got, "data: one") {
			t.Errorf("first chunk = %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the first chunk did not arrive while the Bridge held the response open: the proxy buffers")
	}
	close(release)
	rest, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(rest), "data: two") {
		t.Errorf("rest = %q", rest)
	}
}

func TestBotNotReachablePages(t *testing.T) {
	up := newUpstream(t, nil)
	tests := []struct {
		name  string
		err   error
		want  string
		retry bool
	}{
		{"starting", provision.ErrStarting, "Your bot is starting", true},
		{"not running", provision.ErrNotRunning, "Your bot is not running", false},
		{"nomad unreadable", errors.New("nomad: dial tcp 10.1.2.3:4646: SECRET-DETAIL"), "cannot be reached right now", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, up.addr(), nil)
			r.res.err = tc.err
			before := up.n
			resp, b := r.do("GET", "/", nil, nil)
			if resp.StatusCode != 503 || !strings.Contains(string(b), tc.want) {
				t.Fatalf("%d %s", resp.StatusCode, b)
			}
			if (resp.Header.Get("Retry-After") != "") != tc.retry {
				t.Errorf("Retry-After = %q", resp.Header.Get("Retry-After"))
			}
			if up.n != before {
				t.Error("a request reached the Bridge")
			}
			for _, leak := range []string{"SECRET-DETAIL", "10.1.2.3", "4646", "Bad Gateway", "dial tcp"} {
				if strings.Contains(string(b), leak) {
					t.Errorf("page leaks %q", leak)
				}
			}
			if !strings.Contains(string(b), "https://app.sanholo.test/account") {
				t.Error("the page does not link to the account")
			}
			if resp.Header.Get("Content-Security-Policy") != csp || resp.Header.Get("Cache-Control") != "no-store" {
				t.Error("the page lacks the security headers")
			}
		})
	}
}

func TestAddressIsCachedAndRefreshedOnConnectionFailure(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up.addr(), nil)

	r.do("GET", "/", nil, nil)
	r.do("GET", "/", nil, nil)
	if r.res.calls != 1 {
		t.Errorf("resolver calls = %d after two requests inside the cache window, want 1", r.res.calls)
	}
	r.advance(11 * time.Second)
	r.do("GET", "/", nil, nil)
	if r.res.calls != 2 {
		t.Errorf("resolver calls = %d after the cache expired, want 2", r.res.calls)
	}

	// The allocation moves: the cached address now refuses connections.
	dead := httptest.NewServer(nil)
	deadAddr := strings.TrimPrefix(dead.URL, "http://")
	dead.Close()
	r.res.mu.Lock()
	r.res.addr = deadAddr
	r.res.mu.Unlock()
	r.advance(11 * time.Second)
	resp, b := r.do("GET", "/", nil, nil)
	if resp.StatusCode != 503 || !strings.Contains(string(b), "starting") || strings.Contains(string(b), deadAddr) {
		t.Fatalf("dead upstream: %d %s", resp.StatusCode, b)
	}
	// Nomad now reports the new address; the very next request must ask again
	// rather than wait out the cache window.
	r.res.mu.Lock()
	r.res.addr = up.addr()
	r.res.mu.Unlock()
	callsBefore := r.res.calls
	if resp, b := r.do("GET", "/", nil, nil); resp.StatusCode != 200 || string(b) != "bridge" {
		t.Fatalf("after the allocation moved: %d %s", resp.StatusCode, b)
	}
	if r.res.calls != callsBefore+1 {
		t.Errorf("the failed address stayed cached (resolver calls %d -> %d)", callsBefore, r.res.calls)
	}
}

// countingTransport records every dial the proxy attempts.
type countingTransport struct {
	mu    sync.Mutex
	hosts []string
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.hosts = append(c.hosts, r.URL.Host)
	c.mu.Unlock()
	return nil, errors.New("not dialled in this test")
}

func TestUpstreamAddressIsValidated(t *testing.T) {
	for _, bad := range []string{"", "nohost", ":8080", "10.0.0.1:0", "10.0.0.1:99999", "10.0.0.1:abc"} {
		ct := &countingTransport{}
		r := newRig(t, bad, func(c *Config) { c.Transport = ct })
		resp, _ := r.do("GET", "/", nil, nil)
		if resp.StatusCode != 503 {
			t.Errorf("address %q: status %d, want 503", bad, resp.StatusCode)
		}
		if len(ct.hosts) != 0 {
			t.Errorf("address %q was dialled: %v", bad, ct.hosts)
		}
	}
	ct := &countingTransport{}
	r := newRig(t, "10.0.0.7:23456", func(c *Config) { c.Transport = ct })
	r.do("GET", "/", nil, nil)
	if len(ct.hosts) != 1 || ct.hosts[0] != "10.0.0.7:23456" {
		t.Errorf("a valid address was not used: %v", ct.hosts)
	}
}

// The request body goes through as the browser sent it, still encoded.
func TestEncodedRequestBodyIsForwardedRaw(t *testing.T) {
	up := newUpstream(t, nil)
	r := newRig(t, up.addr(), nil)
	var zipped bytes.Buffer
	zw := gzip.NewWriter(&zipped)
	_, _ = zw.Write([]byte(`{"order":"mine"}`))
	_ = zw.Close()
	hdr := http.Header{}
	hdr.Set("Content-Encoding", "gzip")
	hdr.Set("Content-Type", "application/json")
	r.do("POST", "/bridge/orders", bytes.NewReader(zipped.Bytes()), hdr)
	got := up.get()
	if !bytes.Equal(got.body, zipped.Bytes()) || got.header.Get("Content-Encoding") != "gzip" {
		t.Errorf("body %q (encoding %q) was altered; want the %d raw bytes", got.body, got.header.Get("Content-Encoding"), zipped.Len())
	}
}

func TestNewRefusesMissingPieces(t *testing.T) {
	if _, err := New(Config{Resolver: scripted{&resolver{}}}); err == nil {
		t.Error("New without a key")
	}
	if _, err := New(Config{Key: signKey}); err == nil {
		t.Error("New without a resolver")
	}
}
