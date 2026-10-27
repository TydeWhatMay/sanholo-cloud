package server

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lhaig/sanholo-cloud/internal/auth"
	"github.com/lhaig/sanholo-cloud/internal/connect"
	"github.com/lhaig/sanholo-cloud/internal/game"
	"github.com/lhaig/sanholo-cloud/internal/game/gamefake"
	"github.com/lhaig/sanholo-cloud/internal/secrets"
	"github.com/lhaig/sanholo-cloud/internal/secrets/qmfake"
)

const (
	cpToken = "cp-client-token-7f3a"
	tokenA  = "GAMETOKEN-aaaaaaaaaaaaaaaa"
	tokenA2 = "GAMETOKEN-a2a2a2a2a2a2a2a2" // same game account as tokenA
	tokenB  = "GAMETOKEN-bbbbbbbbbbbbbbbb"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type gameEnv struct {
	*env
	qm   *qmfake.Fake
	gm   *gamefake.Fake
	conn *connect.Service
	logs *syncBuf
}

// newGameEnv mounts the app with a connect service over fake qm and game.
func newGameEnv(t *testing.T) *gameEnv {
	t.Helper()
	e := passkeyEnv(t) // passkeys on, so tests can make an operator
	g := &gameEnv{env: e, qm: qmfake.New(t, cpToken), gm: gamefake.New(t), logs: &syncBuf{}}
	g.gm.AddAccount(tokenA, gamefake.Replicant{Code: "AAAA1111", Name: "Alpha", CreatedAt: "2026-01-01T00:00:00Z"})
	g.gm.AddAccount(tokenA2, gamefake.Replicant{Code: "AAAA1111", Name: "Alpha", CreatedAt: "2026-01-01T00:00:00Z"})
	g.gm.AddAccount(tokenB, gamefake.Replicant{Code: "BBBB2222", Name: "Bravo", CreatedAt: "2026-01-02T00:00:00Z"})
	logger := slog.New(slog.NewTextHandler(g.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	sec, err := secrets.New(secrets.Config{Addr: g.qm.URL(), Token: cpToken, Timeout: 2 * time.Second, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	g.mount(t, sec, logger)
	return g
}

func (g *gameEnv) mount(t *testing.T, sec *secrets.Client, logger *slog.Logger) {
	t.Helper()
	gc, err := game.New(game.Config{BaseURL: g.gm.URL(), Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	g.conn, err = connect.New(g.pool, sec, gc, logger, 1)
	if err != nil {
		t.Fatal(err)
	}
	g.app = New(g.pool, WithApp(&App{Auth: g.svc, Admin: g.admin, Connect: g.conn}))
}

// user returns a signed-in client and the tenant id of its account.
func (g *gameEnv) user(t *testing.T, email string) (*client, string) {
	t.Helper()
	g.account(t, email)
	c := g.signedIn(t, email)
	var tenant string
	if err := g.pool.QueryRow(context.Background(), `SELECT tenant_id FROM accounts WHERE lower(email) = lower($1)`, email).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	return c, tenant
}

func (g *gameEnv) rowCount(t *testing.T) int {
	t.Helper()
	return rows(t, g.pool, `SELECT count(*) FROM game_accounts`)
}

func validate(c *client, token string) result {
	return c.post("/account/game/validate", url.Values{"token": {token}})
}

func qmPath(tenant string) string { return "sanholo/tenants/" + tenant + "/game" }

func TestGameSectionShowsForm(t *testing.T) {
	g := newGameEnv(t)
	c, _ := g.user(t, "ann@example.org")
	r := c.get("/account", nil)
	for _, want := range []string{"<h2>Game account</h2>", `action="/account/game/validate"`, `type="password"`, `name="token"`, `name="csrf"`} {
		if !strings.Contains(r.body, want) {
			t.Errorf("account page lacks %q", want)
		}
	}
	if strings.Contains(r.body, "not available") {
		t.Error("account page says connecting is not available")
	}
}

func TestGameFlowValidateConfirmStore(t *testing.T) {
	g := newGameEnv(t)
	c, tenant := g.user(t, "ann@example.org")

	v := validate(c, tokenA)
	if v.status != 200 {
		t.Fatalf("validate: %d %s", v.status, v.body)
	}
	for _, want := range []string{"Alpha", "AAAA1111", `action="/account/game/confirm"`, `action="/account/game/cancel"`} {
		if !strings.Contains(v.body, want) {
			t.Errorf("confirmation page lacks %q", want)
		}
	}
	var ck string
	for _, c := range v.cookies {
		if c.Name == gameCookie {
			ck = c.Value
			if !c.Secure || !c.HttpOnly || c.Path != "/" || c.Domain != "" || c.MaxAge != 600 {
				t.Errorf("cookie attributes: %+v", c)
			}
		}
	}
	if ck == "" {
		t.Fatal("no confirmation cookie")
	}
	if strings.Contains(v.body, ck) {
		t.Error("the cookie secret is in the page")
	}
	if len(g.qm.Requests()) != 0 || g.rowCount(t) != 0 {
		t.Fatal("validate wrote to qm or the database")
	}

	done := c.post("/account/game/confirm", nil)
	if done.status != 200 || !strings.Contains(done.body, "Game account connected") {
		t.Fatalf("confirm: %d %s", done.status, done.body)
	}
	stored, ok := g.qm.Stored(qmPath(tenant))
	if !ok || stored["api_token"] != tokenA || stored["replicant_code"] != "AAAA1111" {
		t.Fatalf("qm holds %v", stored)
	}
	for _, c := range done.cookies {
		if c.Name == gameCookie && c.Value != "" {
			t.Errorf("the confirmation cookie was not cleared: %+v", c)
		}
	}

	acct := c.get("/account", nil)
	for _, want := range []string{"Alpha", "AAAA1111", time.Now().UTC().Format("2006-01-02")} {
		if !strings.Contains(acct.body, want) {
			t.Errorf("account page lacks %q", want)
		}
	}
	if strings.Contains(acct.body, tokenA) {
		t.Error("the account page shows the token")
	}
	// A second confirm has nothing to confirm.
	if r := c.post("/account/game/confirm", nil); r.status != 410 {
		t.Errorf("second confirm = %d, want 410", r.status)
	}
	if g.qm.Versions(qmPath(tenant)) != 1 {
		t.Error("the token was written twice")
	}
}

func TestGameErrorsWriteNothing(t *testing.T) {
	cases := []struct {
		name   string
		token  string
		prep   func(g *gameEnv)
		status int
		want   string
	}{
		{"invalid token", "not-a-token", nil, 400, "The game did not accept this token."},
		{"game unreachable", tokenA, func(g *gameEnv) { g.gm.Close() }, 503, "could not reach the game"},
		{"game failing", tokenA, func(g *gameEnv) { g.gm.SetStatus(500) }, 503, "could not reach the game"},
		{"no replicant", "empty", func(g *gameEnv) { g.gm.AddAccount("empty") }, 422, "no replicant"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newGameEnv(t)
			c, _ := g.user(t, "ann@example.org")
			if tc.prep != nil {
				tc.prep(g)
			}
			r := validate(c, tc.token)
			if r.status != tc.status || !strings.Contains(r.body, tc.want) {
				t.Fatalf("got %d %q, want %d containing %q", r.status, r.body, tc.status, tc.want)
			}
			for _, ck := range r.cookies {
				if ck.Name == gameCookie && ck.Value != "" {
					t.Error("a confirmation cookie was issued")
				}
			}
			if len(g.qm.Requests()) != 0 || g.rowCount(t) != 0 {
				t.Fatal("something was written")
			}
			if strings.Contains(r.body, tc.token) {
				t.Error("the page echoes the token")
			}
		})
	}
}

func TestGameAccountConnectedElsewhere(t *testing.T) {
	g := newGameEnv(t)
	ann, _ := g.user(t, "ann@example.org")
	bob, bobTenant := g.user(t, "bob@example.org")
	validate(ann, tokenA)
	ann.post("/account/game/confirm", nil)

	r := validate(bob, tokenA2) // a different token, the same game account
	if r.status != 409 || !strings.Contains(r.body, "This game account is already connected to a SanHolo account.") {
		t.Fatalf("got %d %s", r.status, r.body)
	}
	for _, leak := range []string{"ann", "Ann", "tenant"} {
		if strings.Contains(r.body, leak) {
			t.Errorf("the refusal mentions %q", leak)
		}
	}
	if g.qm.Versions(qmPath(bobTenant)) != 0 || g.rowCount(t) != 1 {
		t.Fatal("bob wrote something")
	}
}

func TestGameBetaLimitAndReconnect(t *testing.T) {
	g := newGameEnv(t)
	c, tenant := g.user(t, "ann@example.org")
	validate(c, tokenA)
	c.post("/account/game/confirm", nil)

	r := validate(c, tokenB)
	if r.status != 409 || !strings.Contains(r.body, "Beta limit: one game account.") {
		t.Fatalf("got %d %s", r.status, r.body)
	}
	if g.qm.Versions(qmPath(tenant)) != 1 {
		t.Fatal("the refused account reached qm")
	}

	v := validate(c, tokenA2)
	if v.status != 200 || !strings.Contains(v.body, "replaces the stored token") {
		t.Fatalf("reconnect: %d %s", v.status, v.body)
	}
	if r := c.post("/account/game/confirm", nil); r.status != 200 {
		t.Fatalf("reconnect confirm: %d %s", r.status, r.body)
	}
	stored, _ := g.qm.Stored(qmPath(tenant))
	// A reconnect is a new version of the same secret, not a deletion.
	if stored["api_token"] != tokenA2 || g.qm.Versions(qmPath(tenant)) != 2 {
		t.Fatalf("qm holds %v (versions %d)", stored, g.qm.Versions(qmPath(tenant)))
	}
	if n := g.qm.Count("DELETE", "/"); n != 0 {
		t.Fatalf("%d delete requests during a reconnect", n)
	}
}

func TestGameCancel(t *testing.T) {
	g := newGameEnv(t)
	c, _ := g.user(t, "ann@example.org")
	validate(c, tokenA)
	r := c.post("/account/game/cancel", nil)
	if r.status != 303 || r.header.Get("Location") != "/account" {
		t.Fatalf("cancel: %d", r.status)
	}
	if r := c.post("/account/game/confirm", nil); r.status != 410 {
		t.Fatalf("confirm after cancel = %d, want 410", r.status)
	}
	if len(g.qm.Requests()) != 0 {
		t.Fatal("qm was written after a cancel")
	}
}

func TestGameConfirmNeedsTheValidatingBrowser(t *testing.T) {
	g := newGameEnv(t)
	ann, _ := g.user(t, "ann@example.org")
	bob, _ := g.user(t, "bob@example.org")
	v := validate(ann, tokenA)
	var secret string
	for _, c := range v.cookies {
		if c.Name == gameCookie {
			secret = c.Value
		}
	}
	// Another signed-in person presents Ann's cookie value.
	annCookie := ann.cookies[gameCookie]
	if secret == "" || annCookie == nil || annCookie.Value != secret {
		t.Fatal("fixture: no cookie to present")
	}
	bob.cookies[gameCookie] = annCookie
	if r := bob.post("/account/game/confirm", nil); r.status != 410 {
		t.Fatalf("bob confirmed with ann's cookie: %d", r.status)
	}
	if g.qm.Versions(qmPath("x")) != 0 || len(g.qm.Requests()) != 0 {
		t.Fatal("qm was written")
	}
	// No cookie at all.
	delete(ann.cookies, gameCookie)
	if r := ann.post("/account/game/confirm", nil); r.status != 410 {
		t.Fatalf("confirm without a cookie: %d", r.status)
	}
	// Ann's pending entry is intact for Ann once she has the cookie back.
	ann.cookies[gameCookie] = annCookie
	if r := ann.post("/account/game/confirm", nil); r.status != 200 {
		t.Fatalf("ann's own confirmation failed: %d %s", r.status, r.body)
	}
}

func TestGameRoutesNeedSessionAndCSRF(t *testing.T) {
	g := newGameEnv(t)
	anon := g.client(t)
	for _, p := range []string{"/account/game/validate", "/account/game/confirm", "/account/game/cancel"} {
		if r := anon.post(p, url.Values{"token": {tokenA}}); r.status != 303 || r.header.Get("Location") != "/signin" {
			t.Errorf("anonymous POST %s = %d %s", p, r.status, r.header.Get("Location"))
		}
	}
	c, _ := g.user(t, "ann@example.org")
	for _, p := range []string{"/account/game/validate", "/account/game/confirm", "/account/game/cancel"} {
		if r := c.do("POST", p, url.Values{"token": {tokenA}}, nil); r.status != 403 {
			t.Errorf("POST %s without CSRF = %d", p, r.status)
		}
		if r := c.do("POST", p, url.Values{"token": {tokenA}, "csrf": {"x"}}, nil); r.status != 403 {
			t.Errorf("POST %s with a wrong CSRF = %d", p, r.status)
		}
	}
	if n := len(g.gm.Requests()); n != 0 {
		t.Fatalf("%d game requests from refused POSTs", n)
	}
}

// pinMidWindow fixes the clock at the middle of an hour, so a fixed-window
// rate limit cannot reset part-way through a test that runs across the hour.
func pinMidWindow(g *gameEnv) {
	at := time.Now().Truncate(time.Hour).Add(90 * time.Minute)
	g.svc.SetClock(func() time.Time { return at })
}

func TestGameValidateRateLimits(t *testing.T) {
	t.Run("per account", func(t *testing.T) {
		g := newGameEnv(t)
		pinMidWindow(g)
		c, _ := g.user(t, "ann@example.org")
		for i := range gameValidateAccountLimit {
			if r := validate(c, "nope"); r.status != 400 {
				t.Fatalf("attempt %d = %d", i+1, r.status)
			}
		}
		if r := validate(c, tokenA); r.status != 429 {
			t.Fatalf("over the account limit = %d, want 429", r.status)
		}
		if n := len(g.gm.Requests()); n != gameValidateAccountLimit {
			t.Fatalf("%d game reads, want %d: the limited request still reached the game", n, gameValidateAccountLimit)
		}
	})
	t.Run("per address", func(t *testing.T) {
		g := newGameEnv(t)
		pinMidWindow(g)
		var clients []*client
		for i := range 4 {
			c, _ := g.user(t, fmt.Sprintf("u%d@example.org", i))
			clients = append(clients, c)
		}
		// Three accounts use up the address's 30 attempts at 10 each; every
		// account is still inside its own limit.
		for _, c := range clients[:3] {
			for range gameValidateAccountLimit {
				if r := validate(c, "nope"); r.status != 400 {
					t.Fatalf("status %d", r.status)
				}
			}
		}
		if r := validate(clients[3], tokenA); r.status != 429 {
			t.Fatalf("fourth account from the same address = %d, want 429", r.status)
		}
	})
}

func TestGameConfirmRateLimit(t *testing.T) {
	g := newGameEnv(t)
	c, _ := g.user(t, "ann@example.org")
	for range gameConfirmLimit {
		if r := c.post("/account/game/confirm", nil); r.status != 410 {
			t.Fatalf("status %d", r.status)
		}
	}
	if r := c.post("/account/game/confirm", nil); r.status != 429 {
		t.Fatalf("over the limit = %d", r.status)
	}
}

func TestGameUnavailableWithoutQM(t *testing.T) {
	g := newGameEnv(t)
	sec, err := secrets.New(secrets.Config{})
	if err != nil {
		t.Fatal(err)
	}
	g.mount(t, sec, nil)
	c, _ := g.user(t, "ann@example.org")
	page := c.get("/account", nil)
	if page.status != 200 || !strings.Contains(page.body, "Connecting a game account is not available here.") {
		t.Fatalf("account page: %d", page.status)
	}
	if strings.Contains(page.body, "/account/game/validate") {
		t.Error("the form is offered although connecting is unavailable")
	}
	for _, p := range []string{"/account/game/validate", "/account/game/confirm"} {
		if r := c.post(p, url.Values{"token": {tokenA}}); r.status != 503 || !strings.Contains(r.body, "not available here") {
			t.Errorf("POST %s = %d %s", p, r.status, r.body)
		}
	}
	if r := c.post("/account/game/cancel", nil); r.status != 303 {
		t.Errorf("cancel = %d", r.status)
	}
	if n := len(g.gm.Requests()); n != 0 {
		t.Fatalf("%d game reads without qm", n)
	}
}

func TestAppWithoutConnectServiceDoesNotCrash(t *testing.T) {
	e := newEnv(t, auth.Options{}) // App.Connect is nil
	e.account(t, "ann@example.org")
	c := e.signedIn(t, "ann@example.org")
	if r := c.get("/account", nil); r.status != 200 || !strings.Contains(r.body, "not available here") {
		t.Fatalf("account page: %d", r.status)
	}
	for _, p := range []string{"/account/game/validate", "/account/game/confirm", "/account/game/cancel"} {
		if r := c.post(p, url.Values{"token": {tokenA}}); r.status != 503 && r.status != 303 {
			t.Errorf("POST %s = %d", p, r.status)
		}
	}
}

func TestAdminTenantListShowsGameAccount(t *testing.T) {
	g := newGameEnv(t)
	ann, tenant := g.user(t, "ann@example.org")
	g.user(t, "bob@example.org") // no game account
	validate(ann, tokenA)
	ann.post("/account/game/confirm", nil)

	op, _ := operator(t, g.env, "op@example.org")
	r := op.get("/admin", nil)
	if r.status != 200 {
		t.Fatalf("admin: %d", r.status)
	}
	if !strings.Contains(r.body, "Alpha (AAAA1111)") {
		t.Errorf("the tenant list lacks the game account:\n%s", r.body)
	}
	if !strings.Contains(r.body, "<th>Game account</th>") || !strings.Contains(r.body, ">none<") {
		t.Error("the game account column or the empty state is missing")
	}
	if strings.Contains(r.body, tokenA) {
		t.Error("the admin page shows the token")
	}
	_ = tenant
}

// The token appears in exactly two places: the Authorization header of the
// request to the game, and the body of the write to qm. Nowhere else: not in
// a URL, a log, an error page, a cookie, a response, or the database.
func TestTokenAppearsOnlyInTheGameHeaderAndTheQMWrite(t *testing.T) {
	g := newGameEnv(t)
	var std syncBuf
	oldLog, oldSlog := log.Writer(), slog.Default()
	log.SetOutput(&std)
	slog.SetDefault(slog.New(slog.NewTextHandler(&std, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { log.SetOutput(oldLog); slog.SetDefault(oldSlog) })

	ann, tenant := g.user(t, "ann@example.org")
	bob, _ := g.user(t, "bob@example.org")

	var responses []result
	collect := func(r result) result { responses = append(responses, r); return r }
	collect(validate(ann, tokenA))
	collect(ann.post("/account/game/confirm", nil))
	collect(ann.get("/account", nil))
	collect(validate(ann, tokenA2)) // reconnect
	collect(ann.post("/account/game/confirm", nil))
	collect(validate(bob, tokenA)) // taken
	collect(validate(bob, tokenB))
	g.qm.SetOverride(func(r qmfake.Request) (int, string, bool) { return 503, `{}`, true })
	collect(bob.post("/account/game/confirm", nil)) // qm failure
	g.qm.SetOverride(nil)
	collect(validate(bob, "wrong"))
	g.gm.SetStatus(500)
	collect(validate(bob, tokenB))
	g.gm.SetStatus(0)

	for _, tok := range []string{tokenA, tokenA2, tokenB} {
		for _, r := range responses {
			if strings.Contains(r.body, tok) {
				t.Errorf("a response body contains %q", tok)
			}
			for k, vs := range r.header {
				for _, v := range vs {
					if strings.Contains(v, tok) {
						t.Errorf("response header %s contains %q", k, tok)
					}
				}
			}
			for _, c := range r.cookies {
				if strings.Contains(c.Value, tok) {
					t.Errorf("cookie %s contains %q", c.Name, tok)
				}
			}
		}
		for _, l := range []string{g.logs.String(), std.String()} {
			if strings.Contains(l, tok) {
				t.Errorf("a log contains %q:\n%s", tok, l)
			}
		}
		var inDB int
		for _, q := range []string{
			`SELECT count(*) FROM audit_log WHERE concat(actor, action, target, details::text) LIKE $1`,
			`SELECT count(*) FROM mail_outbox WHERE concat(to_email, subject, body) LIKE $1`,
			`SELECT count(*) FROM game_accounts WHERE concat(game_account_id, replicant_name, replicant_code) LIKE $1`,
			`SELECT count(*) FROM rate_counters WHERE key LIKE $1`,
		} {
			inDB += rows(t, g.pool, q, "%"+tok+"%")
		}
		if inDB != 0 {
			t.Errorf("%q is in the database", tok)
		}

		sawGame := 0
		for _, r := range g.gm.Requests() {
			if r.URL != "/v1/accounts/me" || r.Method != "GET" {
				t.Errorf("unexpected game request %s %s", r.Method, r.URL)
			}
			if strings.Contains(r.URL, tok) || strings.Contains(r.Body, tok) {
				t.Errorf("the token is in the game request URL or body")
			}
			for k, vs := range r.Header {
				for _, v := range vs {
					if strings.Contains(v, tok) {
						if k != "Authorization" || v != "Bearer "+tok {
							t.Errorf("the token is in game request header %s", k)
						}
						sawGame++
					}
				}
			}
		}
		if sawGame == 0 {
			t.Errorf("the game never saw %q in an Authorization header; the check is vacuous", tok)
		}
		puts := 0
		for _, r := range g.qm.Requests() {
			if strings.Contains(r.URL, tok) {
				t.Errorf("the token is in a qm URL: %s", r.URL)
			}
			for k, vs := range r.Header {
				for _, v := range vs {
					if strings.Contains(v, tok) {
						t.Errorf("the token is in qm header %s", k)
					}
				}
			}
			if strings.Contains(r.Body, tok) {
				if r.Method != "POST" || !strings.Contains(r.URL, "/secret/data/") {
					t.Errorf("the token is in a qm request other than the write: %s %s", r.Method, r.URL)
				}
				puts++
			}
		}
		// tokenB's write was refused with a 503 by the override, but the
		// request itself was recorded; tokenA and tokenA2 were stored.
		if puts == 0 {
			t.Errorf("qm never received %q; the check is vacuous", tok)
		}
	}
	if stored, _ := g.qm.Stored(qmPath(tenant)); stored["api_token"] != tokenA2 {
		t.Fatalf("fixture: qm holds %v", stored)
	}
	if !strings.Contains(g.logs.String(), "connect:") {
		t.Fatal("no log lines captured; the log check is vacuous")
	}
}

func TestPrivacyNoticeDescribesTheGameAccountRecord(t *testing.T) {
	e := newEnv(t, auth.Options{})
	body := e.client(t).get("/privacy", nil).body
	for _, want := range []string{
		"which game account you connected",
		"the name and code of that replicant",
		"held in the server's memory for up to 10 minutes",
		"Account, game account record, game token, instance volume and push subscriptions: until",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("privacy notice lacks %q", want)
		}
	}
	if connect.PendingTTL != 10*time.Minute {
		t.Errorf("PendingTTL = %v: update the privacy notice and the confirmation page", connect.PendingTTL)
	}
}
