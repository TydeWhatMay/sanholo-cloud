package server

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/lhaig/sanholo-cloud/internal/provision"
	"github.com/lhaig/sanholo-cloud/internal/updater"
)

const relDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func releasesEnv(t *testing.T) (*env, *client) {
	t.Helper()
	e := passkeyEnv(t)
	e.admin.SetUpdater(updater.New(e.pool, nil, updater.Options{BatchSize: 5}))
	op, _ := operator(t, e, "op@example.org")
	return e, op
}

func TestAdminReleasesPage(t *testing.T) {
	e, op := releasesEnv(t)
	ctx := context.Background()

	empty := op.get("/admin/releases", nil)
	if empty.status != 200 || !strings.Contains(empty.body, "There are no releases.") || !strings.Contains(empty.body, "none yet") {
		t.Fatalf("empty page: %d %s", empty.status, empty.body)
	}
	if strings.Contains(empty.body, "/resume") {
		t.Error("resume is offered with nothing halted")
	}
	if !strings.Contains(op.get("/admin", nil).body, `href="/admin/releases"`) {
		t.Error("the admin navigation does not link to the releases")
	}

	add := op.post("/admin/releases/add", url.Values{
		"version": {"v0.8.0"}, "digest": {relDigest}, "url": {"https://example.org/r/0.8.0"}, "notes": {"<script>alert(1)</script> fixes"},
	})
	if add.status != 303 || !strings.HasPrefix(add.header.Get("Location"), "/admin/releases?done=release") {
		t.Fatalf("add: %d %q %s", add.status, add.header.Get("Location"), add.body)
	}
	page := op.get(add.header.Get("Location"), nil)
	for _, want := range []string{"The release is added.", "v0.8.0", relDigest, "https://example.org/r/0.8.0", "&lt;script&gt;", "available", "Start rollout"} {
		if !strings.Contains(page.body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(page.body, "<script>alert") {
		t.Error("release notes were not escaped")
	}

	for name, form := range map[string]url.Values{
		"duplicate":   {"version": {"0.8.0"}},
		"bad version": {"version": {"soon"}},
		"bad digest":  {"version": {"0.9.0"}, "digest": {"sha256:00"}},
		"http link":   {"version": {"0.9.0"}, "url": {"http://example.org"}},
	} {
		want := 400
		if name == "duplicate" {
			want = 409
		}
		if r := op.post("/admin/releases/add", form); r.status != want {
			t.Errorf("%s: %d, want %d", name, r.status, want)
		}
	}
	if rows(t, e.pool, `SELECT count(*) FROM releases`) != 1 {
		t.Error("a refused release was stored")
	}

	// Start, and a second rollout is refused.
	if r := op.post("/admin/releases/add", url.Values{"version": {"0.9.0"}}); r.status != 303 {
		t.Fatalf("add second: %d", r.status)
	}
	start := op.post("/admin/releases/0.8.0/start", nil)
	if start.status != 303 || !strings.HasPrefix(start.header.Get("Location"), "/admin/releases?done=rollout") {
		t.Fatalf("start: %d %q %s", start.status, start.header.Get("Location"), start.body)
	}
	if r := op.post("/admin/releases/0.9.0/start", nil); r.status != 409 {
		t.Errorf("second rollout: %d", r.status)
	}
	if r := op.post("/admin/releases/7.7.7/start", nil); r.status != 404 {
		t.Errorf("unknown release: %d", r.status)
	}
	page = op.get("/admin/releases", nil)
	if !strings.Contains(page.body, "rolling") || strings.Contains(page.body, `/admin/releases/0.8.0/start"`) {
		t.Errorf("a rolling release offers a second start or is not shown rolling:\n%s", page.body)
	}

	// A halted rollout offers resume and abandon, with the fixed reason.
	if _, err := e.pool.Exec(ctx, `INSERT INTO tenants (id, status) VALUES ('acme', 'active')`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO rollout_tenants (release_version, tenant_id, batch, state, reason, previous_version) VALUES ('0.8.0', 'acme', 0, 'reverted', 'the deployment failed', '0.7.1')`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE releases SET state = 'halted', halted_tenant = 'acme', halt_reason = 'Nomad reported that the deployment failed' WHERE version = '0.8.0'`); err != nil {
		t.Fatal(err)
	}
	page = op.get("/admin/releases", nil)
	for _, want := range []string{"halted: acme, Nomad reported that the deployment failed", `action="/admin/releases/resume"`, `action="/admin/releases/abandon"`, "canary", "reverted", "v0.7.1"} {
		if !strings.Contains(page.body, want) {
			t.Errorf("halted page lacks %q", want)
		}
	}
	if r := op.post("/admin/releases/resume", nil); r.status != 303 || !strings.Contains(r.header.Get("Location"), "done=resume") {
		t.Fatalf("resume: %d %s", r.status, r.body)
	}
	if r := op.post("/admin/releases/resume", nil); r.status != 409 {
		t.Errorf("resume when nothing is halted: %d", r.status)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE releases SET state = 'halted', halted_tenant = 'acme', halt_reason = 'x' WHERE version = '0.8.0'`); err != nil {
		t.Fatal(err)
	}
	if r := op.post("/admin/releases/abandon", nil); r.status != 303 || !strings.Contains(r.header.Get("Location"), "done=abandon") {
		t.Fatalf("abandon: %d %s", r.status, r.body)
	}

	// Canary form.
	if r := op.post("/admin/releases/canary", url.Values{"tenant": {"acme"}}); r.status != 303 {
		t.Fatalf("canary: %d %s", r.status, r.body)
	}
	if rows(t, e.pool, `SELECT count(*) FROM tenants WHERE is_canary AND id = 'acme'`) != 1 {
		t.Error("canary not set")
	}
	if r := op.post("/admin/releases/canary", url.Values{"tenant": {"nope"}}); r.status != 404 {
		t.Errorf("unknown canary: %d", r.status)
	}
	if r := op.post("/admin/releases/canary", url.Values{"tenant": {""}}); r.status != 303 || rows(t, e.pool, `SELECT count(*) FROM tenants WHERE is_canary`) != 0 {
		t.Errorf("clear canary: %d", r.status)
	}

	// Every action names the operator in the audit log.
	var opID string
	_ = e.pool.QueryRow(ctx, `SELECT id::text FROM accounts WHERE email = 'op@example.org'`).Scan(&opID)
	for _, action := range []string{"release.add", "rollout.start", "rollout.resume", "rollout.abandon", "tenant.canary", "tenant.canary_clear"} {
		if rows(t, e.pool, `SELECT count(*) FROM audit_log WHERE action = $1 AND actor = $2`, action, opID) < 1 {
			t.Errorf("no %s audit row by the operator", action)
		}
	}
	if audit := op.get("/admin/audit", nil); !strings.Contains(audit.body, "release.add") {
		t.Error("the audit page does not show release actions")
	}
}

func TestAdminReleaseActionsRefusedWithoutOperatorPasskey(t *testing.T) {
	e := passkeyEnv(t)
	e.admin.SetUpdater(updater.New(e.pool, nil, updater.Options{}))
	_, opLink := operator(t, e, "op@example.org")
	e.account(t, "user@example.org")
	user := e.signedIn(t, "user@example.org")
	if err := e.admin.AddRelease(context.Background(), "cli", "0.8.0", "", "", ""); err != nil {
		t.Fatal(err)
	}
	posts := []struct {
		path string
		form url.Values
	}{
		{"/admin/releases/add", url.Values{"version": {"0.9.0"}}},
		{"/admin/releases/0.8.0/start", nil},
		{"/admin/releases/resume", nil},
		{"/admin/releases/abandon", nil},
		{"/admin/releases/canary", url.Values{"tenant": {"x"}}},
	}
	for _, p := range posts {
		for name, c := range map[string]*client{"anonymous": e.client(t), "non-operator": user, "operator by link": opLink} {
			want := map[string]int{"anonymous": 303, "non-operator": 404, "operator by link": 403}[name]
			if r := c.post(p.path, p.form); r.status != want {
				t.Errorf("%s %s: %d, want %d", name, p.path, r.status, want)
			}
		}
	}
	if rows(t, e.pool, `SELECT count(*) FROM releases`) != 1 || rows(t, e.pool, `SELECT count(*) FROM releases WHERE state <> 'available'`) != 0 {
		t.Error("a refused request changed the releases")
	}
	if rows(t, e.pool, `SELECT count(*) FROM audit_log WHERE action LIKE 'release.%' OR action LIKE 'rollout.%'`) != 1 {
		t.Error("a refused request left an audit row")
	}
}

func TestAdminReleaseActionsNeedCSRF(t *testing.T) {
	e, op := releasesEnv(t)
	if r := op.do("POST", "/admin/releases/add", url.Values{"version": {"0.8.0"}}, nil); r.status != 403 {
		t.Fatalf("no csrf token: %d", r.status)
	}
	if r := op.do("POST", "/admin/releases/add", url.Values{"version": {"0.8.0"}, "csrf": {"wrong"}}, nil); r.status != 403 {
		t.Fatalf("wrong csrf token: %d", r.status)
	}
	if rows(t, e.pool, `SELECT count(*) FROM releases`) != 0 {
		t.Fatal("release added without a CSRF token")
	}
}

func TestAccountPageShowsTheInstanceVersion(t *testing.T) {
	p := newProvEnv(t)
	p.admin.SetUpdater(updater.New(p.pool, p.prov, updater.Options{}))
	c, tenant := p.user(t, "ann@example.org")
	connectGame(t, c, tokenA)
	job := "sanholo-tenant-" + tenant
	eventually(t, "the job", func() bool { e, _ := p.fake.Has(job); return e })
	if !strings.Contains(c.get("/account", nil).body, "Version 0.7.1.") {
		t.Error("the account page does not show the bot's version")
	}
	p.fake.SetPhase(job, "healthy")
	eventually(t, "running", func() bool { return p.instance(t, tenant).State == provision.StateRunning })
	p.prov.Wait()
	if err := p.prov.Update(context.Background(), tenant, "0.8.0", ""); err != nil {
		t.Fatal(err)
	}
	if body := c.get("/account", nil).body; !strings.Contains(body, "Version 0.8.0.") {
		t.Errorf("a starting bot shows the wrong version:\n%s", body)
	}
	p.fake.SetPhase(job, "healthy")
	if inst, err := p.prov.Refresh(context.Background(), tenant); err != nil || inst.State != provision.StateRunning {
		t.Fatalf("refresh: %+v, %v", inst, err)
	}
	body := c.get("/account", nil).body
	if !strings.Contains(body, "Running") || !strings.Contains(body, "Version 0.8.0.") || strings.Contains(body, "Version 0.7.1.") {
		t.Errorf("the account page shows the wrong version:\n%s", body)
	}
}

// A canary that stays unhealthy must not wedge a release silently: the page
// says why and offers an audited override.
func TestAdminReleasesPageShowsTheCanaryGateAndOverrides(t *testing.T) {
	e, op := releasesEnv(t)
	ctx := context.Background()
	if r := op.post("/admin/releases/add", url.Values{"version": {"0.8.0"}}); r.status != 303 {
		t.Fatalf("add: %d", r.status)
	}
	if strings.Contains(op.get("/admin/releases", nil).body, "override-canary") {
		t.Error("the override is offered although nothing holds the release")
	}
	if r := op.post("/admin/releases/0.8.0/override-canary", nil); r.status != 409 {
		t.Errorf("override with nothing holding the release = %d, want 409", r.status)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO tenants (id, status, is_canary) VALUES ('acme', 'active', true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO tenant_instances (tenant_id, job_id, image_version, state, last_unhealthy_at) VALUES ('acme', 'sanholo-tenant-acme', '0.7.1', 'running', now() - interval '10 minutes')`); err != nil {
		t.Fatal(err)
	}
	page := op.get("/admin/releases", nil)
	for _, want := range []string{"Held by the canary gate", "acme", "was unhealthy in the last hour", `action="/admin/releases/0.8.0/override-canary"`} {
		if !strings.Contains(page.body, want) {
			t.Errorf("page lacks %q:\n%s", want, page.body)
		}
	}
	if r := op.post("/admin/releases/0.8.0/start", nil); r.status != 409 || !strings.Contains(r.body, "override the canary gate") {
		t.Errorf("a refused start: %d %s", r.status, r.body)
	}
	if r := op.post("/admin/releases/0.8.0/override-canary", nil); r.status != 303 || !strings.HasPrefix(r.header.Get("Location"), "/admin/releases?done=override") {
		t.Fatalf("override: %d %q", r.status, r.header.Get("Location"))
	}
	if rows(t, e.pool, `SELECT count(*) FROM audit_log WHERE action = 'rollout.canary_override' AND target = 'release:0.8.0'`) != 1 {
		t.Error("the override is not audited")
	}
	if strings.Contains(op.get("/admin/releases", nil).body, "Held by the canary gate") {
		t.Error("an overridden gate is still shown as holding")
	}
	if r := op.post("/admin/releases/0.8.0/start", nil); r.status != 303 {
		t.Errorf("start after the override: %d", r.status)
	}
	if r := op.post("/admin/releases/7.7.7/override-canary", nil); r.status != 404 {
		t.Errorf("override of an unknown release: %d", r.status)
	}
}
