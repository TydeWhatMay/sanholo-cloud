package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lhaig/sanholo-cloud/internal/admin"
	"github.com/lhaig/sanholo-cloud/internal/auth"
	"github.com/lhaig/sanholo-cloud/internal/dbtest"
)

const baseURL = "http://127.0.0.1:8090"

type env struct {
	app   *fiber.App
	svc   *auth.Service
	admin *admin.Service
	pool  *pgxpool.Pool
}

func newEnv(t *testing.T, opt auth.Options, trusted ...netip.Prefix) *env {
	t.Helper()
	pool := dbtest.Migrated(t)
	opt.BaseURL = baseURL
	if opt.MaxTenants == 0 {
		opt.MaxTenants = 20
	}
	svc := auth.New(pool, opt)
	adm := admin.New(pool, svc)
	app := New(pool, WithApp(&App{Auth: svc, Admin: adm, TrustedProxies: trusted}))
	return &env{app: app, svc: svc, admin: adm, pool: pool}
}

// client is a minimal cookie jar over app.Test.
type client struct {
	t       *testing.T
	app     *fiber.App
	cookies map[string]*http.Cookie
}

func (e *env) client(t *testing.T) *client {
	return &client{t: t, app: e.app, cookies: map[string]*http.Cookie{}}
}

type result struct {
	status  int
	body    string
	header  http.Header
	cookies []*http.Cookie
}

func (c *client) do(method, path string, form url.Values, hdr map[string]string) result {
	c.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	for _, ck := range c.cookies {
		req.AddCookie(&http.Cookie{Name: ck.Name, Value: ck.Value})
	}
	return c.send(req)
}

// send runs a prepared request and keeps the cookies it sets.
func (c *client) send(req *http.Request) result {
	c.t.Helper()
	resp, err := c.app.Test(req, -1)
	if err != nil {
		c.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	for _, ck := range resp.Cookies() {
		if ck.MaxAge < 0 {
			delete(c.cookies, ck.Name)
		} else {
			c.cookies[ck.Name] = ck
		}
	}
	return result{status: resp.StatusCode, body: string(b), header: resp.Header, cookies: resp.Cookies()}
}

func (c *client) get(path string, hdr map[string]string) result { return c.do("GET", path, nil, hdr) }

// post sends form with the CSRF token from the cookie, fetching one first.
func (c *client) post(path string, form url.Values) result {
	c.t.Helper()
	if c.cookies[csrfCookie] == nil {
		c.get("/", nil)
	}
	if form == nil {
		form = url.Values{}
	}
	form.Set("csrf", c.cookies[csrfCookie].Value)
	return c.do("POST", path, form, nil)
}

func linkFromOutbox(t *testing.T, pool *pgxpool.Pool, to string) string {
	t.Helper()
	var body string
	err := pool.QueryRow(context.Background(),
		`SELECT body FROM mail_outbox WHERE to_email = $1 ORDER BY id DESC LIMIT 1`, to).Scan(&body)
	if err != nil {
		t.Fatalf("no mail for %s: %v", to, err)
	}
	_, rest, ok := strings.Cut(body, baseURL)
	if !ok {
		t.Fatalf("no link in %q", body)
	}
	link, _, _ := strings.Cut(rest, "\n")
	return link
}

// confirm opens the emailed link as a person does: GET the page, then press
// its button (POST with the token from the page).
func confirm(t *testing.T, c *client, link string) result {
	t.Helper()
	page := c.get(link, nil)
	if page.status != 200 {
		t.Fatalf("confirm page: %d %s", page.status, page.body)
	}
	_, rest, _ := strings.Cut(link, "token=")
	return c.post("/signin/verify", url.Values{"token": {rest}})
}

func rows(t *testing.T, pool *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *env) account(t *testing.T, email string) {
	t.Helper()
	ctx := context.Background()
	code, _, err := e.svc.IssueInvite(ctx, email)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Redeem(ctx, code); err != nil {
		t.Fatal(err)
	}
}

// signedIn returns a client holding a live session for email.
func (e *env) signedIn(t *testing.T, email string) *client {
	t.Helper()
	c := e.client(t)
	if r := c.post("/signin", url.Values{"email": {email}}); r.status != 200 {
		t.Fatalf("signin: %d %s", r.status, r.body)
	}
	if _, err := e.svc.ProcessSignInRequests(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := confirm(t, c, linkFromOutbox(t, e.pool, email))
	if r.status != 303 || r.header.Get("Location") != "/account" {
		t.Fatalf("verify: %d %s", r.status, r.body)
	}
	return c
}

func TestCSRFRequiredOnPosts(t *testing.T) {
	e := newEnv(t, auth.Options{})
	c := e.client(t)
	c.get("/", nil)
	tok := c.cookies[csrfCookie].Value
	// A token differing in its first character, whatever that character is.
	wrong := "x" + tok[1:]
	if tok[0] == 'x' {
		wrong = "y" + tok[1:]
	}
	cases := []struct {
		name string
		form url.Values
		hdr  map[string]string
		want int
	}{
		{"no token", url.Values{"email": {"a@example.org"}}, nil, 403},
		{"wrong field token", url.Values{"email": {"a@example.org"}, "csrf": {wrong}}, nil, 403},
		{"empty field token", url.Values{"email": {"a@example.org"}, "csrf": {""}}, nil, 403},
		{"field token", url.Values{"email": {"a@example.org"}, "csrf": {tok}}, nil, 200},
		{"header token", url.Values{"email": {"b@example.org"}}, map[string]string{"X-CSRF-Token": tok}, 200},
		{"wrong header token", url.Values{"email": {"c@example.org"}}, map[string]string{"X-CSRF-Token": "nope"}, 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if r := c.do("POST", "/waitlist", tc.form, tc.hdr); r.status != tc.want {
				t.Fatalf("status %d, want %d", r.status, tc.want)
			}
		})
	}
	t.Run("token in body but no cookie", func(t *testing.T) {
		fresh := e.client(t)
		r := fresh.do("POST", "/waitlist", url.Values{"email": {"d@example.org"}, "csrf": {tok}}, nil)
		if r.status != 403 {
			t.Fatalf("status %d, want 403", r.status)
		}
	})
	t.Run("no cookie and no token", func(t *testing.T) {
		// Two empty strings compare equal; the missing cookie must still fail.
		if r := e.client(t).do("POST", "/waitlist", url.Values{"email": {"e@example.org"}}, nil); r.status != 403 {
			t.Fatalf("status %d, want 403", r.status)
		}
	})
	if n := rows(t, e.pool, `SELECT count(*) FROM waitlist`); n != 2 {
		t.Fatalf("waitlist rows = %d, want only the two accepted", n)
	}
	// The cookie is hardened.
	var ck *http.Cookie
	for _, x := range e.client(t).get("/", nil).cookies {
		if x.Name == csrfCookie {
			ck = x
		}
	}
	if ck == nil || !ck.Secure || !ck.HttpOnly || ck.Path != "/" || ck.Domain != "" {
		t.Fatalf("csrf cookie = %+v", ck)
	}
}

func TestWaitlistSamePageEveryTime(t *testing.T) {
	e := newEnv(t, auth.Options{})
	c := e.client(t)
	first := c.post("/waitlist", url.Values{"email": {"a@example.org"}})
	second := c.post("/waitlist", url.Values{"email": {"A@Example.org"}})
	if first.status != 200 || first.status != second.status || first.body != second.body {
		t.Fatalf("pages differ:\n%d %s\n%d %s", first.status, first.body, second.status, second.body)
	}
	if rows(t, e.pool, `SELECT count(*) FROM waitlist`) != 1 || rows(t, e.pool, `SELECT count(*) FROM mail_outbox`) != 0 {
		t.Fatal("duplicate row or mail")
	}
	for _, bad := range []string{"", "a@b", "a@@b.co", "a b@example.org", "a@example.org\r\nBcc: x@y.z", "a@example.org\n"} {
		if r := c.post("/waitlist", url.Values{"email": {bad}}); r.status != 400 {
			t.Errorf("%q: status %d, want 400", bad, r.status)
		}
	}
	if rows(t, e.pool, `SELECT count(*) FROM waitlist`) != 1 {
		t.Fatal("invalid address stored")
	}
}

func TestSignupInviteFlow(t *testing.T) {
	e := newEnv(t, auth.Options{MaxTenants: 1})
	ctx := context.Background()
	c := e.client(t)

	if r := c.get("/signup", nil); r.status != 403 {
		t.Fatalf("GET /signup no invite: %d", r.status)
	}
	if r := c.post("/signup", url.Values{"email": {"a@example.org"}}); r.status != 403 {
		t.Fatalf("POST /signup no invite: %d", r.status)
	}
	if r := c.post("/signup", url.Values{"code": {"bogus"}, "email": {"a@example.org"}}); r.status != 403 {
		t.Fatalf("bogus invite: %d", r.status)
	}
	if rows(t, e.pool, `SELECT count(*) FROM tenants`) != 0 {
		t.Fatal("tenant created without an invite")
	}

	code, _, _ := e.svc.IssueInvite(ctx, "a@example.org")
	code2, _, _ := e.svc.IssueInvite(ctx, "b@example.org")
	r := c.get("/signup?code="+url.QueryEscape(code), nil)
	if r.status != 200 || !strings.Contains(r.body, "a@example.org") {
		t.Fatalf("signup form: %d %s", r.status, r.body)
	}
	if rows(t, e.pool, `SELECT count(*) FROM invites WHERE used_at IS NOT NULL`) != 0 {
		t.Fatal("viewing the form consumed the invite")
	}
	if r := c.post("/signup", url.Values{"code": {code}}); r.status != 200 {
		t.Fatalf("redeem: %d %s", r.status, r.body)
	}
	if r := c.post("/signup", url.Values{"code": {code}}); r.status != 410 || !strings.Contains(r.body, "already been used") {
		t.Fatalf("second redeem: %d %s", r.status, r.body)
	}
	if r := c.post("/signup", url.Values{"code": {code2}}); r.status != 503 || !strings.Contains(r.body, "The beta is full") {
		t.Fatalf("over the cap: %d %s", r.status, r.body)
	}
	if rows(t, e.pool, `SELECT count(*) FROM tenants`) != 1 {
		t.Fatal("cap exceeded")
	}
}

func TestSignInSamePageKnownAndUnknown(t *testing.T) {
	e := newEnv(t, auth.Options{})
	e.account(t, "known@example.org")
	c := e.client(t)
	known := c.post("/signin", url.Values{"email": {"known@example.org"}})
	unknown := c.post("/signin", url.Values{"email": {"unknown@example.org"}})
	if known.status != 200 || known.status != unknown.status || known.body != unknown.body {
		t.Fatalf("responses differ:\n%d %s\n%d %s", known.status, known.body, unknown.status, unknown.body)
	}
	// Nothing is mailed on the request path for either.
	if n := rows(t, e.pool, `SELECT count(*) FROM mail_outbox`); n != 0 {
		t.Fatalf("outbox has %d rows after the requests", n)
	}
	if n := rows(t, e.pool, `SELECT count(*) FROM signin_requests`); n != 2 {
		t.Fatalf("signin_requests = %d, want one per request", n)
	}
	// After the worker runs, only the known address has mail.
	if _, err := e.svc.ProcessSignInRequests(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rows(t, e.pool, `SELECT count(*) FROM mail_outbox WHERE to_email = 'known@example.org'`) != 1 ||
		rows(t, e.pool, `SELECT count(*) FROM mail_outbox`) != 1 {
		t.Fatal("mail for the wrong addresses")
	}
	if r := c.post("/signin", url.Values{"email": {"x@y.co\r\nBcc: z@y.co"}}); r.status != 400 {
		t.Fatalf("CRLF address: %d", r.status)
	}
}

func TestSignInRateLimitBehindEdge(t *testing.T) {
	// app.Test connections report the peer 0.0.0.0.
	peer := netip.MustParseAddr("0.0.0.0")
	post := func(c *client, email, xff string) int {
		hdr := map[string]string{}
		if xff != "" {
			hdr["X-Forwarded-For"] = xff
		}
		if c.cookies[csrfCookie] == nil {
			c.get("/", nil)
		}
		return c.do("POST", "/signin", url.Values{"email": {email}, "csrf": {c.cookies[csrfCookie].Value}}, hdr).status
	}
	t.Run("spoofed header from untrusted peer is ignored", func(t *testing.T) {
		e := newEnv(t, auth.Options{}) // no trusted proxies
		c := e.client(t)
		for i := range auth.SigninLimit {
			if s := post(c, "u"+string(rune('a'+i))+"@example.org", "198.51.100."+string(rune('1'+i))); s != 200 {
				t.Fatalf("request %d: %d", i, s)
			}
		}
		// A fresh address and a fresh spoofed IP: still limited, by the peer.
		if s := post(c, "fresh@example.org", "203.0.113.77"); s != 429 {
			t.Fatalf("6th with spoofed header: %d, want 429", s)
		}
	})
	t.Run("trusted peer: forwarded address is the key", func(t *testing.T) {
		e := newEnv(t, auth.Options{}, netip.PrefixFrom(peer, 32))
		c := e.client(t)
		for i := range auth.SigninLimit {
			if s := post(c, "u"+string(rune('a'+i))+"@example.org", "198.51.100.1"); s != 200 {
				t.Fatalf("request %d: %d", i, s)
			}
		}
		if s := post(c, "fresh@example.org", "198.51.100.1"); s != 429 {
			t.Fatalf("6th same client: %d, want 429", s)
		}
		if s := post(c, "fresh@example.org", "198.51.100.2"); s != 200 {
			t.Fatalf("other client behind the same edge: %d, want 200", s)
		}
	})
	t.Run("per email regardless of address", func(t *testing.T) {
		e := newEnv(t, auth.Options{}, netip.PrefixFrom(peer, 32))
		c := e.client(t)
		for i := range auth.SigninLimit {
			if s := post(c, "same@example.org", "198.51.100."+string(rune('1'+i))); s != 200 {
				t.Fatalf("request %d: %d", i, s)
			}
		}
		if s := post(c, "SAME@example.org", "198.51.100.99"); s != 429 {
			t.Fatalf("6th for one email: %d, want 429", s)
		}
	})
}

func TestVerifyLinkAndSessionCookie(t *testing.T) {
	e := newEnv(t, auth.Options{})
	e.account(t, "a@example.org")
	c := e.client(t)
	c.post("/signin", url.Values{"email": {"a@example.org"}})
	_, _ = e.svc.ProcessSignInRequests(context.Background())
	link := linkFromOutbox(t, e.pool, "a@example.org")

	t.Run("HEAD, prefetch and a scanner GET do not consume", func(t *testing.T) {
		for _, tc := range []struct {
			method string
			hdr    map[string]string
			want   int
		}{
			{"HEAD", nil, 204},
			{"GET", map[string]string{"Sec-Purpose": "prefetch"}, 204},
			{"GET", map[string]string{"Sec-Purpose": "prefetch;prerender"}, 204},
			{"GET", map[string]string{"Purpose": "prefetch"}, 204},
			{"GET", map[string]string{"User-Agent": "MailScanner/1.0"}, 200},
		} {
			scanner := e.client(t)
			r := scanner.do(tc.method, link, nil, tc.hdr)
			if r.status != tc.want {
				t.Fatalf("%s %v: %d", tc.method, tc.hdr, r.status)
			}
		}
		if rows(t, e.pool, `SELECT count(*) FROM magic_links WHERE used_at IS NOT NULL`) != 0 ||
			rows(t, e.pool, `SELECT count(*) FROM account_sessions`) != 0 {
			t.Fatal("link spent by a GET, HEAD or prefetch request")
		}
	})
	t.Run("the confirm page carries a CSRF-protected POST", func(t *testing.T) {
		r := e.client(t).get(link, nil)
		if !strings.Contains(r.body, `method="post"`) || !strings.Contains(r.body, `name="csrf"`) {
			t.Fatalf("page: %s", r.body)
		}
		_, tok, _ := strings.Cut(link, "token=")
		// A cross-site POST without the CSRF token does not spend it.
		if r := e.client(t).do("POST", "/signin/verify", url.Values{"token": {tok}}, nil); r.status != 403 {
			t.Fatalf("POST without csrf: %d", r.status)
		}
		if rows(t, e.pool, `SELECT count(*) FROM magic_links WHERE used_at IS NOT NULL`) != 0 {
			t.Fatal("spent without a CSRF token")
		}
	})
	t.Run("POST consumes once and sets the cookie", func(t *testing.T) {
		r := confirm(t, c, link)
		if r.status != 303 || r.header.Get("Location") != "/account" {
			t.Fatalf("status %d", r.status)
		}
		var ck *http.Cookie
		for _, x := range r.cookies {
			if x.Name == "__Host-sanholo_session" {
				ck = x
			}
		}
		if ck == nil {
			t.Fatal("no session cookie")
		}
		if !ck.Secure || !ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode || ck.Path != "/" || ck.Domain != "" {
			t.Fatalf("cookie attributes: %+v", ck)
		}
		if ck.MaxAge < 29*24*3600 || ck.MaxAge > 30*24*3600 {
			t.Fatalf("MaxAge = %d, want 30 days", ck.MaxAge)
		}
		if len(ck.Value) < 43 {
			t.Fatalf("session id too short: %d", len(ck.Value))
		}
		if rows(t, e.pool, `SELECT count(*) FROM account_sessions WHERE secret_hash = $1`, ck.Value) != 0 {
			t.Fatal("session id stored in plaintext")
		}
		if r.header.Get("Referrer-Policy") != "no-referrer" {
			t.Fatal("link page lacks Referrer-Policy")
		}
	})
	t.Run("replay is refused", func(t *testing.T) {
		other := e.client(t)
		if r := confirm(t, other, link); r.status != 410 {
			t.Fatalf("replay: %d", r.status)
		}
		if r := other.get("/account", nil); r.status != 303 {
			t.Fatalf("replay gave access: %d", r.status)
		}
	})
	t.Run("garbage token", func(t *testing.T) {
		if r := confirm(t, e.client(t), "/signin/verify?token=nope"); r.status != 410 {
			t.Fatalf("status %d", r.status)
		}
	})
}

func TestSignInRevokesAPlantedSession(t *testing.T) {
	e := newEnv(t, auth.Options{})
	e.account(t, "a@example.org")
	victim := e.signedIn(t, "a@example.org")
	old := victim.cookies[sessionCookie].Value

	// Signing in again with the old cookie present retires that session
	// and issues a different id.
	c := e.client(t)
	c.cookies[sessionCookie] = &http.Cookie{Name: sessionCookie, Value: old}
	c.post("/signin", url.Values{"email": {"a@example.org"}})
	_, _ = e.svc.ProcessSignInRequests(context.Background())
	r := confirm(t, c, linkFromOutbox(t, e.pool, "a@example.org"))
	if r.status != 303 {
		t.Fatalf("status %d", r.status)
	}
	if c.cookies[sessionCookie].Value == old {
		t.Fatal("session id reused across sign-in")
	}
	if _, err := e.svc.LookupSession(context.Background(), old); err == nil {
		t.Fatal("old session survived a new sign-in")
	}
}

func TestAccountPageSessions(t *testing.T) {
	e := newEnv(t, auth.Options{})
	e.account(t, "a@example.org")
	e.account(t, "b@example.org")

	if r := e.client(t).get("/account", nil); r.status != 303 || r.header.Get("Location") != "/signin" {
		t.Fatalf("anonymous: %d %s", r.status, r.header.Get("Location"))
	}
	a1 := e.signedIn(t, "a@example.org")
	a2 := e.signedIn(t, "a@example.org")
	b := e.signedIn(t, "b@example.org")

	r := a1.get("/account", nil)
	if r.status != 200 || !strings.Contains(r.body, "a@example.org") || strings.Count(r.body, "/revoke") != 2 {
		t.Fatalf("account page: %d %s", r.status, r.body)
	}

	// Revoke a2's session from a1, using the id on the page.
	list, _ := e.svc.ListSessions(context.Background(), mustSession(t, e, a2).AccountID)
	var otherID string
	for _, s := range list {
		if s.ID != mustSession(t, e, a1).ID {
			otherID = s.ID
		}
	}
	// Another account cannot revoke it.
	if r := b.post("/account/sessions/"+otherID+"/revoke", nil); r.status != 303 {
		t.Fatalf("cross-account revoke: %d", r.status)
	}
	if r := a2.get("/account", nil); r.status != 200 {
		t.Fatal("another account revoked the session")
	}
	if r := a1.post("/account/sessions/"+otherID+"/revoke", nil); r.status != 303 || r.header.Get("Location") != "/account" {
		t.Fatalf("revoke: %d", r.status)
	}
	if r := a2.get("/account", nil); r.status != 303 {
		t.Fatalf("revoked session still works: %d", r.status)
	}
	// Revoke needs CSRF.
	if r := a1.do("POST", "/signout", url.Values{}, nil); r.status != 403 {
		t.Fatalf("signout without token: %d", r.status)
	}
	if r := a1.get("/account", nil); r.status != 200 {
		t.Fatal("signout without token ended the session")
	}
	// Sign out.
	if r := a1.post("/signout", nil); r.status != 303 || r.header.Get("Location") != "/signin" {
		t.Fatalf("signout: %d", r.status)
	}
	if rows(t, e.pool, `SELECT count(*) FROM account_sessions`) != 1 {
		t.Fatal("sign out left the session in the database")
	}
}

func mustSession(t *testing.T, e *env, c *client) auth.Session {
	t.Helper()
	s, err := e.svc.LookupSession(context.Background(), c.cookies[sessionCookie].Value)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSecurityHeadersAndStatic(t *testing.T) {
	e := newEnv(t, auth.Options{})
	r := e.client(t).get("/", nil)
	for k, want := range map[string]string{
		"Referrer-Policy":        "no-referrer",
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Cache-Control":          "no-store",
	} {
		if got := r.header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if csp := r.header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") {
		t.Errorf("CSP = %q", csp)
	}
	for _, bad := range []string{"unpkg", "cdn", "jsdelivr", "cdnjs"} {
		if strings.Contains(r.body, bad) {
			t.Errorf("page references %s", bad)
		}
	}
	js := e.client(t).get("/static/htmx.min.js", nil)
	if js.status != 200 || !strings.Contains(js.body, "htmx") {
		t.Fatalf("htmx: %d", js.status)
	}
}

func TestWaitlistRateLimitPerIP(t *testing.T) {
	peer := netip.PrefixFrom(netip.MustParseAddr("0.0.0.0"), 32)
	e := newEnv(t, auth.Options{}, peer)
	c := e.client(t)
	join := func(email, ip string) result {
		c.get("/", nil)
		return c.do("POST", "/waitlist", url.Values{"email": {email}, "csrf": {c.cookies[csrfCookie].Value}},
			map[string]string{"X-Forwarded-For": ip})
	}
	var okBody string
	for i := range waitlistLimit {
		r := join("u"+string(rune('a'+i))+"@example.org", "198.51.100.1")
		if r.status != 200 {
			t.Fatalf("submission %d: %d", i, r.status)
		}
		okBody = r.body
	}
	limited := join("one-more@example.org", "198.51.100.1")
	if limited.status != 200 || limited.body != okBody {
		t.Fatalf("limited response differs: %d\n%s", limited.status, limited.body)
	}
	if n := rows(t, e.pool, `SELECT count(*) FROM waitlist`); n != waitlistLimit {
		t.Fatalf("waitlist rows = %d, want %d: a limited submission must store nothing", n, waitlistLimit)
	}
	if r := join("other@example.org", "198.51.100.2"); r.status != 200 ||
		rows(t, e.pool, `SELECT count(*) FROM waitlist`) != waitlistLimit+1 {
		t.Fatal("another client is limited too")
	}
}

func TestSignupRateLimitPerIP(t *testing.T) {
	peer := netip.PrefixFrom(netip.MustParseAddr("0.0.0.0"), 32)
	e := newEnv(t, auth.Options{}, peer)
	c := e.client(t)
	post := func(code, ip string) result {
		c.get("/", nil)
		return c.do("POST", "/signup", url.Values{"code": {code}, "csrf": {c.cookies[csrfCookie].Value}},
			map[string]string{"X-Forwarded-For": ip})
	}
	for i := range signupLimit {
		if r := post("not-a-code", "198.51.100.1"); r.status != 403 {
			t.Fatalf("guess %d: %d", i, r.status)
		}
	}
	// A valid code from the limited address must not redeem.
	code, _, err := e.svc.IssueInvite(context.Background(), "a@example.org")
	if err != nil {
		t.Fatal(err)
	}
	if r := post(code, "198.51.100.1"); r.status != 429 {
		t.Fatalf("over the limit: %d, want 429", r.status)
	}
	if n := rows(t, e.pool, `SELECT count(*) FROM accounts`); n != 0 {
		t.Fatalf("accounts = %d: a limited sign-up must create nothing", n)
	}
	if r := post(code, "198.51.100.2"); r.status != 200 {
		t.Fatalf("another client: %d", r.status)
	}
}

func TestStaticFilesCarrySecurityHeaders(t *testing.T) {
	e := newEnv(t, auth.Options{})
	for _, path := range []string{"/static/htmx.min.js", "/static/app.css", "/static/passkey.js"} {
		r := e.client(t).get(path, nil)
		if r.status != 200 {
			t.Fatalf("%s: %d", path, r.status)
		}
		if got := r.header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s X-Content-Type-Options = %q", path, got)
		}
		if csp := r.header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
			t.Errorf("%s CSP = %q", path, csp)
		}
		if cc := r.header.Get("Cache-Control"); !strings.Contains(cc, "max-age=") || strings.Contains(cc, "no-store") {
			t.Errorf("%s Cache-Control = %q", path, cc)
		}
		if len(r.cookies) != 0 {
			t.Errorf("%s sets a cookie", path)
		}
	}
}
