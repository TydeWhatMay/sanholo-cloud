package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lhaig/sanholo-cloud/internal/auth"
	"github.com/lhaig/sanholo-cloud/internal/webauthntest"
)

const (
	rpID     = "localhost"
	rpOrigin = "http://localhost:8090"
)

func passkeyEnv(t *testing.T) *env {
	t.Helper()
	wa, err := auth.NewWebAuthn(rpID, rpOrigin, "SanHolo")
	if err != nil {
		t.Fatal(err)
	}
	return newEnv(t, auth.Options{WebAuthn: wa})
}

// postJSON sends a JSON body with the CSRF token in the header, as
// static/passkey.js does.
func (c *client) postJSON(path, body string) result {
	c.t.Helper()
	if c.cookies[csrfCookie] == nil {
		c.get("/", nil)
	}
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", c.cookies[csrfCookie].Value)
	for _, ck := range c.cookies {
		req.AddCookie(ck)
	}
	return c.send(req)
}

// addPasskey registers a passkey for the signed-in client.
func addPasskey(t *testing.T, c *client) *webauthntest.Authenticator {
	t.Helper()
	begin := c.postJSON("/account/passkeys/register/begin", "")
	if begin.status != 200 {
		t.Fatalf("register begin: %d %s", begin.status, begin.body)
	}
	au := webauthntest.New(rpID, rpOrigin)
	body, err := au.Create([]byte(begin.body))
	if err != nil {
		t.Fatal(err)
	}
	fin := c.postJSON("/account/passkeys/register/finish", string(body))
	if fin.status != 200 {
		t.Fatalf("register finish: %d %s", fin.status, fin.body)
	}
	return au
}

func passkeySignIn(t *testing.T, c *client, au *webauthntest.Authenticator) result {
	t.Helper()
	begin := c.postJSON("/signin/passkey/begin", "")
	if begin.status != 200 {
		t.Fatalf("signin begin: %d %s", begin.status, begin.body)
	}
	body, err := au.Get([]byte(begin.body))
	if err != nil {
		t.Fatal(err)
	}
	return c.postJSON("/signin/passkey/finish", string(body))
}

func TestPasskeyRegisterThenSignInOverHTTP(t *testing.T) {
	e := passkeyEnv(t)
	e.account(t, "a@example.org")
	c := e.signedIn(t, "a@example.org")

	acct := c.get("/account/", nil)
	if !strings.Contains(acct.body, `data-passkey="register"`) || strings.Contains(acct.body, "<script>") {
		t.Fatalf("account page: %s", acct.body)
	}
	au := addPasskey(t, c)
	if n := rows(t, e.pool, `SELECT count(*) FROM passkeys`); n != 1 {
		t.Fatalf("passkeys = %d", n)
	}
	if !strings.Contains(c.get("/account/", nil).body, "Delete") {
		t.Error("account page does not list the passkey")
	}

	// A fresh browser signs in with the passkey alone.
	fresh := e.client(t)
	if r := fresh.get("/signin", nil); !strings.Contains(r.body, `data-passkey="login"`) {
		t.Fatalf("sign-in page offers no passkey: %s", r.body)
	}
	r := passkeySignIn(t, fresh, au)
	if r.status != 200 || !strings.Contains(r.body, "/account") {
		t.Fatalf("finish: %d %s", r.status, r.body)
	}
	var found bool
	for _, ck := range r.cookies {
		if ck.Name == sessionCookie {
			found = true
			if !ck.Secure || !ck.HttpOnly || ck.Path != "/" || ck.Domain != "" || ck.SameSite != http.SameSiteLaxMode {
				t.Errorf("session cookie = %+v", ck)
			}
		}
	}
	if !found {
		t.Fatal("no session cookie")
	}
	if page := fresh.get("/account/", nil); page.status != 200 || !strings.Contains(page.body, "a@example.org") {
		t.Fatalf("account after passkey sign-in: %d", page.status)
	}
	if n := rows(t, e.pool, `SELECT count(*) FROM account_sessions WHERE method = 'passkey'`); n != 1 {
		t.Fatalf("passkey sessions = %d", n)
	}
}

func TestPasskeyEndpointsNeedSessionAndCSRF(t *testing.T) {
	e := passkeyEnv(t)
	e.account(t, "a@example.org")

	// No session: sent to sign in, nothing stored.
	anon := e.client(t)
	if r := anon.postJSON("/account/passkeys/register/begin", ""); r.status != 303 {
		t.Fatalf("anonymous register begin: %d", r.status)
	}
	if n := rows(t, e.pool, `SELECT count(*) FROM webauthn_challenges`); n != 0 {
		t.Fatal("challenge stored for an anonymous visitor")
	}

	// A session but no CSRF token.
	c := e.signedIn(t, "a@example.org")
	for _, path := range []string{"/account/passkeys/register/begin", "/signin/passkey/begin"} {
		req := httptest.NewRequest("POST", path, strings.NewReader(""))
		for _, ck := range c.cookies {
			req.AddCookie(ck)
		}
		if r := c.send(req); r.status != 403 {
			t.Errorf("%s without CSRF token: %d", path, r.status)
		}
	}
}

func TestPasskeyFinishNeedsTheBrowsersCookie(t *testing.T) {
	e := passkeyEnv(t)
	e.account(t, "a@example.org")
	c := e.signedIn(t, "a@example.org")
	begin := c.postJSON("/account/passkeys/register/begin", "")
	var ck string
	for _, x := range begin.cookies {
		if x.Name == challengeCookie {
			ck = x.Value
			if !x.Secure || !x.HttpOnly || x.Path != "/" || x.Domain != "" || x.MaxAge != 300 {
				t.Errorf("challenge cookie = %+v", x)
			}
		}
	}
	if ck == "" {
		t.Fatal("no challenge cookie")
	}
	body, _ := webauthntest.New(rpID, rpOrigin).Create([]byte(begin.body))

	delete(c.cookies, challengeCookie)
	if r := c.postJSON("/account/passkeys/register/finish", string(body)); r.status != 400 {
		t.Fatalf("without the cookie: %d %s", r.status, r.body)
	}
	if n := rows(t, e.pool, `SELECT count(*) FROM passkeys`); n != 0 {
		t.Fatal("passkey stored without the cookie")
	}
}

func TestPasskeyDeleteOnlyOwnOverHTTP(t *testing.T) {
	e := passkeyEnv(t)
	e.account(t, "a@example.org")
	e.account(t, "b@example.org")
	a := e.signedIn(t, "a@example.org")
	b := e.signedIn(t, "b@example.org")
	addPasskey(t, a)
	list, _ := e.svc.ListPasskeys(context.Background(), accountOf(t, e, "a@example.org"))
	pid := list[0].ID

	if r := b.post("/account/passkeys/"+pid+"/delete", nil); r.status != 303 {
		t.Fatalf("foreign delete: %d", r.status)
	}
	if n := rows(t, e.pool, `SELECT count(*) FROM passkeys`); n != 1 {
		t.Fatal("another account deleted the passkey")
	}
	if r := a.post("/account/passkeys/"+pid+"/delete", nil); r.status != 303 {
		t.Fatalf("own delete: %d", r.status)
	}
	if n := rows(t, e.pool, `SELECT count(*) FROM passkeys`); n != 0 {
		t.Fatal("owner could not delete the passkey")
	}
}

func accountOf(t *testing.T, e *env, email string) string {
	t.Helper()
	var id string
	if err := e.pool.QueryRow(context.Background(),
		`SELECT id::text FROM accounts WHERE email_hash = $1`, auth.HashEmail(email)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestPasskeyUIHiddenWhenDisabled(t *testing.T) {
	e := newEnv(t, auth.Options{})
	if r := e.client(t).get("/signin", nil); strings.Contains(r.body, "data-passkey") {
		t.Fatal("sign-in page offers passkeys while they are off")
	}
	if r := e.client(t).postJSON("/signin/passkey/begin", ""); r.status != 404 {
		t.Fatalf("begin while disabled: %d", r.status)
	}
}

func TestPasskeyScriptIsStaticAndCSPStaysStrict(t *testing.T) {
	e := passkeyEnv(t)
	r := e.client(t).get("/static/passkey.js", nil)
	if r.status != 200 || !strings.Contains(r.body, "navigator.credentials") {
		t.Fatalf("static/passkey.js: %d", r.status)
	}
	page := e.client(t).get("/signin", nil)
	csp := page.header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self';") || strings.Contains(csp, "unsafe") {
		t.Fatalf("csp = %q", csp)
	}
}
