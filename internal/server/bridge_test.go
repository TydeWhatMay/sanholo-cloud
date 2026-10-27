package server

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lhaig/sanholo-cloud/internal/auth"
	"github.com/lhaig/sanholo-cloud/internal/nomad/nomadfake"
	"github.com/lhaig/sanholo-cloud/internal/provision"
	"github.com/lhaig/sanholo-cloud/internal/proxy"
)

const (
	testDomain = "sanholo.test"
	appHost    = "app.sanholo.test"
	appOrigin  = "https://app.sanholo.test"
)

var testKey = ed25519.NewKeyFromSeed([]byte(strings.Repeat("p", 32)))

// recorded is one request the fake Bridge received.
type recorded struct {
	method, uri, host string
	header            http.Header
}

type bridgeEnv struct {
	*env
	// key signs the proxy's identities (testKey unless a test changes it
	// before calling rebuild).
	key ed25519.PrivateKey
	// extra, when set, adjusts the App that rebuild mounts.
	extra func(*App)
	up    *httptest.Server
	res   *fakeResolver
	mu    sync.Mutex
	reqs  []recorded
}

type fakeResolver struct {
	mu   sync.Mutex
	addr string
	// byTenant overrides addr for the tenants it names.
	byTenant map[string]string
	err      error
}

func (f *fakeResolver) Upstream(_ context.Context, tenantID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a, ok := f.byTenant[tenantID]; ok {
		return a, f.err
	}
	return f.addr, f.err
}

func (f *fakeResolver) route(tenantID, addr string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.byTenant == nil {
		f.byTenant = map[string]string{}
	}
	f.byTenant[tenantID] = addr
}

func (f *fakeResolver) set(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (b *bridgeEnv) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.reqs)
}

func (b *bridgeEnv) last() recorded {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.reqs) == 0 {
		panic("no request reached the Bridge")
	}
	return b.reqs[len(b.reqs)-1]
}

// newBridgeEnv mounts an app that routes hosts and proxies to a fake Bridge.
func newBridgeEnv(t *testing.T) *bridgeEnv {
	t.Helper()
	// app.Test connections report the peer 0.0.0.0.
	e := newEnv(t, auth.Options{}, netip.MustParsePrefix("0.0.0.0/32"))
	b := &bridgeEnv{env: e, res: &fakeResolver{}, key: testKey}
	b.up = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		b.mu.Lock()
		b.reqs = append(b.reqs, recorded{method: r.Method, uri: r.RequestURI, host: r.Host, header: r.Header.Clone()})
		b.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "__Host-sanholo_csrf", Value: "bridge-csrf", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		w.Header().Set("Content-Security-Policy", "default-src *")
		_, _ = io.WriteString(w, "the bridge")
	}))
	t.Cleanup(b.up.Close)
	b.res.addr = strings.TrimPrefix(b.up.URL, "http://")
	b.rebuild(t, b.res, nil)
	return b
}

func (b *bridgeEnv) rebuild(t *testing.T, res proxy.Resolver, prov *provision.Service) {
	t.Helper()
	px, err := proxy.New(proxy.Config{
		Key: b.key, Resolver: res, Scheme: "https", AppOrigin: appOrigin,
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/32")}, Cookies: ControlPlaneCookies(),
	})
	if err != nil {
		t.Fatal(err)
	}
	app := &App{
		Auth: b.svc, Admin: b.admin, Provision: prov, Proxy: px, AppBaseURL: appOrigin, TenantDomain: testDomain,
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/32")},
	}
	if b.extra != nil {
		b.extra(app)
	}
	b.env.app = New(b.pool, WithApp(app))
	b.env.host = appHost
}

// person is a signed-in app client for a new account, with a client for the
// tenant's Bridge host (a separate cookie jar, as a browser keeps).
type person struct {
	app, bridge *client
	tenant      string
	host        string
}

func (b *bridgeEnv) person(t *testing.T, email string) person {
	t.Helper()
	b.account(t, email)
	app := b.signedIn(t, email)
	var tenant string
	if err := b.pool.QueryRow(context.Background(), `SELECT tenant_id FROM accounts WHERE lower(email) = lower($1)`, email).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	br := b.client(t)
	br.host = tenant + "." + testDomain
	return person{app: app, bridge: br, tenant: tenant, host: br.host}
}

// enter does what a browser does for "Open my Bridge": presses the button,
// follows the redirect to the tenant host.
func (b *bridgeEnv) enter(t *testing.T, p person) result {
	t.Helper()
	r := p.app.post("/account/bridge/open", nil)
	if r.status != 303 {
		t.Fatalf("open: %d %s", r.status, r.body)
	}
	loc, err := url.Parse(r.header.Get("Location"))
	if err != nil || loc.Scheme != "https" || loc.Host != p.host || loc.Path != "/auth" || loc.Query().Get("code") == "" {
		t.Fatalf("open redirected to %q, want https://%s/auth?code=...", r.header.Get("Location"), p.host)
	}
	return p.bridge.get(loc.RequestURI(), nil)
}

func (b *bridgeEnv) enterOK(t *testing.T, p person) {
	t.Helper()
	if r := b.enter(t, p); r.status != 303 || r.header.Get("Location") != "/" {
		t.Fatalf("auth: %d %q %s", r.status, r.header.Get("Location"), r.body)
	}
}

func cookie(r result, name string) *http.Cookie {
	for _, c := range r.cookies {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func TestHostRouting(t *testing.T) {
	b := newBridgeEnv(t)
	app := b.client(t)

	// The app host is the control plane.
	if r := app.get("/", nil); r.status != 200 || !strings.Contains(r.body, "Waiting list") {
		t.Errorf("app host /: %d", r.status)
	}
	if r := app.get("/auth?code=x", nil); r.status != 404 {
		t.Errorf("/auth on the app host: %d, want 404", r.status)
	}
	for _, h := range []string{"app.sanholo.test:443", "APP.SANHOLO.TEST", "app.sanholo.test."} {
		c := b.client(t)
		c.host = h
		if r := c.get("/", nil); r.status != 200 {
			t.Errorf("app host spelled %q: %d", h, r.status)
		}
	}

	// Every other host is the same plain 404, with nothing set on the way.
	var want string
	for _, h := range []string{
		"example.com", "sanholo.test", "evilsanholo.test", "sanholo.test.evil.example", "a.b.sanholo.test",
		"-bad.sanholo.test", "bad-.sanholo.test", strings.Repeat("a", 64) + ".sanholo.test", "x_y.sanholo.test", ".sanholo.test", "127.0.0.1", "[::1]",
	} {
		c := b.client(t)
		c.host = h
		r := c.get("/", nil)
		if r.status != 404 || r.body != "not found" {
			t.Errorf("host %q: %d %q, want 404 not found", h, r.status, r.body)
		}
		if len(r.cookies) != 0 {
			t.Errorf("host %q: cookies set: %v", h, r.cookies)
		}
		if want == "" {
			want = r.body
		}
	}
	if b.count() != 0 {
		t.Error("an unroutable host reached a Bridge")
	}

	// A valid tenant id with no session is sent to sign in, whether or not
	// the tenant exists: the answer reveals nothing about tenants.
	p := b.person(t, "ann@example.org")
	ghost := b.client(t)
	ghost.host = "nosuchtenant." + testDomain
	rg, rr := ghost.get("/", nil), p.bridge.get("/", nil)
	if rg.status != 303 || rr.status != 303 || rg.header.Get("Location") != rr.header.Get("Location") || rg.header.Get("Location") != appOrigin+"/signin" {
		t.Errorf("no session: unknown tenant %d %q, real tenant %d %q", rg.status, rg.header.Get("Location"), rr.status, rr.header.Get("Location"))
	}

	// The probe endpoints answer on any host, for the platform's checks.
	probe := b.client(t)
	probe.host = "10.1.2.3"
	if r := probe.get("/healthz", nil); r.status != 200 || r.body != "ok" {
		t.Errorf("/healthz by address: %d %q", r.status, r.body)
	}
}

func TestOpenMyBridgeHandoffAndProxy(t *testing.T) {
	b := newBridgeEnv(t)
	p := b.person(t, "ann@example.org")

	r := p.app.post("/account/bridge/open", nil)
	if r.status != 303 {
		t.Fatalf("open: %d %s", r.status, r.body)
	}
	loc, _ := url.Parse(r.header.Get("Location"))
	code := loc.Query().Get("code")
	if code == "" || len(code) < 40 {
		t.Fatalf("handoff code %q", code)
	}
	if r.header.Get("Referrer-Policy") != "no-referrer" {
		t.Error("the redirect carrying the code can leak it through the referrer")
	}
	// Stored hashed.
	if rows(t, b.pool, `SELECT count(*) FROM bridge_handoffs WHERE code_hash = $1`, code) != 0 {
		t.Error("the plain code is stored")
	}

	// The tenant host swaps it for the Bridge cookie.
	r = p.bridge.get(loc.RequestURI(), nil)
	if r.status != 303 || r.header.Get("Location") != "/" {
		t.Fatalf("auth: %d %q", r.status, r.header.Get("Location"))
	}
	ck := cookie(r, bridgeCookie)
	if ck == nil || !ck.Secure || !ck.HttpOnly || ck.Path != "/" || ck.Domain != "" || ck.SameSite != http.SameSiteLaxMode || ck.Value == "" {
		t.Fatalf("bridge cookie = %+v", ck)
	}
	if !strings.HasPrefix(ck.Name, "__Host-") {
		t.Error("the Bridge cookie lacks the __Host- prefix")
	}
	if ck.MaxAge <= 0 || ck.MaxAge > int(auth.BridgeSessionMax.Seconds()) {
		t.Errorf("Bridge cookie MaxAge = %d", ck.MaxAge)
	}
	if strings.Contains(r.body, code) || strings.Contains(r.header.Get("Location"), code) {
		t.Error("the code is echoed")
	}
	if cookie(r, csrfCookie) != nil {
		// The control plane's CSRF cookie is for the app host only.
		t.Error("the control plane's CSRF cookie was set on the tenant host")
	}

	// Single use: replaying the same URL fails.
	if r2 := b.client(t); true {
		r2.host = p.host
		if got := r2.get(loc.RequestURI(), nil); got.status != 410 {
			t.Errorf("replayed code: %d, want 410", got.status)
		}
	}

	// With the cookie the Bridge is reached, signed for this tenant.
	r = p.bridge.get("/bridge/map.json?x=1", map[string]string{"X-SanHolo-Identity": "forged", "Accept": "application/json"})
	if r.status != 200 || r.body != "the bridge" {
		t.Fatalf("proxied: %d %q", r.status, r.body)
	}
	got := b.last()
	if got.uri != "/bridge/map.json?x=1" || got.host != p.host {
		t.Errorf("upstream saw %s Host %s", got.uri, got.host)
	}
	verify(t, got.header.Get("X-SanHolo-Identity"), p.tenant)
	if got.header.Values("X-SanHolo-Identity")[0] == "forged" || len(got.header.Values("X-SanHolo-Identity")) != 1 {
		t.Error("the forged identity reached the Bridge")
	}
	if r.header.Get("Content-Security-Policy") == "default-src *" || !strings.Contains(r.header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Errorf("CSP = %q", r.header.Get("Content-Security-Policy"))
	}
}

// verify checks the identity header against the proxy's public key, written
// from the client's wire format.
func verify(t *testing.T, token, tenant string) {
	t.Helper()
	payload, sigPart, ok := strings.Cut(token, ".")
	if !ok {
		t.Fatalf("identity %q", token)
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigPart)
	if err != nil || !ed25519.Verify(testKey.Public().(ed25519.PublicKey), []byte("sanholo-identity/v1."+payload), sig) {
		t.Fatal("identity does not verify against the proxy's public key")
	}
	raw, _ := base64.RawURLEncoding.DecodeString(payload)
	if !strings.Contains(string(raw), `"tenant":"`+tenant+`"`) || !strings.Contains(string(raw), `"aud":"`+tenant+`"`) {
		t.Errorf("claims %s are not for tenant %s", raw, tenant)
	}
}

// The account session never reaches a Bridge, and a Bridge session is not an
// account session.
func TestTheTwoSessionCookiesStayApart(t *testing.T) {
	b := newBridgeEnv(t)
	p := b.person(t, "ann@example.org")
	b.enterOK(t, p)

	accountSecret := p.app.cookies[sessionCookie].Value
	bridgeSecret := p.bridge.cookies[bridgeCookie].Value
	if accountSecret == bridgeSecret || accountSecret == "" {
		t.Fatal("setup: the cookies must differ")
	}
	// The browser jars are separate: the app jar has no Bridge cookie and the
	// Bridge jar has no account cookie.
	if p.app.cookies[bridgeCookie] != nil || p.bridge.cookies[sessionCookie] != nil {
		t.Fatal("a cookie crossed hosts in the test jars")
	}

	// Even a request that carries both cookies to the Bridge host delivers
	// neither, and keeps the Bridge's own cookies.
	req := httptest.NewRequest("POST", "/orders", strings.NewReader("a=b"))
	req.Host = p.host
	req.Header.Set("Cookie", sessionCookie+"="+accountSecret+"; "+bridgeCookie+"="+bridgeSecret+"; __Host-sanholo_csrf=c; lang=nl")
	req.Header.Set("X-CSRF-Token", "c")
	r := p.bridge.send(req)
	if r.status != 200 {
		t.Fatalf("both cookies: %d %s", r.status, r.body)
	}
	got := b.last()
	all := strings.Join(got.header.Values("Cookie"), "|")
	if strings.Contains(all, accountSecret) || strings.Contains(all, bridgeSecret) || strings.Contains(all, sessionCookie) || strings.Contains(all, bridgeCookie) {
		t.Errorf("a control plane cookie reached the Bridge: %q", all)
	}
	if all != "__Host-sanholo_csrf=c; lang=nl" {
		t.Errorf("Cookie to the Bridge = %q", all)
	}
	if got.header.Get("X-CSRF-Token") != "c" {
		t.Error("X-CSRF-Token was not passed through")
	}

	// The account session cookie alone is not a Bridge session.
	only := b.client(t)
	only.host = p.host
	only.cookies[sessionCookie] = p.app.cookies[sessionCookie]
	before := b.count()
	if r := only.get("/", nil); r.status != 303 || r.header.Get("Location") != appOrigin+"/signin" {
		t.Errorf("account cookie on the Bridge host: %d %q", r.status, r.header.Get("Location"))
	}
	if b.count() != before {
		t.Error("an account session reached the Bridge")
	}
	// And the Bridge cookie alone is not an account session.
	app := b.client(t)
	app.cookies[bridgeCookie] = p.bridge.cookies[bridgeCookie]
	if r := app.get("/account", nil); r.status != 303 || r.header.Get("Location") != "/signin" {
		t.Errorf("Bridge cookie on the app host: %d %q", r.status, r.header.Get("Location"))
	}
}

// The Bridge's own CSRF scheme is untouched: the control plane's CSRF check
// does not run on a tenant host, and the Bridge's cookie reaches the browser.
func TestBridgeCSRFIsThePassThrough(t *testing.T) {
	b := newBridgeEnv(t)
	p := b.person(t, "ann@example.org")
	b.enterOK(t, p)

	// A POST with no control plane CSRF token at all goes through.
	r := p.bridge.do("POST", "/bridge/enqueue", url.Values{"x": {"1"}}, map[string]string{"X-CSRF-Token": "tok"})
	if r.status != 200 {
		t.Fatalf("POST: %d %s", r.status, r.body)
	}
	if b.last().header.Get("X-CSRF-Token") != "tok" || b.last().method != "POST" {
		t.Errorf("upstream saw %v", b.last().header)
	}
	if c := cookie(r, csrfCookie); c == nil || c.Value != "bridge-csrf" || c.SameSite != http.SameSiteStrictMode || !c.HttpOnly || !c.Secure {
		t.Errorf("the Bridge's CSRF cookie did not arrive intact: %+v", c)
	}
	// The same POST to the app host without the token is still refused.
	if r := p.app.do("POST", "/account/bridge/open", url.Values{}, nil); r.status != 403 {
		t.Errorf("app POST without CSRF: %d, want 403", r.status)
	}
}

func TestAuthIsSafeAgainstPrefetchAndHead(t *testing.T) {
	b := newBridgeEnv(t)
	p := b.person(t, "ann@example.org")
	r := p.app.post("/account/bridge/open", nil)
	loc, _ := url.Parse(r.header.Get("Location"))
	target := loc.RequestURI()

	probes := []struct {
		name, method string
		hdr          map[string]string
	}{
		{"HEAD", "HEAD", nil},
		{"Sec-Purpose prefetch", "GET", map[string]string{"Sec-Purpose": "prefetch"}},
		{"Sec-Purpose prefetch;anonymous-client-ip", "GET", map[string]string{"Sec-Purpose": "prefetch;anonymous-client-ip"}},
		{"Purpose prefetch", "GET", map[string]string{"Purpose": "prefetch"}},
	}
	for _, pr := range probes {
		got := p.bridge.do(pr.method, target, nil, pr.hdr)
		if got.status != 204 || len(got.cookies) != 0 {
			t.Errorf("%s: %d, cookies %v; want 204 and nothing set", pr.name, got.status, got.cookies)
		}
	}
	if rows(t, b.pool, `SELECT count(*) FROM bridge_sessions`) != 0 || rows(t, b.pool, `SELECT count(*) FROM bridge_handoffs WHERE used_at IS NOT NULL`) != 0 {
		t.Fatal("a HEAD or prefetch spent the code")
	}
	// The real visit still works after all of them.
	if got := p.bridge.get(target, nil); got.status != 303 || cookie(got, bridgeCookie) == nil {
		t.Errorf("real GET after the probes: %d", got.status)
	}
	// Other methods are refused.
	if got := p.bridge.do("POST", target, url.Values{}, nil); got.status != 405 {
		t.Errorf("POST /auth: %d, want 405", got.status)
	}
}

func TestAuthRefusals(t *testing.T) {
	b := newBridgeEnv(t)
	p := b.person(t, "ann@example.org")
	other := b.person(t, "bob@example.org")

	r := p.app.post("/account/bridge/open", nil)
	loc, _ := url.Parse(r.header.Get("Location"))
	code := loc.Query().Get("code")

	for name, tc := range map[string]struct {
		c    *client
		path string
	}{
		"no code":                {p.bridge, "/auth"},
		"empty code":             {p.bridge, "/auth?code="},
		"garbage":                {p.bridge, "/auth?code=nonsense"},
		"code on another tenant": {other.bridge, "/auth?code=" + url.QueryEscape(code)},
	} {
		got := tc.c.get(tc.path, nil)
		if got.status != 410 || cookie(got, bridgeCookie) != nil {
			t.Errorf("%s: %d, cookie %v", name, got.status, cookie(got, bridgeCookie))
		}
		if strings.Contains(got.body, code) {
			t.Errorf("%s: the page echoes the code", name)
		}
	}
	// The failed attempt on the other tenant did not spend the code.
	if got := p.bridge.get("/auth?code="+url.QueryEscape(code), nil); got.status != 303 {
		t.Errorf("the right tenant after a wrong-tenant attempt: %d", got.status)
	}
	if b.count() != 0 {
		t.Error("a refused /auth reached a Bridge")
	}
}

func TestAuthExpiredCode(t *testing.T) {
	b := newBridgeEnv(t)
	p := b.person(t, "ann@example.org")
	r := p.app.post("/account/bridge/open", nil)
	loc, _ := url.Parse(r.header.Get("Location"))
	if _, err := b.pool.Exec(context.Background(), `UPDATE bridge_handoffs SET expires_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if got := p.bridge.get(loc.RequestURI(), nil); got.status != 410 {
		t.Errorf("expired code: %d, want 410", got.status)
	}
}

func TestAuthRateLimitPerIP(t *testing.T) {
	b := newBridgeEnv(t)
	p := b.person(t, "ann@example.org")
	hit := func(xff string) int {
		return p.bridge.get("/auth?code=nonsense", map[string]string{"X-Forwarded-For": xff}).status
	}
	for i := range bridgeAuthLimit {
		if s := hit("198.51.100.1"); s != 410 {
			t.Fatalf("request %d: %d, want 410", i+1, s)
		}
	}
	if s := hit("198.51.100.1"); s != 429 {
		t.Fatalf("over the limit: %d, want 429", s)
	}
	if s := hit("198.51.100.2"); s != 410 {
		t.Fatalf("another client behind the same edge: %d, want 410", s)
	}
	// Even a valid code is refused while over the limit.
	r := p.app.post("/account/bridge/open", nil)
	loc, _ := url.Parse(r.header.Get("Location"))
	if got := p.bridge.get(loc.RequestURI(), map[string]string{"X-Forwarded-For": "198.51.100.1"}); got.status != 429 {
		t.Errorf("valid code over the limit: %d, want 429", got.status)
	}
	if got := p.bridge.get(loc.RequestURI(), map[string]string{"X-Forwarded-For": "198.51.100.3"}); got.status != 303 {
		t.Errorf("valid code from a fresh client: %d, want 303 (the limited attempt must not spend it)", got.status)
	}
}

func TestNoSessionRedirects(t *testing.T) {
	b := newBridgeEnv(t)
	p := b.person(t, "ann@example.org")

	r := p.bridge.get("/anything?a=b", nil)
	if r.status != 303 || r.header.Get("Location") != appOrigin+"/signin" {
		t.Errorf("no cookie: %d %q", r.status, r.header.Get("Location"))
	}
	// htmx fetches follow redirects invisibly, so they get HX-Redirect.
	r = p.bridge.get("/bridge/screen", map[string]string{"HX-Request": "true"})
	if r.status != 401 || r.header.Get("HX-Redirect") != appOrigin+"/signin" {
		t.Errorf("htmx: %d %q", r.status, r.header.Get("HX-Redirect"))
	}
	// A cookie that names nothing is cleared.
	p.bridge.cookies[bridgeCookie] = &http.Cookie{Name: bridgeCookie, Value: "bogus"}
	r = p.bridge.get("/", nil)
	if r.status != 303 {
		t.Errorf("bogus cookie: %d", r.status)
	}
	if c := cookie(r, bridgeCookie); c == nil || c.Value != "" || !c.Expires.Before(time.Now()) {
		t.Errorf("the stale cookie was not cleared: %+v", c)
	}
	if b.count() != 0 {
		t.Error("an unauthenticated request reached a Bridge")
	}
}

// A session for another customer's Bridge is a 404, indistinguishable from
// a host that does not exist, and nothing is forwarded.
func TestAnotherTenantsSessionIs404(t *testing.T) {
	b := newBridgeEnv(t)
	ann := b.person(t, "ann@example.org")
	bob := b.person(t, "bob@example.org")
	b.enterOK(t, ann)

	// Ann's Bridge cookie, replayed against Bob's host.
	evil := b.client(t)
	evil.host = bob.host
	evil.cookies[bridgeCookie] = ann.bridge.cookies[bridgeCookie]
	r := evil.get("/", nil)
	if r.status != 404 || r.body != "not found" {
		t.Errorf("another tenant's session: %d %q, want 404", r.status, r.body)
	}
	if cookie(r, bridgeCookie) != nil {
		t.Error("the response touched the cookie")
	}
	if b.count() != 0 {
		t.Error("a cross-tenant request reached a Bridge")
	}
	// Ann still works on her own host.
	if r := ann.bridge.get("/", nil); r.status != 200 {
		t.Errorf("own host: %d", r.status)
	}
	// A POST is refused the same way.
	if r := evil.do("POST", "/bridge/enqueue", url.Values{}, map[string]string{"X-CSRF-Token": "t"}); r.status != 404 {
		t.Errorf("cross-tenant POST: %d", r.status)
	}
}

func TestSuspendedTenantIs404(t *testing.T) {
	b := newBridgeEnv(t)
	p := b.person(t, "ann@example.org")
	b.enterOK(t, p)
	if r := p.bridge.get("/", nil); r.status != 200 {
		t.Fatalf("before suspension: %d", r.status)
	}
	suspend := func() {
		if _, err := b.pool.Exec(context.Background(), `UPDATE tenants SET status = 'suspended', status_before_suspend = 'active' WHERE id = $1`, p.tenant); err != nil {
			t.Fatal(err)
		}
	}
	suspend()
	before := b.count()
	r := p.bridge.get("/", nil)
	if r.status != 404 || r.body != "not found" {
		t.Errorf("suspended: %d %q, want 404", r.status, r.body)
	}
	if b.count() != before {
		t.Error("a suspended tenant's request reached its Bridge")
	}
	// No new handoff can be made: the account session itself no longer works.
	if r := p.app.post("/account/bridge/open", nil); r.status != 303 || r.header.Get("Location") != "/signin" {
		t.Errorf("open while suspended: %d %q", r.status, r.header.Get("Location"))
	}
	// Reinstated, the same Bridge session works again.
	if _, err := b.pool.Exec(context.Background(), `UPDATE tenants SET status = 'active', status_before_suspend = NULL WHERE id = $1`, p.tenant); err != nil {
		t.Fatal(err)
	}
	if r := p.bridge.get("/", nil); r.status != 200 {
		t.Errorf("after reinstating: %d", r.status)
	}
}

func TestSigningOutOrRevokingEndsTheBridgeSession(t *testing.T) {
	b := newBridgeEnv(t)
	for name, end := range map[string]func(p person){
		"sign out": func(p person) {
			if r := p.app.post("/signout", nil); r.status != 303 {
				t.Fatalf("signout: %d", r.status)
			}
		},
		"revoke from the sessions list": func(p person) {
			ses, err := b.svc.LookupSession(context.Background(), p.app.cookies[sessionCookie].Value)
			if err != nil {
				t.Fatal(err)
			}
			if r := p.app.post("/account/sessions/"+ses.ID+"/revoke", nil); r.status != 303 {
				t.Fatalf("revoke: %d", r.status)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := b.person(t, strings.ReplaceAll(name, " ", "")+"@example.org")
			b.enterOK(t, p)
			if r := p.bridge.get("/", nil); r.status != 200 {
				t.Fatalf("before: %d", r.status)
			}
			end(p)
			before := b.count()
			r := p.bridge.get("/", nil)
			if r.status != 303 || r.header.Get("Location") != appOrigin+"/signin" {
				t.Errorf("after: %d %q", r.status, r.header.Get("Location"))
			}
			if b.count() != before {
				t.Error("the ended session still reached the Bridge")
			}
		})
	}
}

func TestSigningInAgainRevokesTheOldBridgeCookie(t *testing.T) {
	b := newBridgeEnv(t)
	p := b.person(t, "ann@example.org")
	b.enterOK(t, p)
	old := p.bridge.cookies[bridgeCookie].Value
	b.enterOK(t, p) // a second handoff with the cookie still in the jar
	if p.bridge.cookies[bridgeCookie].Value == old {
		t.Fatal("the Bridge cookie was reused")
	}
	stale := b.client(t)
	stale.host = p.host
	stale.cookies[bridgeCookie] = &http.Cookie{Name: bridgeCookie, Value: old}
	if r := stale.get("/", nil); r.status != 303 {
		t.Errorf("old Bridge cookie after a new handoff: %d, want 303 to sign in", r.status)
	}
}

func TestBotNotRunningPageComesFromTheControlPlane(t *testing.T) {
	b := newBridgeEnv(t)
	p := b.person(t, "ann@example.org")
	b.enterOK(t, p)
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"starting":    {provision.ErrStarting, "Your bot is starting"},
		"not running": {provision.ErrNotRunning, "Your bot is not running"},
	} {
		b.res.set(tc.err)
		before := b.count()
		r := p.bridge.get("/", nil)
		if r.status != 503 || !strings.Contains(r.body, tc.want) {
			t.Errorf("%s: %d %s", name, r.status, r.body)
		}
		if strings.Contains(r.body, "Bad Gateway") || b.count() != before {
			t.Errorf("%s: a proxy error page or a forwarded request", name)
		}
		if !strings.Contains(r.header.Get("Content-Security-Policy"), "frame-ancestors 'none'") || r.header.Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s: missing security headers", name)
		}
	}
}

func TestTenantHostResponsesCarrySecurityHeaders(t *testing.T) {
	b := newBridgeEnv(t)
	p := b.person(t, "ann@example.org")
	ghost := b.client(t)
	ghost.host = "nobody." + testDomain
	responses := map[string]result{
		"redirect to sign in": ghost.get("/", nil),
		"not found":           b.clientWithHost(t, "example.com").get("/", nil),
		"auth failure":        p.bridge.get("/auth?code=x", nil),
		"auth HEAD":           p.bridge.do("HEAD", "/auth?code=x", nil, nil),
	}
	b.enterOK(t, p)
	responses["proxied"] = p.bridge.get("/", nil)
	for name, r := range responses {
		for h, want := range map[string]string{
			"X-Frame-Options": "DENY", "Referrer-Policy": "same-origin", "X-Content-Type-Options": "nosniff",
			"Strict-Transport-Security": "max-age=31536000",
		} {
			if got := r.header.Get(h); got != want {
				t.Errorf("%s: %s = %q, want %q", name, h, got, want)
			}
		}
		if !strings.Contains(r.header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Errorf("%s: CSP = %q", name, r.header.Get("Content-Security-Policy"))
		}
	}
}

func (b *bridgeEnv) clientWithHost(t *testing.T, host string) *client {
	c := b.client(t)
	c.host = host
	return c
}

func TestOpenMyBridgeNeedsASessionAndCSRF(t *testing.T) {
	b := newBridgeEnv(t)
	p := b.person(t, "ann@example.org")
	if r := p.app.do("POST", "/account/bridge/open", url.Values{}, nil); r.status != 403 {
		t.Errorf("without a CSRF token: %d, want 403", r.status)
	}
	anon := b.client(t)
	if r := anon.post("/account/bridge/open", nil); r.status != 303 || r.header.Get("Location") != "/signin" {
		t.Errorf("without a session: %d %q", r.status, r.header.Get("Location"))
	}
	// GET spends nothing and offers nothing.
	if r := p.app.get("/account/bridge/open", nil); r.status == 303 && strings.Contains(r.header.Get("Location"), "code=") {
		t.Error("a GET produced a handoff code")
	}
	if rows(t, b.pool, `SELECT count(*) FROM bridge_handoffs`) != 0 {
		t.Error("a refused request created a handoff")
	}
}

func TestOpenMyBridgeRateLimit(t *testing.T) {
	b := newBridgeEnv(t)
	p := b.person(t, "ann@example.org")
	for i := range bridgeOpenLimit {
		if r := p.app.post("/account/bridge/open", nil); r.status != 303 {
			t.Fatalf("press %d: %d", i+1, r.status)
		}
	}
	if r := p.app.post("/account/bridge/open", nil); r.status != 429 {
		t.Errorf("over the limit: %d, want 429", r.status)
	}
}

func TestWithoutTheProxyEveryHostIsTheControlPlane(t *testing.T) {
	e := newEnv(t, auth.Options{})
	c := e.client(t)
	c.host = "anything.example"
	if r := c.get("/", nil); r.status != 200 {
		t.Errorf("routing off: %d", r.status)
	}
	if r := c.post("/account/bridge/open", nil); r.status != 303 {
		t.Errorf("open with routing off and no session: %d", r.status)
	}
}

func TestAccountPageOffersTheBridgeWhenRunning(t *testing.T) {
	p := newProvEnv(t)
	px, err := proxy.New(proxy.Config{Key: testKey, Resolver: p.prov, Scheme: "https", AppOrigin: appOrigin, Cookies: ControlPlaneCookies()})
	if err != nil {
		t.Fatal(err)
	}
	p.app = New(p.pool, WithApp(&App{Auth: p.svc, Admin: p.admin, Connect: p.conn, Provision: p.prov, Proxy: px, AppBaseURL: appOrigin, TenantDomain: testDomain}))
	p.host = appHost
	c, tenant := p.user(t, "ann@example.org")
	connectGame(t, c, tokenA)
	job := "sanholo-tenant-" + tenant

	// Starting: no button, and pressing it by hand is refused politely when
	// there is no instance at all.
	eventually(t, "the instance row", func() bool { return p.instance(t, tenant) != nil })
	if page := c.get("/account", nil); strings.Contains(page.body, "Open my Bridge") {
		t.Error("the account page offers the Bridge while the bot is starting")
	}
	// Running: the button is there.
	p.fake.SetPhase(job, nomadfake.Healthy)
	eventually(t, "running", func() bool { return p.instance(t, tenant).State == provision.StateRunning })
	page := c.get("/account", nil)
	if !strings.Contains(page.body, `action="/account/bridge/open"`) || !strings.Contains(page.body, "Open my Bridge") {
		t.Errorf("no Open my Bridge when running:\n%s", page.body)
	}
	// And the whole path works against the real resolver: Nomad's address for
	// the allocation is the upstream.
	r := c.post("/account/bridge/open", nil)
	if r.status != 303 {
		t.Fatalf("open: %d %s", r.status, r.body)
	}
	if _, err := p.prov.Upstream(context.Background(), tenant); err != nil {
		t.Errorf("Upstream for a running tenant: %v", err)
	}
	// Stopped: pressing the button is a polite refusal.
	if err := p.prov.Stop(context.Background(), tenant, false); err != nil {
		t.Fatal(err)
	}
	if r := c.post("/account/bridge/open", nil); r.status != 409 || !strings.Contains(r.body, "not running") {
		t.Errorf("open for a stopped bot: %d %s", r.status, r.body)
	}
	if page := c.get("/account", nil); strings.Contains(page.body, "Open my Bridge") {
		t.Error("the account page offers the Bridge for a stopped bot")
	}
}

// Routing uses the Host header alone: a forwarded-host header from the
// browser neither turns a tenant host into the app host nor the reverse.
func TestForwardedHostIsIgnoredForRouting(t *testing.T) {
	b := newBridgeEnv(t)
	p := b.person(t, "ann@example.org")
	r := p.bridge.get("/account", map[string]string{"X-Forwarded-Host": appHost})
	if r.status != 303 || r.header.Get("Location") != appOrigin+"/signin" {
		t.Errorf("tenant host with a forged X-Forwarded-Host: %d %q", r.status, r.header.Get("Location"))
	}
	other := b.client(t)
	other.host = "example.com"
	if r := other.get("/", map[string]string{"X-Forwarded-Host": appHost}); r.status != 404 {
		t.Errorf("unknown host with a forged X-Forwarded-Host: %d, want 404", r.status)
	}
}

// A tenant being deleted loses its Bridge exactly as a suspended one does:
// the existing Bridge session answers 404 and nothing reaches the instance.
func TestDeletingTenantIs404(t *testing.T) {
	b := newBridgeEnv(t)
	p := b.person(t, "ann@example.org")
	b.enterOK(t, p)
	if r := p.bridge.get("/", nil); r.status != 200 {
		t.Fatalf("before deletion: %d", r.status)
	}
	if _, err := b.pool.Exec(context.Background(), `UPDATE tenants SET status = 'deleting' WHERE id = $1`, p.tenant); err != nil {
		t.Fatal(err)
	}
	before := b.count()
	r := p.bridge.get("/", nil)
	if r.status != 404 || r.body != "not found" {
		t.Errorf("deleting: %d %q, want 404", r.status, r.body)
	}
	if b.count() != before {
		t.Error("a deleting tenant's request reached its Bridge")
	}
}
