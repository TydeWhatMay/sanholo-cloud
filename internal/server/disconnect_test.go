package server

import (
	"context"
	"html"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/lhaig/sanholo-cloud/internal/nomad/nomadfake"
	"github.com/lhaig/sanholo-cloud/internal/provision"
)

// connected: a customer with a connected game account and a running bot.
func (p *provEnv) connected(t *testing.T) (c *client, tenant, job string) {
	t.Helper()
	c, tenant = p.user(t, "ann@example.org")
	job = "sanholo-tenant-" + tenant
	connectGame(t, c, tokenA)
	eventually(t, "the job", func() bool { e, _ := p.fake.Has(job); return e })
	p.fake.SetPhase(job, nomadfake.Healthy)
	eventually(t, "running", func() bool { return p.instance(t, tenant).State == provision.StateRunning })
	p.prov.Wait()
	return c, tenant, job
}

func TestDisconnectNeedsAConfirmationStepAndChangesNothingOnGET(t *testing.T) {
	p := newProvEnv(t)
	c, tenant, job := p.connected(t)
	page := c.get("/account", nil)
	if !strings.Contains(page.body, `href="/account/game/disconnect"`) {
		t.Errorf("the account page has no way to disconnect:\n%s", page.body)
	}
	r := c.get("/account/game/disconnect", nil)
	for _, want := range []string{"Disconnect your game account", "Alpha", `action="/account/game/disconnect"`, `name="csrf"`, "permanent"} {
		if r.status != 200 || !strings.Contains(r.body, want) {
			t.Errorf("confirmation page (%d) lacks %q", r.status, want)
		}
	}
	// HEAD and prefetch change nothing either, and nothing was disconnected.
	if r := c.do("HEAD", "/account/game/disconnect", nil, nil); r.status == 200 && r.body != "" {
		t.Error("HEAD rendered the page")
	}
	if rows(t, p.pool, `SELECT count(*) FROM game_accounts WHERE tenant_id = $1`, tenant) != 1 {
		t.Error("a GET disconnected the game account")
	}
	if exists, stopped := p.fake.Has(job); !exists || stopped {
		t.Error("a GET stopped the bot")
	}
	if p.fake.Count("DELETE", "/v1/job/") != 0 {
		t.Error("a GET sent a delete to Nomad")
	}
}

func TestDisconnectRequiresASessionAndTheCSRFToken(t *testing.T) {
	p := newProvEnv(t)
	c, tenant, _ := p.connected(t)
	if r := newGameEnv(t).client(t).post("/account/game/disconnect", nil); r.status != 303 || r.header.Get("Location") != "/signin" {
		t.Errorf("anonymous disconnect = %d %q, want a redirect to sign in", r.status, r.header.Get("Location"))
	}
	c.get("/", nil)
	if r := c.do("POST", "/account/game/disconnect", url.Values{}, nil); r.status != 403 {
		t.Errorf("disconnect without a CSRF token = %d, want 403", r.status)
	}
	if rows(t, p.pool, `SELECT count(*) FROM game_accounts WHERE tenant_id = $1`, tenant) != 1 {
		t.Error("a request without a CSRF token disconnected the game account")
	}
}

func TestDisconnectStopsPurgesAndDeletesInOrderAndKeepsTheAccount(t *testing.T) {
	p := newProvEnv(t)
	c, tenant, job := p.connected(t)
	r := c.post("/account/game/disconnect", nil)
	if r.status != 200 || !strings.Contains(r.body, "Game account disconnected") || !strings.Contains(html.UnescapeString(r.body), "Your token and your bot's data volume are deleted.") {
		t.Fatalf("disconnect: %d %s", r.status, r.body)
	}
	// Stop without purge, then purge, then the volume.
	var order []string
	for _, rq := range p.fake.Requests() {
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
	if exists, _ := p.fake.Has(job); exists {
		t.Error("the job is still registered")
	}
	if len(p.fake.Volumes()) != 0 {
		t.Errorf("volumes left: %v", p.fake.Volumes())
	}
	if _, ok := p.qm.Stored(qmPath(tenant)); ok {
		t.Error("the token is still in qm")
	}
	if rows(t, p.pool, `SELECT count(*) FROM game_accounts WHERE tenant_id = $1`, tenant) != 0 ||
		rows(t, p.pool, `SELECT count(*) FROM tenant_instances WHERE tenant_id = $1`, tenant) != 0 {
		t.Error("the game account or instance record is still there")
	}
	if rows(t, p.pool, `SELECT count(*) FROM tenants WHERE id = $1`, tenant) != 1 ||
		rows(t, p.pool, `SELECT count(*) FROM accounts WHERE tenant_id = $1`, tenant) != 1 {
		t.Error("the tenant or the account was removed")
	}
	if rows(t, p.pool, `SELECT count(*) FROM pending_cleanups`) != 0 {
		t.Error("a finished disconnect left pending deletions")
	}
	if rows(t, p.pool, `SELECT count(*) FROM audit_log WHERE action = 'game.disconnect' AND target = $1`, "tenant:"+tenant) != 1 {
		t.Error("no audit row")
	}
	// The account page offers to connect again.
	if page := c.get("/account", nil); !strings.Contains(page.body, `action="/account/game/validate"`) || strings.Contains(page.body, "Disconnect this game account") {
		t.Errorf("account page after the disconnect:\n%s", page.body)
	}
	if strings.Contains(p.logs.String(), tokenA) {
		t.Error("the token is in the log")
	}
}

// Until qm ships its purge endpoint every purge is refused. The disconnect
// still completes, says so plainly, and keeps the deletion to be retried.
func TestDisconnectWhenQmCannotDeleteYetKeepsThePurgePending(t *testing.T) {
	p := newProvEnv(t)
	c, tenant, _ := p.connected(t)
	p.qm.SetDeleteAllowed(false)
	r := c.post("/account/game/disconnect", nil)
	if r.status != 200 || !strings.Contains(r.body, "Deleting your token in the secrets store is not finished yet") ||
		!strings.Contains(r.body, "retries until it succeeds") || strings.Contains(html.UnescapeString(r.body), "Your token and your bot's data volume are deleted.") {
		t.Fatalf("disconnect: %d %s", r.status, r.body)
	}
	if rows(t, p.pool, `SELECT count(*) FROM game_accounts WHERE tenant_id = $1`, tenant) != 0 {
		t.Error("the game account row was kept")
	}
	var attempts int
	var lastErr string
	if err := p.pool.QueryRow(context.Background(),
		`SELECT attempts, last_error FROM pending_cleanups WHERE tenant_key = $1 AND kind = 'secret'`, tenant).Scan(&attempts, &lastErr); err != nil {
		t.Fatalf("no pending purge recorded: %v", err)
	}
	if attempts != 1 || lastErr == "" {
		t.Errorf("attempts %d last_error %q", attempts, lastErr)
	}
	if _, ok := p.qm.Stored(qmPath(tenant)); !ok {
		t.Fatal("the token vanished although qm refused the deletion")
	}
	if strings.Contains(p.logs.String(), tokenA) {
		t.Error("the token is in the log")
	}
	// The worker finishes it once qm allows it.
	if n := p.conn.RetryCleanups(context.Background()); n != 0 {
		t.Errorf("a retry against a refusing qm finished %d", n)
	}
	p.qm.SetDeleteAllowed(true)
	p.clock.advance(2 * time.Minute) // past the first backoff
	if n := p.conn.RetryCleanups(context.Background()); n != 1 {
		t.Errorf("retry finished %d, want 1", n)
	}
	if _, ok := p.qm.Stored(qmPath(tenant)); ok {
		t.Error("the token is still in qm after the retry")
	}
	if rows(t, p.pool, `SELECT count(*) FROM pending_cleanups`) != 0 {
		t.Error("the finished purge is still pending")
	}
}

func TestDisconnectKeepsAVolumeDeletionNomadRefusedPending(t *testing.T) {
	p := newProvEnv(t)
	c, tenant, _ := p.connected(t)
	p.fake.Override(func(r nomadfake.Request) (int, string, bool) {
		return 503, "", r.Method == "DELETE" && strings.HasPrefix(r.Path, "/v1/volume/host/")
	})
	r := c.post("/account/game/disconnect", nil)
	if r.status != 200 || !strings.Contains(html.UnescapeString(r.body), "Deleting your bot's data volume is not finished yet") {
		t.Fatalf("disconnect: %d %s", r.status, r.body)
	}
	if rows(t, p.pool, `SELECT count(*) FROM pending_cleanups WHERE tenant_key = $1 AND kind = 'volume'`, tenant) != 1 {
		t.Error("the volume deletion was not recorded")
	}
	p.fake.Override(nil)
	p.clock.advance(2 * time.Minute) // past the first backoff
	if n := p.conn.RetryCleanups(context.Background()); n != 1 || len(p.fake.Volumes()) != 0 {
		t.Errorf("retry finished %d, volumes %v", n, p.fake.Volumes())
	}
}

// If the bot cannot be stopped nothing else happens: the customer can try
// again and the token is not pulled from under a running bot.
func TestDisconnectChangesNothingWhenTheBotCannotBeStopped(t *testing.T) {
	p := newProvEnv(t)
	c, tenant, job := p.connected(t)
	p.fake.Override(func(r nomadfake.Request) (int, string, bool) { return 503, "", r.Method == "DELETE" })
	r := c.post("/account/game/disconnect", nil)
	if r.status != 502 || !strings.Contains(r.body, "Nothing was changed") {
		t.Fatalf("disconnect: %d %s", r.status, r.body)
	}
	if rows(t, p.pool, `SELECT count(*) FROM game_accounts WHERE tenant_id = $1`, tenant) != 1 {
		t.Error("the game account was removed")
	}
	if _, ok := p.qm.Stored(qmPath(tenant)); !ok {
		t.Error("the token was deleted")
	}
	if exists, _ := p.fake.Has(job); !exists {
		t.Error("the job was removed")
	}
	if rows(t, p.pool, `SELECT count(*) FROM pending_cleanups`) != 0 {
		t.Error("a pending deletion was recorded for a disconnect that did not happen")
	}
}

// Connecting again while a purge is pending must not lose the new token to
// the old purge.
func TestReconnectCancelsAPendingPurge(t *testing.T) {
	p := newProvEnv(t)
	c, tenant, _ := p.connected(t)
	p.qm.SetDeleteAllowed(false)
	if r := c.post("/account/game/disconnect", nil); r.status != 200 {
		t.Fatalf("disconnect: %d", r.status)
	}
	if rows(t, p.pool, `SELECT count(*) FROM pending_cleanups WHERE tenant_key = $1`, tenant) == 0 {
		t.Fatal("setup: nothing pending")
	}
	connectGame(t, c, tokenA)
	eventually(t, "the new job", func() bool { e, _ := p.fake.Has("sanholo-tenant-" + tenant); return e })
	p.fake.SetPhase("sanholo-tenant-"+tenant, nomadfake.Healthy)
	if rows(t, p.pool, `SELECT count(*) FROM pending_cleanups WHERE tenant_key = $1`, tenant) != 0 {
		t.Error("the pending deletions survived a new connect")
	}
	p.qm.SetDeleteAllowed(true)
	p.conn.RetryCleanups(context.Background())
	if _, ok := p.qm.Stored(qmPath(tenant)); !ok {
		t.Error("the retry of the old purge removed the new token")
	}
	p.prov.Wait()
}

func TestDisconnectWithoutAGameAccount(t *testing.T) {
	p := newProvEnv(t)
	c, _ := p.user(t, "ann@example.org")
	if r := c.post("/account/game/disconnect", nil); r.status != 409 {
		t.Errorf("disconnect with nothing connected = %d, want 409", r.status)
	}
	if r := c.get("/account/game/disconnect", nil); r.status != 409 {
		t.Errorf("confirmation with nothing connected = %d, want 409", r.status)
	}
}

// The deletion of a tenant that connected again is moot. The retry must not
// purge a token that belongs to a live connection, whatever left the row.
func TestRetryDoesNotPurgeATenantThatIsConnected(t *testing.T) {
	p := newProvEnv(t)
	_, tenant, _ := p.connected(t)
	if _, err := p.pool.Exec(context.Background(),
		`INSERT INTO pending_cleanups (tenant_key, kind) VALUES ($1, 'secret'), ($1, 'volume')`, tenant); err != nil {
		t.Fatal(err)
	}
	p.conn.RetryCleanups(context.Background())
	if _, ok := p.qm.Stored(qmPath(tenant)); !ok {
		t.Error("a connected tenant's token was purged")
	}
	if len(p.fake.Volumes()) != 1 {
		t.Errorf("a retry deleted a connected tenant's volume: %v", p.fake.Volumes())
	}
	// The old token's purge is moot; the volume's deletion is left to the
	// start of the new connection, which makes it before creating a volume.
	if rows(t, p.pool, `SELECT count(*) FROM pending_cleanups WHERE kind = 'secret'`) != 0 {
		t.Error("the moot purge was kept")
	}
	if rows(t, p.pool, `SELECT count(*) FROM pending_cleanups WHERE kind = 'volume'`) != 1 {
		t.Error("a connected tenant's pending volume deletion was dropped without being carried out")
	}
}

// Nomad's deployment verdict never changes after it is decided; the account
// page and the admin list show "not answering" from the live check.
func TestAccountAndAdminShowALiveCheckFailure(t *testing.T) {
	p := newProvEnv(t)
	op, _ := operator(t, p.env, "op@example.org")
	c, tenant, job := p.connected(t)
	if page := c.get("/account", nil); !strings.Contains(page.body, "Bot: <strong>Running</strong>") {
		t.Fatalf("account page before the failure:\n%s", page.body)
	}
	p.fake.SetCheck(job, "failure")
	p.prov.Reconcile(context.Background())
	page := c.get("/account", nil)
	if !strings.Contains(page.body, "Running, not answering") {
		t.Errorf("account page after a failing check:\n%s", page.body)
	}
	if list := op.get("/admin", nil); !strings.Contains(list.body, "unhealthy") {
		t.Errorf("admin list after a failing check:\n%s", list.body)
	}
	p.fake.SetCheck(job, "success")
	p.prov.Reconcile(context.Background())
	if page := c.get("/account", nil); !strings.Contains(page.body, "Bot: <strong>Running</strong>") {
		t.Errorf("account page after the check recovered:\n%s", page.body)
	}
	_ = tenant
}
