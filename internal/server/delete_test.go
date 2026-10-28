package server

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lhaig/sanholo-cloud/internal/auth"
	"github.com/lhaig/sanholo-cloud/internal/connect"
	"github.com/lhaig/sanholo-cloud/internal/db"
	"github.com/lhaig/sanholo-cloud/internal/deletionlog"
	"github.com/lhaig/sanholo-cloud/internal/mail"
	"github.com/lhaig/sanholo-cloud/internal/nomad/nomadfake"
	"github.com/lhaig/sanholo-cloud/internal/provision"
	"github.com/lhaig/sanholo-cloud/internal/secrets/qmfake"
)

const annEmail = "ann@example.org"

// delEnv is a provisioning environment with the deletion log on and a mail
// worker over a fake mailer.
type delEnv struct {
	*provEnv
	logPath string
	mailer  *mail.Fake
}

func newDelEnv(t *testing.T) *delEnv {
	t.Helper()
	p := newProvEnv(t)
	d := &delEnv{provEnv: p, logPath: filepath.Join(t.TempDir(), "deletions.jsonl"), mailer: &mail.Fake{}}
	p.conn.SetDeletionLog(deletionlog.New(d.logPath))
	p.conn.SetLockWait(time.Second)
	return d
}

// sendMail runs the outbox once at the test clock.
func (d *delEnv) sendMail(t *testing.T) {
	t.Helper()
	// A minute ahead: the outbox stamps rows with the database's clock.
	w := &mail.Worker{Pool: d.pool, Mailer: d.mailer, Now: func() time.Time { return d.clock.now().Add(time.Minute) }}
	if _, err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (d *delEnv) passes() {
	// Past every backoff and the resume delay.
	d.clock.advance(10 * time.Minute)
	d.conn.RetryCleanups(context.Background())
}

// dump returns every row of every table as JSON text, keyed by table.
func dump(t *testing.T, pool *pgxpool.Pool) map[string][]string {
	t.Helper()
	ctx := context.Background()
	rs, err := pool.Query(ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema() AND table_type = 'BASE TABLE'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rs.Next() {
		var n string
		if err := rs.Scan(&n); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, n)
	}
	rs.Close()
	out := map[string][]string{}
	for _, tb := range tables {
		r, err := pool.Query(ctx, `SELECT row_to_json(x)::text FROM "`+tb+`" x`)
		if err != nil {
			t.Fatal(err)
		}
		for r.Next() {
			var s string
			if err := r.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out[tb] = append(out[tb], s)
		}
		r.Close()
	}
	return out
}

// where lists the tables whose rows mention needle (case-insensitive).
func where(d map[string][]string, needle string) []string {
	var out []string
	for tb, rs := range d {
		for _, r := range rs {
			if strings.Contains(strings.ToLower(r), strings.ToLower(needle)) {
				out = append(out, tb)
				break
			}
		}
	}
	return out
}

func exec(t *testing.T, pool *pgxpool.Pool, q string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// seedPersonalRows puts a row for the customer into every table that can
// hold one, so that a deletion that misses a table is seen.
func seedPersonalRows(t *testing.T, p *provEnv, tenant, email string) (accountID string) {
	t.Helper()
	accountID = accountID0(t, p.pool, email)
	hash := auth.HashEmail(email)
	exec(t, p.pool, `INSERT INTO waitlist (email) VALUES ($1)`, strings.ToUpper(email))
	exec(t, p.pool, `INSERT INTO invites (code_hash, email, expires_at) VALUES ('seed-invite', $1, now() + interval '1 day')`, email)
	exec(t, p.pool, `INSERT INTO mail_outbox (to_email, subject, body) VALUES ($1, 'an older mail', 'x')`, strings.ToUpper(email))
	exec(t, p.pool, `INSERT INTO signin_requests (email_hash) VALUES ($1)`, hash)
	for _, k := range []string{"signin:email:" + hash, "game:validate:account:" + accountID, "passkey:reg:" + accountID, "export:account:" + accountID} {
		exec(t, p.pool, `INSERT INTO rate_counters (key, window_start, count) VALUES ($1, now(), 1)`, k)
	}
	exec(t, p.pool, `INSERT INTO passkeys (id, account_id, credential) VALUES ('\x0102'::bytea, $1, '{}')`, accountID)
	exec(t, p.pool, `INSERT INTO push_subscriptions (tenant_id, account_id, endpoint, p256dh, auth) VALUES ($1, $2, 'https://push.example/seed', 'k', 'a')`, tenant, accountID)
	exec(t, p.pool, `INSERT INTO export_links (token_hash, account_id, tenant_id, expires_at) VALUES ('seed-link', $1, $2, now() + interval '1 day')`, accountID, tenant)
	exec(t, p.pool, `INSERT INTO magic_links (token_hash, account_id, expires_at) VALUES ('seed-magic', $1, now() + interval '1 day')`, accountID)
	exec(t, p.pool, `INSERT INTO webauthn_challenges (id_hash, kind, account_id, data, expires_at) VALUES ('seed-ch', 'login', $1, '{}', now() + interval '1 day')`, accountID)
	var sid string
	if err := p.pool.QueryRow(context.Background(), `SELECT id::text FROM account_sessions WHERE account_id = $1 LIMIT 1`, accountID).Scan(&sid); err != nil {
		t.Fatal(err)
	}
	exec(t, p.pool, `INSERT INTO bridge_sessions (secret_hash, account_session_id, expires_at) VALUES ('seed-bs', $1, now() + interval '1 day')`, sid)
	exec(t, p.pool, `INSERT INTO bridge_handoffs (code_hash, account_session_id, expires_at) VALUES ('seed-bh', $1, now() + interval '1 day')`, sid)
	exec(t, p.pool, `INSERT INTO releases (version) VALUES ('9.9.9') ON CONFLICT DO NOTHING`)
	exec(t, p.pool, `INSERT INTO rollout_tenants (release_version, tenant_id, batch, previous_version) VALUES ('9.9.9', $1, 0, '0.7.1')`, tenant)

	// The deletion tests mean something only if each kind of row is there.
	all := dump(t, p.pool)
	for needle, tables := range map[string][]string{
		email:     {"accounts", "waitlist", "invites", "mail_outbox"},
		hash:      {"accounts", "signin_requests", "rate_counters"},
		accountID: {"rate_counters", "passkeys", "push_subscriptions", "export_links", "magic_links", "webauthn_challenges", "account_sessions"},
		tenant:    {"accounts", "game_accounts", "tenant_instances", "push_subscriptions", "export_links", "rollout_tenants"},
	} {
		have := strings.Join(where(all, needle), ",")
		for _, tb := range tables {
			if !strings.Contains(","+have+",", ","+tb+",") {
				t.Fatalf("the fixture has no row for %q in %s (found in %s)", needle, tb, have)
			}
		}
	}
	return accountID
}

func accountID0(t *testing.T, pool *pgxpool.Pool, email string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `SELECT id::text FROM accounts WHERE lower(email) = lower($1)`, email).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func tenantRows(t *testing.T, pool *pgxpool.Pool, tenant string) map[string]int {
	t.Helper()
	tables, err := db.TenantTables(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, tb := range tables {
		out[tb] = rows(t, pool, `SELECT count(*) FROM "`+tb+`" WHERE tenant_id = $1`, tenant)
	}
	return out
}

func deletion(t *testing.T, pool *pgxpool.Pool, tenant string) (state, detail string, completed bool, accountSet bool) {
	t.Helper()
	var done *time.Time
	var acct *string
	if err := pool.QueryRow(context.Background(),
		`SELECT check_state, check_detail, completed_at, deleted_account::text FROM tenant_deletions WHERE tenant_key = $1`, tenant).Scan(&state, &detail, &done, &acct); err != nil {
		t.Fatalf("no deletion record for %s: %v", tenant, err)
	}
	return state, detail, done != nil, acct != nil
}

func deleteForm(c *client, email string) result {
	return c.post("/account/delete", url.Values{"email": {email}})
}

func TestDeleteConfirmationPageStatesWhatHappensAndChangesNothingOnGET(t *testing.T) {
	d := newDelEnv(t)
	c, tenant, job := d.connected(t)
	page := c.get("/account", nil)
	if !strings.Contains(page.body, `href="/account/delete"`) {
		t.Errorf("the account page has no way to delete:\n%s", page.body)
	}
	r := c.get("/account/delete", nil)
	text := html.UnescapeString(r.body)
	for _, want := range []string{
		"Delete your account", `action="/account/delete"`, `name="csrf"`, `name="email"`,
		"Your Replicant Space account is not deleted",
		"Encrypted backups made before the deletion keep your data until they expire, at most 30 days",
		"A deleted account is not restored from a backup",
		"records it and retries until it succeeds",
		"permanent",
	} {
		if r.status != 200 || !strings.Contains(text, want) {
			t.Errorf("confirmation page (%d) lacks %q", r.status, want)
		}
	}
	for name, r := range map[string]result{
		"HEAD":        c.do("HEAD", "/account/delete", nil, nil),
		"prefetch":    c.get("/account/delete", map[string]string{"Sec-Purpose": "prefetch"}),
		"Purpose hdr": c.get("/account/delete", map[string]string{"Purpose": "prefetch"}),
	} {
		if r.status != 204 || strings.Contains(r.body, "Delete your account") {
			t.Errorf("%s: %d, want 204 and no page", name, r.status)
		}
	}
	if st := scalar(t, d.pool, `SELECT status FROM tenants WHERE id = $1`, tenant); st == "deleting" {
		t.Error("a GET started the deletion")
	}
	if exists, stopped := d.fake.Has(job); !exists || stopped {
		t.Error("a GET stopped the bot")
	}
}

func scalar(t *testing.T, pool *pgxpool.Pool, q string, args ...any) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(), q, args...).Scan(&s); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return s
}

func TestDeleteNeedsASessionAndTheCSRFToken(t *testing.T) {
	d := newDelEnv(t)
	c, tenant, _ := d.connected(t)
	if r := newGameEnv(t).client(t).post("/account/delete", url.Values{"email": {annEmail}}); r.status != 303 || r.header.Get("Location") != "/signin" {
		t.Errorf("anonymous delete = %d %q, want a redirect to sign in", r.status, r.header.Get("Location"))
	}
	c.get("/", nil)
	if r := c.do("POST", "/account/delete", url.Values{"email": {annEmail}}, nil); r.status != 403 {
		t.Errorf("delete without a CSRF token = %d, want 403", r.status)
	}
	if st := scalar(t, d.pool, `SELECT status FROM tenants WHERE id = $1`, tenant); st != "active" && st != "pending" {
		t.Errorf("tenant status = %q after a refused request", st)
	}
}

func TestDeleteWithAWrongAddressChangesNothing(t *testing.T) {
	d := newDelEnv(t)
	c, tenant, job := d.connected(t)
	d.account(t, "bob@example.org")
	before := dump(t, d.pool)
	for name, typed := range map[string]string{
		"empty":                "",
		"another account":      "bob@example.org",
		"a longer address":     "xann@example.org",
		"a prefix":             "ann@example",
		"the name only":        "ann",
		"a different domain":   "ann@example.com",
		"with a sub-address":   "ann+x@example.org",
		"surrounding wildcard": "%ann@example.org%",
	} {
		r := deleteForm(c, typed)
		if r.status != 400 || !strings.Contains(r.body, "Nothing was changed") {
			t.Errorf("%s: %d %s, want 400", name, r.status, r.body)
		}
	}
	after := dump(t, d.pool)
	for _, tb := range []string{"tenants", "accounts", "game_accounts", "tenant_instances", "account_sessions", "audit_log", "pending_cleanups"} {
		if strings.Join(before[tb], "\n") != strings.Join(after[tb], "\n") {
			t.Errorf("table %s changed after refused requests", tb)
		}
	}
	if exists, stopped := d.fake.Has(job); !exists || stopped {
		t.Error("the bot was stopped by a refused request")
	}
	if _, ok := d.qm.Stored(qmPath(tenant)); !ok {
		t.Error("the token was purged by a refused request")
	}
	if _, err := os.Stat(d.logPath); err == nil {
		t.Error("a refused request wrote to the deletion log")
	}
	if d.fake.Count("DELETE", "/v1/") != 0 {
		t.Error("a refused request sent a delete to Nomad")
	}
	// The right address, in another case and with spaces around it, works.
	if r := deleteForm(c, "  ANN@Example.ORG "); r.status != 200 {
		t.Errorf("the right address = %d %s", r.status, r.body)
	}
}

func TestDeleteAccountRefusesAnAccountThatIsNotTheTenants(t *testing.T) {
	d := newDelEnv(t)
	_, tenant, job := d.connected(t)
	d.account(t, "bob@example.org")
	bobID := accountID0(t, d.pool, "bob@example.org")
	// Bob's account id with Ann's tenant and address: the account does not
	// belong to the tenant, so nothing may happen.
	if _, err := d.conn.DeleteAccount(context.Background(), bobID, tenant, annEmail); err == nil {
		t.Fatal("an account deleted another account's tenant")
	}
	if st := scalar(t, d.pool, `SELECT status FROM tenants WHERE id = $1`, tenant); st == "deleting" {
		t.Error("the tenant was marked deleting")
	}
	if exists, stopped := d.fake.Has(job); !exists || stopped {
		t.Error("the bot was stopped")
	}
}

func TestDeleteAccountRemovesEverythingInOrder(t *testing.T) {
	d := newDelEnv(t)
	c, tenant, job := d.connected(t)
	bobC, bobTenant := d.user(t, "bob@example.org")
	_ = bobC
	acct := seedPersonalRows(t, d.provEnv, tenant, annEmail)
	bobBefore := bobSnapshot(t, d.pool, bobTenant)

	// What the tenant looks like when Nomad is asked to stop the job: it is
	// marked deleting already and nothing has been removed yet.
	var atStop string
	var once sync.Once
	d.fake.Override(func(r nomadfake.Request) (int, string, bool) {
		if r.Method == "DELETE" && strings.HasPrefix(r.Path, "/v1/job/") {
			once.Do(func() {
				atStop = scalar(t, d.pool, `SELECT status || ':' || (SELECT count(*) FROM accounts WHERE tenant_id = $1)::text || ':' || (SELECT count(*) FROM game_accounts WHERE tenant_id = $1)::text FROM tenants WHERE id = $1`, tenant)
			})
		}
		return 0, "", false
	})

	hdr := map[string]string{}
	_ = hdr
	if c.cookies[sessionCookie] == nil {
		t.Fatal("the customer is not signed in before the deletion")
	}
	oldSession := c.cookies[sessionCookie].Value
	r := deleteForm(c, annEmail)
	text := html.UnescapeString(r.body)
	if r.status != 200 || !strings.Contains(r.body, "Account deleted") {
		t.Fatalf("delete: %d %s", r.status, r.body)
	}
	for _, want := range []string{
		"Your game token is deleted from the secrets store.",
		"Your Replicant Space account is not deleted.",
		"keep your data until they expire, at most 30 days",
		"SanHolo emailed a confirmation to " + annEmail,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the page lacks %q:\n%s", want, text)
		}
	}
	if atStop != "deleting:1:1" {
		t.Errorf("when the bot was stopped the tenant was %q, want deleting with its rows intact", atStop)
	}

	// Stop, purge, then the volume; the token is gone.
	var order []string
	for _, rq := range d.fake.Requests() {
		switch {
		case rq.Method == "DELETE" && strings.HasPrefix(rq.Path, "/v1/job/"):
			if strings.Contains(rq.Query, "purge=true") {
				order = append(order, "purge")
			} else {
				order = append(order, "stop")
			}
		case rq.Method == "DELETE" && strings.HasPrefix(rq.Path, "/v1/volume/host/"):
			order = append(order, "volume")
		}
	}
	if strings.Join(order, ",") != "stop,purge,volume" {
		t.Errorf("Nomad calls = %v, want stop, purge, volume", order)
	}
	if exists, _ := d.fake.Has(job); exists || len(d.fake.Volumes()) != 0 {
		t.Errorf("job exists=%v volumes=%v", exists, d.fake.Volumes())
	}
	if _, ok := d.qm.Stored(qmPath(tenant)); ok {
		t.Error("the token is still in qm")
	}

	// Every table the schema gives a tenant_id column is empty for the tenant.
	counts := tenantRows(t, d.pool, tenant)
	if len(counts) < 6 {
		t.Fatalf("only %d tenant tables found (%v): the schema query broke", len(counts), counts)
	}
	for tb, n := range counts {
		if n != 0 {
			t.Errorf("%d rows left in %s", n, tb)
		}
	}
	// The tombstone is gone once everything verified.
	if n := rows(t, d.pool, `SELECT count(*) FROM tenants WHERE id = $1`, tenant); n != 0 {
		t.Error("the tenants row is still there although every check passed")
	}
	state, _, completed, acctSet := deletion(t, d.pool, tenant)
	if state != "passed" || !completed || acctSet {
		t.Errorf("deletion record: state=%s completed=%v account kept=%v", state, completed, acctSet)
	}
	// Nothing of the customer is left anywhere, except the confirmation
	// email that is addressed to them.
	all := dump(t, d.pool)
	for what, needle := range map[string]string{"email": annEmail, "email hash": auth.HashEmail(annEmail)} {
		for _, tb := range where(all, needle) {
			if tb != "mail_outbox" {
				t.Errorf("the %s is still in %s", what, tb)
			}
		}
	}
	for _, tb := range where(all, acct) {
		if tb != "audit_log" {
			t.Errorf("the account id is still in %s", tb)
		}
	}
	for _, tb := range where(all, tenant) {
		if tb != "audit_log" && tb != "tenant_deletions" {
			t.Errorf("the tenant id is still in %s", tb)
		}
	}
	// The confirmation is the only mail left for the address.
	var n int
	var subject string
	if err := d.pool.QueryRow(context.Background(), `SELECT count(*), coalesce(max(subject), '') FROM mail_outbox WHERE lower(to_email) = lower($1)`, annEmail).Scan(&n, &subject); err != nil {
		t.Fatal(err)
	}
	if n != 1 || subject != "Your SanHolo account is deleted" {
		t.Errorf("%d mails for the address (%q), want only the confirmation", n, subject)
	}
	d.sendMail(t)
	var sent []mail.Message
	for _, m := range d.mailer.Sent() {
		if strings.EqualFold(m.To, annEmail) {
			sent = append(sent, m)
		}
	}
	if len(sent) != 1 {
		t.Fatalf("sent to the customer: %+v", sent)
	}
	for _, want := range []string{"is deleted", "Your Replicant Space account is not deleted", "within 30 days", "not restored from a backup"} {
		if !strings.Contains(sent[0].Body, want) {
			t.Errorf("the confirmation lacks %q:\n%s", want, sent[0].Body)
		}
	}
	// Signed out: the cookie is cleared and the old one is dead.
	if ck := c.cookies[sessionCookie]; ck != nil && ck.Value != "" {
		t.Errorf("the response did not clear the session cookie: %v", r.header["Set-Cookie"])
	}
	// The old cookie value is dead on the server too, not just forgotten by the browser.
	replay := d.client(t)
	replay.cookies[sessionCookie] = &http.Cookie{Name: sessionCookie, Value: oldSession}
	if got := replay.get("/account", nil); got.status != 303 || got.header.Get("Location") != "/signin" {
		t.Errorf("replaying the old session cookie = %d, want a redirect to sign in", got.status)
	}
	if got := c.get("/account", nil); got.status != 303 || got.header.Get("Location") != "/signin" {
		t.Errorf("/account after the deletion = %d %q, want a redirect to sign in", got.status, got.header.Get("Location"))
	}
	if n := rows(t, d.pool, `SELECT count(*) FROM account_sessions WHERE account_id = $1`, acct); n != 0 {
		t.Error("the sessions survived")
	}
	// The audit row names ids, never the address.
	if n := rows(t, d.pool, `SELECT count(*) FROM audit_log WHERE action = 'account.delete' AND actor = $1 AND target = $2`, acct, "tenant:"+tenant); n != 1 {
		t.Errorf("%d account.delete audit rows, want 1", n)
	}
	for _, line := range all["audit_log"] {
		low := strings.ToLower(line)
		if strings.Contains(low, "ann@") || strings.Contains(low, strings.ToLower(auth.HashEmail(annEmail))) {
			t.Errorf("an audit row holds personal data: %s", line)
		}
	}
	// The deletion log has the ids and nothing else.
	b, err := os.ReadFile(d.logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), tenant) || !strings.Contains(string(b), acct) || strings.Contains(strings.ToLower(string(b)), "ann@") {
		t.Errorf("deletion log: %s", b)
	}
	// Nobody else was touched.
	if got := bobSnapshot(t, d.pool, bobTenant); got != bobBefore {
		t.Errorf("another customer's rows changed:\nbefore: %s\nafter:  %s", bobBefore, got)
	}
}

func bobSnapshot(t *testing.T, pool *pgxpool.Pool, tenant string) string {
	t.Helper()
	all := dump(t, pool)
	var parts []string
	for _, tb := range []string{"tenants", "accounts", "account_sessions", "waitlist", "invites"} {
		for _, r := range all[tb] {
			if strings.Contains(r, "bob@") || strings.Contains(r, tenant) {
				parts = append(parts, tb+":"+r)
			}
		}
	}
	return fmt.Sprint(len(parts)) + "|" + strings.Join(parts, "|")
}

// qm cannot delete yet (Part E): the account is deleted, the token is still
// there, the tombstone stays and the check says so. Later the purge works and
// the tombstone goes.
func TestPendingQMPurgeKeepsTheTombstoneAndTheCheckReportsIt(t *testing.T) {
	d := newDelEnv(t)
	c, tenant, _ := d.connected(t)
	d.qm.SetDeleteAllowed(false)
	r := deleteForm(c, annEmail)
	text := html.UnescapeString(r.body)
	if r.status != 200 || !strings.Contains(text, "Deleting your game token in the secrets store is not finished yet") ||
		strings.Contains(text, "Your game token is deleted from the secrets store.") {
		t.Fatalf("delete: %d %s", r.status, text)
	}
	if n := rows(t, d.pool, `SELECT count(*) FROM tenants WHERE id = $1 AND status = 'deleting'`, tenant); n != 1 {
		t.Fatal("the tombstone is gone although the token is still in qm")
	}
	for tb, n := range tenantRows(t, d.pool, tenant) {
		if n != 0 {
			t.Errorf("%d rows left in %s", n, tb)
		}
	}
	if n := rows(t, d.pool, `SELECT count(*) FROM pending_cleanups WHERE tenant_key = $1 AND kind = 'secret'`, tenant); n != 1 {
		t.Error("the purge is not recorded")
	}
	state, detail, completed, _ := deletion(t, d.pool, tenant)
	if state != "pending" || !strings.Contains(detail, "qm purge pending") || completed {
		t.Errorf("check = %s %q completed=%v, want pending with the qm purge", state, detail, completed)
	}
	// The tombstone holds no personal data.
	for _, tb := range where(dump(t, d.pool), annEmail) {
		if tb != "mail_outbox" {
			t.Errorf("the address is in %s", tb)
		}
	}
	// The operator sees it.
	op, _ := operator(t, d.env, "op@example.org")
	page := op.get("/admin", nil)
	for _, want := range []string{tenant, "Account deletions", "qm purge pending", "being deleted", "secret"} {
		if !strings.Contains(page.body, want) {
			t.Errorf("/admin lacks %q", want)
		}
	}
	// Every pass keeps saying so while qm refuses.
	d.passes()
	if state, detail, _, _ := deletion(t, d.pool, tenant); state != "pending" || !strings.Contains(detail, "qm purge pending") {
		t.Errorf("after a pass: %s %q", state, detail)
	}
	// Production qm gets the Part E purge route.
	d.qm.SetDeleteAllowed(true)
	d.passes()
	if _, ok := d.qm.Stored(qmPath(tenant)); ok {
		t.Error("the token is still in qm after the purge worked")
	}
	state, _, completed, acctSet := deletion(t, d.pool, tenant)
	if state != "passed" || !completed || acctSet {
		t.Errorf("check = %s completed=%v account kept=%v", state, completed, acctSet)
	}
	if n := rows(t, d.pool, `SELECT count(*) FROM tenants WHERE id = $1`, tenant); n != 0 {
		t.Error("the tombstone is still there after everything passed")
	}
	if n := rows(t, d.pool, `SELECT count(*) FROM pending_cleanups WHERE tenant_key = $1`, tenant); n != 0 {
		t.Error("a finished deletion left a pending record")
	}
}

// Something keyed by the address that appears after the rows were deleted
// (here: while the volume is deleted) is found by the check that runs right
// after the deletion.
func TestTheCheckAfterTheDeletionFindsALateRowKeyedByTheAddress(t *testing.T) {
	d := newDelEnv(t)
	c, tenant, _ := d.connected(t)
	var once sync.Once
	d.fake.Override(func(r nomadfake.Request) (int, string, bool) {
		if r.Method == "DELETE" && strings.HasPrefix(r.Path, "/v1/volume/host/") {
			once.Do(func() { exec(t, d.pool, `INSERT INTO waitlist (email) VALUES ($1)`, annEmail) })
		}
		return 0, "", false
	})
	if r := deleteForm(c, annEmail); r.status != 200 {
		t.Fatalf("delete: %d %s", r.status, r.body)
	}
	state, detail, completed, _ := deletion(t, d.pool, tenant)
	if state != "failed" || !strings.Contains(detail, "rows remain in waitlist") || completed {
		t.Errorf("check = %s %q completed=%v, want failed with the waitlist row", state, detail, completed)
	}
}

func TestVolumeNomadRefusesIsPendingThenCompletes(t *testing.T) {
	d := newDelEnv(t)
	c, tenant, _ := d.connected(t)
	refuse := true
	var mu sync.Mutex
	d.fake.Override(func(r nomadfake.Request) (int, string, bool) {
		mu.Lock()
		defer mu.Unlock()
		if refuse && r.Method == "DELETE" && strings.HasPrefix(r.Path, "/v1/volume/host/") {
			return http.StatusInternalServerError, "volume busy", true
		}
		return 0, "", false
	})
	r := deleteForm(c, annEmail)
	if r.status != 200 || !strings.Contains(html.UnescapeString(r.body), "Deleting your bot's data volume is not finished yet") {
		t.Fatalf("delete: %d %s", r.status, r.body)
	}
	state, detail, _, _ := deletion(t, d.pool, tenant)
	if state != "pending" || !strings.Contains(detail, "volume deletion pending") {
		t.Errorf("check = %s %q", state, detail)
	}
	if n := rows(t, d.pool, `SELECT count(*) FROM tenants WHERE id = $1`, tenant); n != 1 {
		t.Error("the tombstone is gone while the volume exists")
	}
	mu.Lock()
	refuse = false
	mu.Unlock()
	d.passes()
	if state, _, completed, _ := deletion(t, d.pool, tenant); state != "passed" || !completed || len(d.fake.Volumes()) != 0 {
		t.Errorf("after the volume went: %s completed=%v volumes=%v", state, completed, d.fake.Volumes())
	}
}

// pendingTombstone deletes ann's account with qm refusing, so that the
// tombstone stays, and returns what a check needs.
func pendingTombstone(t *testing.T) (d *delEnv, tenant, acct string) {
	t.Helper()
	d = newDelEnv(t)
	c, tenant, _ := d.connected(t)
	acct = accountID0(t, d.pool, annEmail)
	d.qm.SetDeleteAllowed(false)
	if r := deleteForm(c, annEmail); r.status != 200 {
		t.Fatalf("delete: %d %s", r.status, r.body)
	}
	return d, tenant, acct
}

// The check job finds what should be gone: each case plants one leftover and
// the next pass must report it (and fail, not pass).
func TestTheCheckJobFindsLeftovers(t *testing.T) {
	cases := []struct {
		name   string
		plant  func(t *testing.T, d *delEnv, tenant, acct string)
		expect string
	}{
		{"a row in a table with tenant_id", func(t *testing.T, d *delEnv, tenant, _ string) {
			exec(t, d.pool, `INSERT INTO game_accounts (tenant_id, game_account_id, replicant_name, replicant_code) VALUES ($1, 'x', 'n', 'c')`, tenant)
		}, "rows remain in game_accounts"},
		{"a rate-limit row keyed by the account id", func(t *testing.T, d *delEnv, _, acct string) {
			exec(t, d.pool, `INSERT INTO rate_counters (key, window_start, count) VALUES ('game:validate:account:'||$1, now(), 1)`, acct)
		}, "rows remain in rate_counters"},
		{"a table keyed by the account id added later", func(t *testing.T, d *delEnv, _, acct string) {
			exec(t, d.pool, `CREATE TABLE zz_owned (account_id uuid NOT NULL, note text)`)
			exec(t, d.pool, `INSERT INTO zz_owned VALUES ($1, 'x')`, acct)
		}, "rows remain in zz_owned"},
		{"a table added later", func(t *testing.T, d *delEnv, tenant, _ string) {
			exec(t, d.pool, `CREATE TABLE zz_later (tenant_id text NOT NULL REFERENCES tenants (id), note text)`)
			exec(t, d.pool, `INSERT INTO zz_later VALUES ($1, 'x')`, tenant)
		}, "rows remain in zz_later"},
		{"the token in qm without a recorded purge", func(t *testing.T, d *delEnv, tenant, _ string) {
			exec(t, d.pool, `DELETE FROM pending_cleanups WHERE tenant_key = $1`, tenant)
			d.qm.Seed(qmPath(tenant), map[string]string{"api_token": "x", "replicant_code": "y"})
		}, "the token is still in qm"},
		{"the Nomad job", func(t *testing.T, d *delEnv, tenant, _ string) {
			exec(t, d.pool, `DELETE FROM pending_cleanups WHERE tenant_key = $1 AND kind = 'secret'`, tenant)
			d.fake.Override(func(r nomadfake.Request) (int, string, bool) {
				if r.Method == "GET" && r.Path == "/v1/job/sanholo-tenant-"+tenant {
					return 200, `{"ID":"sanholo-tenant-` + tenant + `","Status":"running","Version":3}`, true
				}
				return 0, "", false
			})
		}, "the Nomad job still exists"},
		{"the Nomad volume", func(t *testing.T, d *delEnv, tenant, _ string) {
			exec(t, d.pool, `DELETE FROM pending_cleanups WHERE tenant_key = $1 AND kind = 'secret'`, tenant)
			d.fake.AddVolume("sanholo-tenant-" + tenant)
		}, "the Nomad volume still exists"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, tenant, acct := pendingTombstone(t)
			tc.plant(t, d, tenant, acct)
			d.passes()
			state, detail, completed, _ := deletion(t, d.pool, tenant)
			if state != "failed" || !strings.Contains(detail, tc.expect) || completed {
				t.Errorf("check = %s %q completed=%v, want failed with %q", state, detail, completed, tc.expect)
			}
			if n := rows(t, d.pool, `SELECT count(*) FROM tenants WHERE id = $1`, tenant); n != 1 {
				t.Error("the tombstone was removed although a check failed")
			}
		})
	}
}

// A leftover the cleanups can remove is queued again, so the check repairs.
func TestTheCheckJobRequeuesWhatItFindsUndone(t *testing.T) {
	d, tenant, _ := pendingTombstone(t)
	exec(t, d.pool, `DELETE FROM pending_cleanups WHERE tenant_key = $1`, tenant)
	d.qm.Seed(qmPath(tenant), map[string]string{"api_token": "x", "replicant_code": "y"})
	d.qm.SetDeleteAllowed(true)
	d.passes() // finds the token: failed, secret queued again
	if n := rows(t, d.pool, `SELECT count(*) FROM pending_cleanups WHERE tenant_key = $1 AND kind = 'secret'`, tenant); n != 1 {
		t.Error("the leftover token was not queued for deletion")
	}
	d.passes() // purges it and verifies
	if _, ok := d.qm.Stored(qmPath(tenant)); ok {
		t.Error("the token is still in qm")
	}
	if state, _, completed, _ := deletion(t, d.pool, tenant); state != "passed" || !completed {
		t.Errorf("after the repair: %s completed=%v", state, completed)
	}
}

func TestTablesAddedLaterAreDeletedWithoutChangingTheCode(t *testing.T) {
	d := newDelEnv(t)
	c, tenant, _ := d.connected(t)
	acct := accountID0(t, d.pool, annEmail)
	exec(t, d.pool, `CREATE TABLE zz_later (tenant_id text NOT NULL REFERENCES tenants (id), note text)`)
	// No foreign key: only its column name ties it to the account.
	exec(t, d.pool, `CREATE TABLE zz_by_account (account_id uuid NOT NULL, note text)`)
	exec(t, d.pool, `INSERT INTO zz_later VALUES ($1, 'x')`, tenant)
	exec(t, d.pool, `INSERT INTO zz_by_account VALUES ($1, 'x')`, acct)
	if r := deleteForm(c, annEmail); r.status != 200 {
		t.Fatalf("delete: %d %s", r.status, r.body)
	}
	if n := rows(t, d.pool, `SELECT count(*) FROM zz_later`) + rows(t, d.pool, `SELECT count(*) FROM zz_by_account`); n != 0 {
		t.Errorf("%d rows of tables added later survived", n)
	}
}

func TestADeletionWaitsForTheTenantLockAndTimesOutCleanly(t *testing.T) {
	d := newDelEnv(t)
	c, tenant, job := d.connected(t)
	d.conn.SetLockWait(200 * time.Millisecond)
	unlock, err := d.prov.LockTenant(context.Background(), tenant)
	if err != nil {
		t.Fatal(err)
	}
	r := deleteForm(c, annEmail)
	if r.status != 503 || !strings.Contains(r.body, "Nothing was changed") {
		t.Errorf("with the lock held: %d %s", r.status, r.body)
	}
	if st := scalar(t, d.pool, `SELECT status FROM tenants WHERE id = $1`, tenant); st == "deleting" {
		t.Error("the tenant was marked deleting without the lock")
	}
	if exists, stopped := d.fake.Has(job); !exists || stopped {
		t.Error("the bot was stopped without the lock")
	}
	if _, err := os.Stat(d.logPath); err == nil {
		t.Error("a deletion that did not start wrote to the deletion log")
	}
	unlock()
	d.conn.SetLockWait(5 * time.Second)
	if r := deleteForm(c, annEmail); r.status != 200 {
		t.Errorf("after the lock was released: %d %s", r.status, r.body)
	}
}

// A start that was waiting for the tenant's lock while the deletion ran must
// not bring the bot back afterwards.
func TestAStartDuringTheDeletionDoesNotBringTheBotBack(t *testing.T) {
	d := newDelEnv(t)
	c, tenant, job := d.connected(t)
	startErr := make(chan error, 1)
	var once sync.Once
	d.fake.Override(func(r nomadfake.Request) (int, string, bool) {
		if r.Method == "DELETE" && strings.HasPrefix(r.Path, "/v1/job/") {
			once.Do(func() {
				// The deletion holds the lock right now: this start waits.
				go func() { startErr <- d.prov.Start(context.Background(), tenant) }()
				time.Sleep(100 * time.Millisecond)
			})
		}
		return 0, "", false
	})
	if r := deleteForm(c, annEmail); r.status != 200 {
		t.Fatalf("delete: %d %s", r.status, r.body)
	}
	select {
	case err := <-startErr:
		if err == nil {
			t.Error("a start that raced the deletion succeeded")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the racing start never returned")
	}
	if exists, _ := d.fake.Has(job); exists {
		t.Error("the job is registered again after the deletion")
	}
	if n := rows(t, d.pool, `SELECT count(*) FROM tenant_instances WHERE tenant_id = $1`, tenant); n != 0 {
		t.Error("the instance row came back")
	}
	if len(d.fake.Volumes()) != 0 {
		t.Errorf("volumes = %v after the deletion", d.fake.Volumes())
	}
}

// A connect that was waiting for confirmation when the deletion marked the
// tenant must write nothing, to qm or to the database.
func TestAConfirmedConnectAfterTheMarkWritesNothing(t *testing.T) {
	g := newGameEnv(t)
	c, tenant := g.user(t, annEmail)
	if r := validate(c, tokenA); r.status != 200 {
		t.Fatalf("validate: %d %s", r.status, r.body)
	}
	exec(t, g.pool, `UPDATE tenants SET status = 'deleting' WHERE id = $1`, tenant)
	r := c.post("/account/game/confirm", nil)
	if r.status != 409 || !strings.Contains(r.body, "being deleted") {
		t.Errorf("confirm after the mark: %d %s, want 409", r.status, r.body)
	}
	if g.rowCount(t) != 0 {
		t.Error("a game account row was written for a deleting tenant")
	}
	if _, ok := g.qm.Stored(qmPath(tenant)); ok || g.qm.Count("POST", "/qm/v1/secret/data/") != 0 {
		t.Error("a token was written to qm for a deleting tenant")
	}
}

// A token held in memory for a confirmation does not outlive the account.
func TestAPendingConnectionIsDroppedWithTheAccount(t *testing.T) {
	d := newDelEnv(t)
	c, tenant, _ := d.connected(t)
	acct := accountID0(t, d.pool, annEmail)
	_, secret, err := d.conn.Begin(context.Background(), acct, tenant, tokenA)
	if err != nil {
		t.Fatal(err)
	}
	if r := deleteForm(c, annEmail); r.status != 200 {
		t.Fatalf("delete: %d %s", r.status, r.body)
	}
	if _, err := d.conn.Confirm(context.Background(), acct, tenant, secret); !errors.Is(err, connect.ErrExpired) {
		t.Errorf("Confirm after the deletion: %v, want ErrExpired", err)
	}
}

func TestTwoDeletionsAtOnceDeleteOnceAndMailOnce(t *testing.T) {
	d := newDelEnv(t)
	c, tenant, _ := d.connected(t)
	c2 := &client{t: t, app: c.app, cookies: map[string]*http.Cookie{}, host: c.host}
	for k, v := range c.cookies {
		c2.cookies[k] = v
	}
	var wg sync.WaitGroup
	statuses := make([]int, 2)
	for i, cl := range []*client{c, c2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i] = deleteForm(cl, annEmail).status
		}()
	}
	wg.Wait()
	ok := 0
	for _, s := range statuses {
		if s == 200 {
			ok++
		}
	}
	if ok != 1 {
		t.Errorf("statuses %v, want exactly one 200", statuses)
	}
	if n := rows(t, d.pool, `SELECT count(*) FROM mail_outbox WHERE subject = 'Your SanHolo account is deleted'`); n != 1 {
		t.Errorf("%d confirmation mails, want 1", n)
	}
	if n := rows(t, d.pool, `SELECT count(*) FROM audit_log WHERE action = 'account.delete' AND target = $1`, "tenant:"+tenant); n != 1 {
		t.Errorf("%d audit rows, want 1", n)
	}
	if n := rows(t, d.pool, `SELECT count(*) FROM accounts WHERE tenant_id = $1`, tenant); n != 0 {
		t.Error("the account survived")
	}
}

// Nomad refuses to stop the job: the tenant is marked deleting (its Bridge
// is closed), nothing else is removed, and the background pass finishes the
// deletion once Nomad answers.
func TestADeletionThatCannotStopTheBotIsFinishedLater(t *testing.T) {
	d := newDelEnv(t)
	c, tenant, job := d.connected(t)
	var mu sync.Mutex
	broken := true
	d.fake.Override(func(r nomadfake.Request) (int, string, bool) {
		mu.Lock()
		defer mu.Unlock()
		if broken && r.Method == "DELETE" && strings.HasPrefix(r.Path, "/v1/job/") {
			return http.StatusInternalServerError, "boom", true
		}
		return 0, "", false
	})
	r := deleteForm(c, annEmail)
	if r.status != 502 || !strings.Contains(r.body, "Deletion started") || !strings.Contains(r.body, "retries by itself") {
		t.Fatalf("delete: %d %s", r.status, r.body)
	}
	if st := scalar(t, d.pool, `SELECT status FROM tenants WHERE id = $1`, tenant); st != "deleting" {
		t.Errorf("status = %q, want deleting", st)
	}
	if n := rows(t, d.pool, `SELECT count(*) FROM accounts WHERE tenant_id = $1`, tenant); n != 1 {
		t.Error("the account was removed although the bot could not be stopped")
	}
	if exists, _ := d.fake.Has(job); !exists {
		t.Error("the job vanished")
	}
	if n := rows(t, d.pool, `SELECT count(*) FROM mail_outbox WHERE subject = 'Your SanHolo account is deleted'`); n != 0 {
		t.Error("a confirmation was queued before the account was deleted")
	}
	// Nomad is back, but it is too soon: the pass leaves the deletion to the
	// request that started it.
	mu.Lock()
	broken = false
	mu.Unlock()
	d.clock.advance(time.Minute)
	d.conn.RetryCleanups(context.Background())
	if n := rows(t, d.pool, `SELECT count(*) FROM accounts WHERE tenant_id = $1`, tenant); n != 1 {
		t.Error("the pass raced the request that started the deletion")
	}
	d.passes()
	if n := rows(t, d.pool, `SELECT count(*) FROM accounts WHERE tenant_id = $1`, tenant); n != 0 {
		t.Error("the pass did not finish the deletion")
	}
	if exists, _ := d.fake.Has(job); exists {
		t.Error("the job survived")
	}
	if n := rows(t, d.pool, `SELECT count(*) FROM mail_outbox WHERE subject = 'Your SanHolo account is deleted'`); n != 1 {
		t.Errorf("%d confirmations, want 1", n)
	}
	if n := rows(t, d.pool, `SELECT count(*) FROM audit_log WHERE action = 'account.delete' AND target = $1`, "tenant:"+tenant); n != 1 {
		t.Errorf("%d audit rows after a retry, want 1", n)
	}
}

// Without provisioning there is no advisory lock; the tenants row lock and
// the recheck under it keep two deletions of one account from both going on.
func TestTwoDeletionsWithoutProvisioningDeleteOnce(t *testing.T) {
	g := newGameEnv(t)
	c, tenant := g.user(t, annEmail)
	c2 := &client{t: t, app: c.app, cookies: map[string]*http.Cookie{}, host: c.host}
	for k, v := range c.cookies {
		c2.cookies[k] = v
	}
	var wg sync.WaitGroup
	statuses := make([]int, 2)
	for i, cl := range []*client{c, c2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i] = deleteForm(cl, annEmail).status
		}()
	}
	wg.Wait()
	ok := 0
	for _, s := range statuses {
		if s == 200 {
			ok++
		}
	}
	if ok != 1 {
		t.Errorf("statuses %v, want exactly one 200", statuses)
	}
	if n := rows(t, g.pool, `SELECT count(*) FROM mail_outbox WHERE subject = 'Your SanHolo account is deleted'`); n != 1 {
		t.Errorf("%d confirmation mails, want 1", n)
	}
	if n := rows(t, g.pool, `SELECT count(*) FROM audit_log WHERE action = 'account.delete' AND target = $1`, "tenant:"+tenant); n != 1 {
		t.Errorf("%d audit rows, want 1", n)
	}
}

func TestADeletionWhoseLogCannotBeWrittenRemovesNothing(t *testing.T) {
	d := newDelEnv(t)
	d.conn.SetDeletionLog(deletionlog.New(filepath.Join(t.TempDir(), "missing-dir", "deletions.jsonl")))
	c, tenant, job := d.connected(t)
	r := deleteForm(c, annEmail)
	if r.status != 502 {
		t.Errorf("delete = %d %s, want 502", r.status, r.body)
	}
	if n := rows(t, d.pool, `SELECT count(*) FROM accounts WHERE tenant_id = $1`, tenant); n != 1 {
		t.Error("the account was removed although the deletion could not be logged")
	}
	if exists, stopped := d.fake.Has(job); !exists || stopped {
		t.Error("the bot was stopped although the deletion could not be logged")
	}
	if st := scalar(t, d.pool, `SELECT status FROM tenants WHERE id = $1`, tenant); st != "deleting" {
		t.Errorf("status = %q, want deleting (the background pass retries)", st)
	}
}

// The confirmation holds the address, so the outbox must not keep it: it
// expires after a day if it cannot be sent, and the outbox cleanup deletes
// every sent or failed row 7 days after it was created.
func TestTheConfirmationDoesNotOutliveTheOutboxRetention(t *testing.T) {
	for _, sends := range []bool{true, false} {
		name := map[bool]string{true: "delivered", false: "never delivered"}[sends]
		t.Run(name, func(t *testing.T) {
			d := newDelEnv(t)
			c, _, _ := d.connected(t)
			if r := deleteForm(c, annEmail); r.status != 200 {
				t.Fatalf("delete: %d %s", r.status, r.body)
			}
			if !sends {
				d.mailer.FailNext(1000)
			}
			d.sendMail(t)
			if !sends {
				// Still queued, then expired after a day.
				d.clock.advance(25 * time.Hour)
				d.sendMail(t)
				if n := rows(t, d.pool, `SELECT count(*) FROM mail_outbox WHERE failed_at IS NOT NULL AND body = ''`); n != 1 {
					t.Fatalf("the undelivered confirmation was not failed and erased (%d)", n)
				}
			}
			d.clock.advance(8 * 24 * time.Hour)
			d.svc.SetClock(d.clock.now)
			if err := d.svc.Cleanup(context.Background()); err != nil {
				t.Fatal(err)
			}
			if tb := where(dump(t, d.pool), annEmail); len(tb) != 0 {
				t.Errorf("the address is still in %v 8 days after the deletion", tb)
			}
		})
	}
}

func TestAStartOnADeletingTenantIsRefused(t *testing.T) {
	d := newDelEnv(t)
	_, tenant, _ := d.connected(t)
	exec(t, d.pool, `UPDATE tenants SET status = 'deleting' WHERE id = $1`, tenant)
	if err := d.prov.Start(context.Background(), tenant); err != provision.ErrNotAllowed {
		t.Errorf("Start on a deleting tenant: %v, want ErrNotAllowed", err)
	}
}

func TestPrivacyNoticeDescribesExportAndDeletion(t *testing.T) {
	e := newEnv(t, auth.Options{})
	body := html.UnescapeString(e.client(t).get("/privacy", nil).body)
	for _, want := range []string{
		"The download link works once, only while you are signed in to the account it belongs to, and expires after 24 hours.",
		"Export links: a hash of the link is kept until 1 day after it expires.",
		"Deleting your account needs you to type its email address.",
		"The game account at Replicant Space is not deleted.",
		"records it and retries until it succeeds",
		"Until then the token or the volume is still there",
		"A deleted account is not restored from a backup",
		"deletion log",
		"7 days after it was created",
		"Data you delete remains in backups taken before the deletion until they expire, at most 30 days",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("privacy notice lacks %q", want)
		}
	}
}

// A qm that answers 404 (an unknown mount or route: a wrong prefix, a qm
// without the route) has not shown the secret gone. The purge stays pending
// and the check never passes on it.
func TestAQMAnswering404NeverPassesTheDeletion(t *testing.T) {
	d, tenant, _ := pendingTombstone(t)
	d.qm.SetDeleteAllowed(true)
	d.qm.SetOverride(func(r qmfake.Request) (int, string, bool) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL, "/qm/v1/secret/metadata/") {
			return http.StatusNotFound, "", true
		}
		return 0, "", false
	})
	d.passes()
	if n := rows(t, d.pool, `SELECT count(*) FROM pending_cleanups WHERE tenant_key = $1 AND kind = 'secret'`, tenant); n != 1 {
		t.Error("a purge whose check read 404 was taken as done")
	}
	if state, detail, completed, _ := deletion(t, d.pool, tenant); state != "pending" || completed || !strings.Contains(detail, "qm purge pending") {
		t.Errorf("check = %s %q completed=%v, want pending", state, detail, completed)
	}
	// Without a recorded purge the check asks qm itself: 404 is unreadable.
	exec(t, d.pool, `DELETE FROM pending_cleanups WHERE tenant_key = $1 AND kind = 'secret'`, tenant)
	d.passes()
	if state, detail, completed, _ := deletion(t, d.pool, tenant); state != "pending" || completed || !strings.Contains(detail, "qm could not be read") {
		t.Errorf("check = %s %q completed=%v, want pending, qm unreadable", state, detail, completed)
	}
	if n := rows(t, d.pool, `SELECT count(*) FROM tenants WHERE id = $1`, tenant); n != 1 {
		t.Error("the tombstone was removed on a 404")
	}
}

// The push and alert-intake rate-limit rows are keyed by the tenant id; a
// deletion removes exactly the deleted tenant's, never a neighbour's whose
// id merely starts with it.
func TestDeletionRemovesTheTenantsRateLimitRowsOnly(t *testing.T) {
	d := newDelEnv(t)
	c, tenant, _ := d.connected(t)
	neighbour := tenant + "b"
	keys := func(id string) []string { return []string{"push:tenant:" + id, "alerts:tenant:" + id} }
	for _, id := range []string{tenant, neighbour} {
		for _, k := range keys(id) {
			exec(t, d.pool, `INSERT INTO rate_counters (key, window_start, count) VALUES ($1, now(), 1)`, k)
		}
	}
	if r := deleteForm(c, annEmail); r.status != 200 {
		t.Fatalf("delete: %d %s", r.status, r.body)
	}
	for _, k := range keys(tenant) {
		if n := rows(t, d.pool, `SELECT count(*) FROM rate_counters WHERE key = $1`, k); n != 0 {
			t.Errorf("%s survived the deletion", k)
		}
	}
	for _, k := range keys(neighbour) {
		if n := rows(t, d.pool, `SELECT count(*) FROM rate_counters WHERE key = $1`, k); n != 1 {
			t.Errorf("the neighbour's %s was deleted", k)
		}
	}
}

// The check finds a tenant-keyed rate-limit row that appears after the
// deletion, and ignores a neighbour's.
func TestTheCheckFindsTenantRateLimitRows(t *testing.T) {
	for _, k := range []string{"push:tenant:", "alerts:tenant:"} {
		t.Run(k, func(t *testing.T) {
			d, tenant, _ := pendingTombstone(t)
			exec(t, d.pool, `INSERT INTO rate_counters (key, window_start, count) VALUES ($1, now(), 1)`, k+tenant+"b")
			d.passes()
			if state, detail, _, _ := deletion(t, d.pool, tenant); strings.Contains(detail, "rate_counters") {
				t.Fatalf("a neighbour's row failed the check: %s %q", state, detail)
			}
			exec(t, d.pool, `INSERT INTO rate_counters (key, window_start, count) VALUES ($1, now(), 1)`, k+tenant)
			d.passes()
			state, detail, completed, _ := deletion(t, d.pool, tenant)
			if state != "failed" || !strings.Contains(detail, "rows remain in rate_counters") || completed {
				t.Errorf("check = %s %q completed=%v, want failed on rate_counters", state, detail, completed)
			}
		})
	}
}
