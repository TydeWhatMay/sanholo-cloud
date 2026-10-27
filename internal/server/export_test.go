package server

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/lhaig/sanholo-cloud/internal/auth"
	"github.com/lhaig/sanholo-cloud/internal/export"
	"github.com/lhaig/sanholo-cloud/internal/export/instancefake"
	"github.com/lhaig/sanholo-cloud/internal/provision"
)

type exportEnv struct {
	*env
	inst   *instancefake.Fake
	res    *fakeResolver
	svc    *export.Service
	clk    *testClock
	tenant string // ann's
}

// newExportEnv mounts the app with an export service in front of a fake bot
// that belongs to ann's tenant.
func newExportEnv(t *testing.T) (*exportEnv, *client) {
	t.Helper()
	e := newEnv(t, auth.Options{})
	e.account(t, "ann@example.org")
	var tenant string
	if err := e.pool.QueryRow(context.Background(), `SELECT tenant_id FROM accounts WHERE email = 'ann@example.org'`).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	inst := instancefake.New(t, pub, tenant)
	inst.SetEntries(
		instancefake.Entry{Name: "catalog.sqlite", Data: bytes.Repeat([]byte("CATALOG"), 5000)},
		instancefake.Entry{Name: "goals.json", Data: []byte("[]")},
	)
	res := &fakeResolver{addr: inst.Addr()}
	clk := &testClock{t: time.Now()}
	svc := export.New(export.Config{Pool: e.pool, BaseURL: baseURL, Key: priv, Resolver: res, Now: clk.now, BusyWait: time.Millisecond})
	e.app = New(e.pool, WithApp(&App{Auth: e.svc, Admin: e.admin, Export: svc}))
	x := &exportEnv{env: e, inst: inst, res: res, svc: svc, clk: clk, tenant: tenant}
	return x, e.signedIn(t, "ann@example.org")
}

// requestExport presses "Export my data" and returns the link path.
func requestExport(t *testing.T, c *client) string {
	t.Helper()
	r := c.post("/account/export", nil)
	if r.status != 200 {
		t.Fatalf("export: %d %s", r.status, r.body)
	}
	_, rest, ok := strings.Cut(r.body, baseURL)
	if !ok {
		t.Fatalf("no link on the page:\n%s", r.body)
	}
	path, _, _ := strings.Cut(rest, "\"")
	if i := strings.IndexAny(path, "<"); i >= 0 {
		path = path[:i]
	}
	return path
}

func tokenOf(link string) string { _, tok, _ := strings.Cut(link, "token="); return tok }

func zipEntries(t *testing.T, data string) map[string][]byte {
	t.Helper()
	zr, err := zip.NewReader(strings.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}
	out := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		_ = rc.Close()
		out[f.Name] = b
	}
	return out
}

func TestExportRoutesNeedASessionAndTheCSRFToken(t *testing.T) {
	x, c := newExportEnv(t)
	anon := x.client(t)
	for _, tc := range []struct{ method, path string }{
		{"POST", "/account/export"}, {"GET", "/account/export/download?token=x"}, {"POST", "/account/export/download"},
	} {
		var r result
		if tc.method == "POST" {
			r = anon.post(tc.path, url.Values{"token": {"x"}})
		} else {
			r = anon.get(tc.path, nil)
		}
		if r.status != 303 || r.header.Get("Location") != "/signin" {
			t.Errorf("anonymous %s %s = %d %q, want a redirect to sign in", tc.method, tc.path, r.status, r.header.Get("Location"))
		}
	}
	c.get("/", nil)
	if r := c.do("POST", "/account/export", url.Values{}, nil); r.status != 403 {
		t.Errorf("export without a CSRF token = %d, want 403", r.status)
	}
	if n := rows(t, x.pool, `SELECT count(*) FROM export_links`); n != 0 {
		t.Errorf("%d links were made without a session or CSRF token", n)
	}
	link := requestExport(t, c)
	if r := c.do("POST", "/account/export/download", url.Values{"token": {tokenOf(link)}}, nil); r.status != 403 {
		t.Errorf("download without a CSRF token = %d, want 403", r.status)
	}
	if ok, _ := x.svc.Valid(context.Background(), tokenOf(link), accountID(t, x.env, "ann@example.org")); !ok {
		t.Error("a request without a CSRF token spent the link")
	}
}

func accountID(t *testing.T, e *env, email string) string {
	t.Helper()
	var id string
	if err := e.pool.QueryRow(context.Background(), `SELECT id::text FROM accounts WHERE lower(email) = lower($1)`, email).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestExportShowsTheLinkOnceAndMailsIt(t *testing.T) {
	x, c := newExportEnv(t)
	before := c.get("/account", nil)
	if !strings.Contains(before.body, `action="/account/export"`) || !strings.Contains(before.body, "Export my data") {
		t.Errorf("the account page has no export button:\n%s", before.body)
	}
	link := requestExport(t, c)
	if !strings.HasPrefix(link, "/account/export/download?token=") || len(tokenOf(link)) < 40 {
		t.Fatalf("link = %q", link)
	}
	if r := c.post("/account/export", nil); !strings.Contains(r.body, "Your export is ready") {
		t.Error("the page does not say the export is ready")
	}
	var body string
	if err := x.pool.QueryRow(context.Background(), `SELECT body FROM mail_outbox WHERE to_email = 'ann@example.org' AND subject = 'Your SanHolo data export' ORDER BY id LIMIT 1`).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, baseURL+link) || !strings.Contains(body, "It expires in 24 hours") {
		t.Errorf("mail:\n%s", body)
	}
	// Shown once: not on the account page, not on a reload.
	if page := c.get("/account", nil); strings.Contains(page.body, tokenOf(link)) {
		t.Error("the link is on the account page")
	}
}

func TestExportIsRateLimitedPerAccount(t *testing.T) {
	x, c := newExportEnv(t)
	for i := range export.DailyLimit {
		if r := c.post("/account/export", nil); r.status != 200 {
			t.Fatalf("request %d: %d", i+1, r.status)
		}
	}
	r := c.post("/account/export", nil)
	if r.status != 429 || !strings.Contains(r.body, "3 exports a day") {
		t.Errorf("request %d = %d %s, want 429", export.DailyLimit+1, r.status, r.body)
	}
	if n := rows(t, x.pool, `SELECT count(*) FROM export_links`); n != export.DailyLimit {
		t.Errorf("%d links, want %d", n, export.DailyLimit)
	}
	// Another account has its own allowance.
	x.account(t, "bob@example.org")
	bob := x.signedIn(t, "bob@example.org")
	if r := bob.post("/account/export", nil); r.status != 200 {
		t.Errorf("another account is limited too: %d", r.status)
	}
}

func TestExportLinkPageIsSafeAgainstHEADAndPrefetch(t *testing.T) {
	x, c := newExportEnv(t)
	link := requestExport(t, c)
	acct := accountID(t, x.env, "ann@example.org")
	page := c.get(link, nil)
	for _, want := range []string{"Download", `action="/account/export/download"`, `name="token"`, `name="csrf"`, `hx-boost="false"`} {
		if page.status != 200 || !strings.Contains(page.body, want) {
			t.Errorf("link page (%d) lacks %q", page.status, want)
		}
	}
	for name, r := range map[string]result{
		"HEAD":            c.do("HEAD", link, nil, nil),
		"Sec-Purpose":     c.get(link, map[string]string{"Sec-Purpose": "prefetch"}),
		"Purpose":         c.get(link, map[string]string{"Purpose": "prefetch"}),
		"Sec-Purpose mix": c.get(link, map[string]string{"Sec-Purpose": "prefetch;anonymous-client-ip"}),
	} {
		if r.status != 204 || strings.Contains(r.body, "Download") {
			t.Errorf("%s: %d %q, want 204 and no page", name, r.status, r.body)
		}
	}
	if ok, _ := x.svc.Valid(context.Background(), tokenOf(link), acct); !ok {
		t.Fatal("looking at the link spent it")
	}
	if n := rows(t, x.pool, `SELECT count(*) FROM export_links WHERE used_at IS NOT NULL`); n != 0 {
		t.Error("a GET marked the link used")
	}
	if x.inst.Requests() != 0 {
		t.Error("a GET asked the bot for its data")
	}
}

func TestExportDownloadStreamsTheArchiveOnce(t *testing.T) {
	x, c := newExportEnv(t)
	link := requestExport(t, c)
	r := c.post("/account/export/download", url.Values{"token": {tokenOf(link)}})
	if r.status != 200 || r.header.Get("Content-Type") != "application/zip" ||
		!strings.HasPrefix(r.header.Get("Content-Disposition"), `attachment; filename="sanholo-export-`) ||
		r.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("download: %d %v", r.status, r.header)
	}
	entries := zipEntries(t, r.body)
	for _, n := range []string{"account.json", "instance/catalog.sqlite", "instance/goals.json", "README.txt"} {
		if _, ok := entries[n]; !ok {
			t.Errorf("the archive lacks %s", n)
		}
	}
	if !strings.Contains(string(entries["account.json"]), "ann@example.org") {
		t.Error("account.json lacks the email")
	}
	if x.inst.Requests() != 1 {
		t.Errorf("the bot was asked %d times", x.inst.Requests())
	}
	// The second use, by POST or by looking, is refused.
	for name, r := range map[string]result{
		"POST": c.post("/account/export/download", url.Values{"token": {tokenOf(link)}}),
		"GET":  c.get(link, nil),
	} {
		if r.status != 410 || !strings.Contains(r.body, "Link not valid") || strings.Contains(r.header.Get("Content-Type"), "zip") {
			t.Errorf("second %s = %d %s, want 410", name, r.status, r.body)
		}
	}
	if x.inst.Requests() != 1 {
		t.Error("a refused request asked the bot again")
	}
}

func TestExportLinkOfAnotherAccountIsRefused(t *testing.T) {
	x, ann := newExportEnv(t)
	link := requestExport(t, ann)
	x.account(t, "bob@example.org")
	bob := x.signedIn(t, "bob@example.org")
	if r := bob.get(link, nil); r.status != 410 || strings.Contains(r.body, `name="token"`) {
		t.Errorf("another account's GET = %d, want 410 and no download page", r.status)
	}
	if r := bob.post("/account/export/download", url.Values{"token": {tokenOf(link)}}); r.status != 410 || strings.Contains(r.header.Get("Content-Type"), "zip") {
		t.Errorf("another account's POST = %d %s, want 410", r.status, r.header.Get("Content-Type"))
	}
	// Ann can still use it.
	if r := ann.post("/account/export/download", url.Values{"token": {tokenOf(link)}}); r.status != 200 || !strings.Contains(r.header.Get("Content-Type"), "zip") {
		t.Errorf("the owner's POST = %d after the other account tried", r.status)
	}
}

func TestExportLinkExpiresAfter24Hours(t *testing.T) {
	x, c := newExportEnv(t)
	link := requestExport(t, c)
	x.clk.advance(24*time.Hour - time.Minute)
	if r := c.get(link, nil); r.status != 200 {
		t.Fatalf("one minute before expiry: %d", r.status)
	}
	x.clk.advance(2 * time.Minute)
	if r := c.get(link, nil); r.status != 410 {
		t.Errorf("GET after expiry = %d, want 410", r.status)
	}
	if r := c.post("/account/export/download", url.Values{"token": {tokenOf(link)}}); r.status != 410 || strings.Contains(r.header.Get("Content-Type"), "zip") {
		t.Errorf("POST after expiry = %d, want 410", r.status)
	}
}

func TestExportWithTheBotDownStillDownloads(t *testing.T) {
	x, c := newExportEnv(t)
	x.res.set(provision.ErrNotRunning)
	link := requestExport(t, c)
	r := c.post("/account/export/download", url.Values{"token": {tokenOf(link)}})
	entries := zipEntries(t, r.body)
	if r.status != 200 || entries["account.json"] == nil || !strings.Contains(string(entries["README.txt"]), "Your bot's data was not available") {
		t.Errorf("%d %v", r.status, entries)
	}
}

func TestAccountPageHasNoExportWithoutTheService(t *testing.T) {
	e := newEnv(t, auth.Options{})
	e.account(t, "ann@example.org")
	c := e.signedIn(t, "ann@example.org")
	if page := c.get("/account", nil); strings.Contains(page.body, `action="/account/export"`) {
		t.Error("the export button shows although no export service is configured")
	}
	if r := c.post("/account/export", nil); r.status != 503 {
		t.Errorf("export without a service = %d, want 503", r.status)
	}
}

func TestExportLinksAreDeletedADayAfterTheyExpire(t *testing.T) {
	x, c := newExportEnv(t)
	requestExport(t, c)
	x.svc2().SetClock(func() time.Time { return time.Now().Add(24*time.Hour + 23*time.Hour) })
	if err := x.svc2().Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := rows(t, x.pool, `SELECT count(*) FROM export_links`); n != 1 {
		t.Fatalf("%d links 23 hours after expiry, want the link kept for a day", n)
	}
	x.svc2().SetClock(func() time.Time { return time.Now().Add(24*time.Hour + 25*time.Hour) })
	if err := x.svc2().Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := rows(t, x.pool, `SELECT count(*) FROM export_links`); n != 0 {
		t.Errorf("%d links 25 hours after expiry, want none", n)
	}
}

// svc2 is the auth service (the export service is x.svc).
func (x *exportEnv) svc2() *auth.Service { return x.env.svc }
