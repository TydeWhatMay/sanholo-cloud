package server

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/lhaig/sanholo-cloud/internal/nomad"
	"github.com/lhaig/sanholo-cloud/internal/nomad/nomadfake"
	"github.com/lhaig/sanholo-cloud/internal/provision"
)

const nomadTok = "nomad-acl-SECRET-token-55aa"

type provEnv struct {
	*gameEnv
	fake *nomadfake.Fake
	prov *provision.Service
}

// newProvEnv is a game environment whose app also provisions through a fake
// Nomad. Clients must be made after it returns.
func newProvEnv(t *testing.T) *provEnv {
	t.Helper()
	g := newGameEnv(t)
	fake := nomadfake.New(t, nomadTok, "sanholo")
	nc, err := nomad.New(nomad.Config{Addr: fake.URL(), Token: nomadTok, Namespace: "sanholo", Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	prov, err := provision.New(g.pool, nc, provision.Options{
		Settings: nomadfake.Settings(), Volume: nomadfake.VolumeSettings(), Version: "0.7.1", Timeout: 10 * time.Minute, Poll: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(prov.Close)
	g.admin.SetProvisioner(prov)
	g.conn.SetProvisioner(prov)
	g.app = New(g.pool, WithApp(&App{Auth: g.svc, Admin: g.admin, Connect: g.conn, Provision: prov}))
	return &provEnv{g, fake, prov}
}

// connectGame validates and confirms tokenA-style tokens for c.
func connectGame(t *testing.T, c *client, token string) result {
	t.Helper()
	if r := validate(c, token); r.status != 200 {
		t.Fatalf("validate: %d %s", r.status, r.body)
	}
	r := c.post("/account/game/confirm", nil)
	if r.status != 200 {
		t.Fatalf("confirm: %d %s", r.status, r.body)
	}
	return r
}

func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (p *provEnv) instance(t *testing.T, tenant string) *provision.Instance {
	t.Helper()
	inst, err := p.prov.Get(context.Background(), tenant)
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

func TestConfirmStartsTheBotAndTheAccountPageShowsIt(t *testing.T) {
	p := newProvEnv(t)
	c, tenant := p.user(t, "ann@example.org")
	if r := c.get("/account", nil); strings.Contains(r.body, "Bot:") {
		t.Error("the account page shows a bot before a game account is connected")
	}
	done := connectGame(t, c, tokenA)
	if !strings.Contains(done.body, "Your bot is starting") {
		t.Errorf("the confirmation does not say the bot is starting: %s", done.body)
	}
	eventually(t, "the instance row", func() bool { return p.instance(t, tenant) != nil })

	// Pending in Nomad: the page says Starting and polls itself.
	page := c.get("/account", nil)
	for _, want := range []string{"Bot: <strong>Starting</strong>", `hx-trigger="every 5s"`, `hx-select="#instance"`} {
		if !strings.Contains(page.body, want) {
			t.Errorf("account page lacks %q:\n%s", want, page.body)
		}
	}
	if p.fake.Image("sanholo-tenant-"+tenant) != "ghcr.io/tydewhatmay/sanholo:v0.7.1" {
		t.Errorf("registered image = %q", p.fake.Image("sanholo-tenant-"+tenant))
	}

	// Healthy: the page says Running and stops polling.
	p.fake.SetPhase("sanholo-tenant-"+tenant, nomadfake.Healthy)
	eventually(t, "running", func() bool { return p.instance(t, tenant).State == provision.StateRunning })
	page = c.get("/account", nil)
	if !strings.Contains(page.body, "Bot: <strong>Running</strong>") || strings.Contains(page.body, "hx-trigger") {
		t.Errorf("running page:\n%s", page.body)
	}
	// The game token never reaches Nomad.
	for _, r := range p.fake.Requests() {
		if strings.Contains(r.Body, tokenA) || strings.Contains(r.Query, tokenA) {
			t.Errorf("the game token reached Nomad: %s %s", r.Method, r.Path)
		}
	}
}

func TestAccountPageWithoutNomad(t *testing.T) {
	g := newGameEnv(t) // no provisioning
	c, _ := g.user(t, "ann@example.org")
	connectGame(t, c, tokenA)
	page := c.get("/account", nil)
	if !strings.Contains(page.body, "Not provisioned") || strings.Contains(page.body, "hx-trigger") {
		t.Errorf("account page:\n%s", page.body)
	}
	if rows(t, g.pool, `SELECT count(*) FROM tenant_instances`) != 0 {
		t.Error("an instance row without Nomad")
	}
}

func TestReconnectRestartsTheBot(t *testing.T) {
	p := newProvEnv(t)
	c, tenant := p.user(t, "ann@example.org")
	connectGame(t, c, tokenA)
	job := "sanholo-tenant-" + tenant
	eventually(t, "the job", func() bool { e, _ := p.fake.Has(job); return e })
	p.fake.SetPhase(job, nomadfake.Healthy)
	eventually(t, "running", func() bool {
		i := p.instance(t, tenant)
		return i != nil && i.State == provision.StateRunning
	})
	done := connectGame(t, c, tokenA2) // same game account, new token
	if !strings.Contains(done.body, "restarting with the new token") {
		t.Errorf("confirmation: %s", done.body)
	}
	eventually(t, "the restart", func() bool { return p.fake.Restarts(job) == 1 })
	p.prov.Wait()
}

func TestFailedBotCanBeRetried(t *testing.T) {
	p := newProvEnv(t)
	p.fake.Override(func(r nomadfake.Request) (int, string, bool) { return 503, "", r.Method == "POST" })
	c, tenant := p.user(t, "ann@example.org")
	connectGame(t, c, tokenA)
	eventually(t, "failure", func() bool {
		i := p.instance(t, tenant)
		return i != nil && i.State == provision.StateFailed
	})
	page := c.get("/account", nil)
	for _, want := range []string{"Bot: <strong>Failed</strong>", provision.ReasonUnreachable, `action="/account/instance/retry"`} {
		if !strings.Contains(page.body, want) {
			t.Errorf("account page lacks %q", want)
		}
	}
	p.fake.Override(nil)
	if r := c.post("/account/instance/retry", nil); r.status != 303 || r.header.Get("Location") != "/account" {
		t.Fatalf("retry: %d %s", r.status, r.body)
	}
	eventually(t, "the job", func() bool { e, _ := p.fake.Has("sanholo-tenant-" + tenant); return e })
}

func TestRetryOnlyForAFailedBot(t *testing.T) {
	p := newProvEnv(t)
	c, tenant := p.user(t, "ann@example.org")
	if r := c.post("/account/instance/retry", nil); r.status != 409 {
		t.Errorf("retry with no instance = %d, want 409", r.status)
	}
	connectGame(t, c, tokenA)
	// The start registers the job in the background after writing the row;
	// count only once that registration has happened, or it lands between
	// the two reads and looks like the refused retry's.
	eventually(t, "the instance", func() bool { return p.instance(t, tenant) != nil })
	eventually(t, "the first registration", func() bool { return p.fake.Count("POST", "/v1/jobs") >= 1 })
	before := p.fake.Count("POST", "/v1/jobs")
	if r := c.post("/account/instance/retry", nil); r.status != 409 {
		t.Errorf("retry of a starting bot = %d, want 409", r.status)
	}
	if p.fake.Count("POST", "/v1/jobs") != before {
		t.Error("a refused retry registered the job again")
	}
	if r := newGameEnv(t).client(t).post("/account/instance/retry", nil); r.status != 303 {
		t.Errorf("anonymous retry = %d, want a redirect to sign in", r.status)
	}
}

func TestAdminShowsInstancesAndRestarts(t *testing.T) {
	p := newProvEnv(t)
	op, _ := operator(t, p.env, "op@example.org")
	c, tenant := p.user(t, "ann@example.org")
	job := "sanholo-tenant-" + tenant

	list := op.get("/admin", nil)
	if !strings.Contains(list.body, "not provisioned") || strings.Contains(list.body, `/restart"`) {
		t.Errorf("tenant list before provisioning:\n%s", list.body)
	}
	if r := op.post("/admin/tenants/"+tenant+"/restart", nil); r.status != 409 {
		t.Errorf("restart of a tenant with no instance = %d, want 409", r.status)
	}

	connectGame(t, c, tokenA)
	eventually(t, "the job", func() bool { e, _ := p.fake.Has(job); return e })
	p.fake.SetPhase(job, nomadfake.Healthy)
	eventually(t, "running", func() bool { return p.instance(t, tenant).State == provision.StateRunning })
	p.prov.Wait()

	list = op.get("/admin", nil)
	for _, want := range []string{"running (v0.7.1)", "healthy, ", `action="/admin/tenants/` + tenant + `/restart"`} {
		if !strings.Contains(list.body, want) {
			t.Errorf("tenant list lacks %q", want)
		}
	}
	// Only the operator's own tenant, which has no game account, is left.
	if n := strings.Count(list.body, "not provisioned"); n != 1 {
		t.Errorf("the placeholder is shown %d times, want once", n)
	}

	r := op.post("/admin/tenants/"+tenant+"/restart", nil)
	if r.status != 303 || !strings.HasPrefix(r.header.Get("Location"), "/admin?done=restart") {
		t.Fatalf("restart: %d %q %s", r.status, r.header.Get("Location"), r.body)
	}
	if !strings.Contains(op.get(r.header.Get("Location"), nil).body, "The bot is restarting.") {
		t.Error("no confirmation notice")
	}
	if p.fake.Restarts(job) != 1 {
		t.Errorf("allocation restarted %d times, want 1", p.fake.Restarts(job))
	}
	if n := rows(t, p.pool, `SELECT count(*) FROM audit_log WHERE action = 'instance.restart' AND target = 'tenant:`+tenant+`'`); n != 1 {
		t.Errorf("%d audit rows for the restart, want 1", n)
	}
	p.prov.Wait()

	// Non-operators cannot restart, and the audit log shows nothing more.
	if r := c.post("/admin/tenants/"+tenant+"/restart", nil); r.status != 404 {
		t.Errorf("customer restart = %d, want 404", r.status)
	}
	if p.fake.Restarts(job) != 1 {
		t.Error("a customer restarted a bot through the admin route")
	}
}

func TestAdminRestartWhenNomadFails(t *testing.T) {
	p := newProvEnv(t)
	op, _ := operator(t, p.env, "op@example.org")
	c, tenant := p.user(t, "ann@example.org")
	job := "sanholo-tenant-" + tenant
	connectGame(t, c, tokenA)
	eventually(t, "the job", func() bool { e, _ := p.fake.Has(job); return e })
	p.fake.SetPhase(job, nomadfake.Healthy)
	eventually(t, "running", func() bool { return p.instance(t, tenant).State == provision.StateRunning })
	p.prov.Wait()
	p.fake.Override(func(r nomadfake.Request) (int, string, bool) { return 503, "", strings.Contains(r.Path, "/restart") })
	if r := op.post("/admin/tenants/"+tenant+"/restart", url.Values{}); r.status != 502 {
		t.Fatalf("restart against a failing Nomad = %d, want 502: %s", r.status, r.body)
	}
	if got := p.instance(t, tenant).State; got != provision.StateFailed {
		t.Errorf("state = %q, want failed", got)
	}
}

// A deployment that failed leaves the same job in Nomad. Retrying must
// register it again; a plain start sees the same spec and does nothing.
func TestRetryOfAFailedDeploymentRegistersTheJobAgain(t *testing.T) {
	p := newProvEnv(t)
	c, tenant := p.user(t, "ann@example.org")
	job := "sanholo-tenant-" + tenant
	connectGame(t, c, tokenA)
	eventually(t, "the job", func() bool { e, _ := p.fake.Has(job); return e })
	p.fake.SetPhase(job, nomadfake.Failed) // the registration worked; the deployment failed
	p.prov.Wait()                          // the start's follow-up ends on the failure
	if got := p.instance(t, tenant); got.State != provision.StateFailed || got.FailureReason != provision.ReasonDeployFailed {
		t.Fatalf("instance = %+v, want a failed deployment", got)
	}
	before := p.fake.Count("POST", "/v1/jobs")
	if r := c.post("/account/instance/retry", nil); r.status != 303 {
		t.Fatalf("retry: %d %s", r.status, r.body)
	}
	eventually(t, "the second registration", func() bool { return p.fake.Count("POST", "/v1/jobs") > before })
	p.fake.SetPhase(job, nomadfake.Failed) // the new version fails again; the follow-up ends
	p.prov.Wait()
	if got := p.fake.Count("POST", "/v1/jobs"); got != before+1 {
		t.Errorf("registrations %d -> %d: the retry did not register the job again", before, got)
	}
	// On real Nomad an identical spec is only a new evaluation: the retry must
	// change the job meta so a new version replaces the failed allocation.
	if p.fake.Version(job) != 2 {
		t.Errorf("job version = %d, want 2 (a new version)", p.fake.Version(job))
	}
	if m, _ := p.fake.Job(job)["Meta"].(map[string]any); m["sanholo.restart"] != "1" {
		t.Errorf("job meta = %v, want sanholo.restart=1", m)
	}
}
