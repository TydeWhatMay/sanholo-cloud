package provision_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lhaig/sanholo-cloud/internal/dbtest"
	"github.com/lhaig/sanholo-cloud/internal/nomad"
	"github.com/lhaig/sanholo-cloud/internal/nomad/nomadfake"
	"github.com/lhaig/sanholo-cloud/internal/provision"
)

const nomadToken = "nomad-acl-SECRET-token-7f3a91"

type env struct {
	pool *pgxpool.Pool
	fake *nomadfake.Fake
	svc  *provision.Service
	logs *bytes.Buffer
	opt  provision.Options
	nc   *nomad.Client

	mu  sync.Mutex
	now time.Time
}

func (e *env) clock() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.now
}

func (e *env) advance(d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.now = e.now.Add(d)
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{pool: dbtest.Migrated(t), logs: &bytes.Buffer{}, now: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	e.fake = nomadfake.New(t, nomadToken, "sanholo")
	log := slog.New(slog.NewTextHandler(&lockedWriter{w: e.logs}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	var err error
	e.nc, err = nomad.New(nomad.Config{Addr: e.fake.URL(), Token: nomadToken, Namespace: "sanholo", Timeout: 2 * time.Second, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	e.opt = provision.Options{Settings: nomadfake.Settings(), Version: "0.7.1", Timeout: 10 * time.Minute, Poll: 5 * time.Millisecond, Logger: log}
	e.svc = e.service(t, e.opt)
	return e
}

type lockedWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func (e *env) service(t *testing.T, opt provision.Options) *provision.Service {
	t.Helper()
	svc, err := provision.New(e.pool, e.nc, opt)
	if err != nil {
		t.Fatal(err)
	}
	svc.SetClock(e.clock)
	t.Cleanup(svc.Close)
	return svc
}

// tenant creates a tenant, with a connected game account unless status says
// otherwise.
func (e *env) tenant(t *testing.T, id, status string, game bool) {
	t.Helper()
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `INSERT INTO tenants (id, status) VALUES ($1, $2)`, id, status); err != nil {
		t.Fatal(err)
	}
	if game {
		if _, err := e.pool.Exec(ctx,
			`INSERT INTO game_accounts (tenant_id, game_account_id, replicant_name, replicant_code) VALUES ($1, $2, 'Alpha', 'AAAA1111')`,
			id, "replicant:"+id); err != nil {
			t.Fatal(err)
		}
	}
}

func job(id string) string { return "sanholo-tenant-" + id }

func (e *env) instance(t *testing.T, id string) *provision.Instance {
	t.Helper()
	inst, err := e.svc.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if inst == nil {
		t.Fatalf("tenant %s has no instance", id)
	}
	return inst
}

func (e *env) posts() int { return e.fake.Count("POST", "/v1/jobs") }

func TestStartRegistersTheJob(t *testing.T) {
	e := newEnv(t)
	e.tenant(t, "acme", "active", true)
	if err := e.svc.Start(context.Background(), "acme"); err != nil {
		t.Fatal(err)
	}
	if got := e.fake.Image(job("acme")); got != "ghcr.io/tydewhatmay/sanholo:v0.7.1" {
		t.Errorf("registered image = %q", got)
	}
	inst := e.instance(t, "acme")
	if inst.State != provision.StatePending || inst.JobID != job("acme") || inst.ImageVersion != "0.7.1" {
		t.Errorf("instance = %+v", inst)
	}
}

func TestStartRefusals(t *testing.T) {
	e := newEnv(t)
	e.tenant(t, "nogame", "active", false)
	e.tenant(t, "susp", "active", true)
	e.tenant(t, "gone", "active", true)
	e.tenant(t, "pend", "pending", true)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `UPDATE tenants SET status = 'suspended', status_before_suspend = 'active' WHERE id = 'susp'`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE tenants SET status = 'deleting' WHERE id = 'gone'`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id   string
		want error
	}{
		{"nogame", provision.ErrNoGameAccount},
		{"susp", provision.ErrNotAllowed},
		{"gone", provision.ErrNotAllowed},
		{"missing", provision.ErrNotFound},
		{"Bad/Id", provision.ErrNotFound},
	} {
		if err := e.svc.Start(ctx, tc.id); !errors.Is(err, tc.want) {
			t.Errorf("Start(%q) = %v, want %v", tc.id, err, tc.want)
		}
	}
	if len(e.fake.Requests()) != 0 {
		t.Errorf("%d requests reached Nomad for refused tenants", len(e.fake.Requests()))
	}
	if n := countRows(t, e.pool); n != 0 {
		t.Errorf("%d instance rows for refused tenants", n)
	}
	// A pending tenant (signed up, not yet active) may be provisioned.
	if err := e.svc.Start(ctx, "pend"); err != nil {
		t.Errorf("pending tenant: %v", err)
	}
}

func countRows(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM tenant_instances`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestStartIsIdempotent(t *testing.T) {
	e := newEnv(t)
	e.tenant(t, "acme", "active", true)
	ctx := context.Background()
	for range 3 {
		if err := e.svc.Start(ctx, "acme"); err != nil {
			t.Fatal(err)
		}
	}
	if e.posts() != 1 {
		t.Errorf("%d registrations for an unchanged job, want 1", e.posts())
	}
	if e.fake.Version(job("acme")) != 1 {
		t.Errorf("job version = %d, want 1", e.fake.Version(job("acme")))
	}
	if countRows(t, e.pool) != 1 {
		t.Error("more than one instance row")
	}
	// A running instance stays running when Start is repeated.
	e.fake.SetPhase(job("acme"), nomadfake.Healthy)
	if _, err := e.svc.Refresh(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Start(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if got := e.instance(t, "acme").State; got != provision.StateRunning {
		t.Errorf("state after a repeated Start = %q, want running", got)
	}
}

// An existing instance keeps its image version: moving versions is the
// updater's job, not a side effect of a configuration change.
func TestStartKeepsTheInstanceVersion(t *testing.T) {
	e := newEnv(t)
	e.tenant(t, "acme", "active", true)
	ctx := context.Background()
	if err := e.svc.Start(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	opt := e.opt
	opt.Version = "0.8.0"
	newer := e.service(t, opt)
	if err := newer.Start(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if e.fake.Image(job("acme")) != "ghcr.io/tydewhatmay/sanholo:v0.7.1" || e.instance(t, "acme").ImageVersion != "0.7.1" {
		t.Errorf("a config change moved the instance to %q", e.fake.Image(job("acme")))
	}
	e.tenant(t, "fresh", "active", true)
	if err := newer.Start(ctx, "fresh"); err != nil {
		t.Fatal(err)
	}
	if e.fake.Image(job("fresh")) != "ghcr.io/tydewhatmay/sanholo:v0.8.0" {
		t.Errorf("a new instance got %q", e.fake.Image(job("fresh")))
	}
}

func TestTriggerFollowsTheInstanceToHealthy(t *testing.T) {
	e := newEnv(t)
	e.tenant(t, "acme", "active", true)
	e.fake.HealthyAfter = 3
	e.svc.Trigger("acme")
	e.svc.Wait()
	inst := e.instance(t, "acme")
	if inst.State != provision.StateRunning || inst.Health != provision.HealthHealthy || inst.HealthAt == nil || inst.FailureReason != "" {
		t.Errorf("instance = %+v", inst)
	}
}

func TestTriggerOnceAtATime(t *testing.T) {
	e := newEnv(t)
	e.tenant(t, "acme", "active", true)
	// The first start blocks at its first Nomad read, so every later trigger
	// arrives while one is in flight.
	release := make(chan struct{})
	e.fake.Override(func(r nomadfake.Request) (int, string, bool) {
		if r.Method == "GET" && r.Path == "/v1/job/"+job("acme") {
			<-release
		}
		return 0, "", false
	})
	e.fake.HealthyAfter = 2
	for range 10 {
		e.svc.Trigger("acme")
	}
	time.Sleep(100 * time.Millisecond) // let any second start reach the blocked read
	close(release)
	e.svc.Wait()
	// One start reads the job once, then the status polls read it twice.
	reads := 0
	for _, r := range e.fake.Requests() {
		if r.Method == "GET" && r.Path == "/v1/job/"+job("acme") {
			reads++
		}
	}
	if got := reads; got > 4 {
		t.Errorf("%d reads of the job: more than one start ran at once", got)
	}
	if e.posts() != 1 {
		t.Errorf("%d registrations from ten triggers, want 1", e.posts())
	}
}

func TestTriggerMarksATimeoutAsFailed(t *testing.T) {
	e := newEnv(t)
	e.tenant(t, "acme", "active", true)
	// Each status read moves the clock on, so the instance never becomes
	// healthy within the timeout.
	e.fake.Override(func(r nomadfake.Request) (int, string, bool) {
		if r.Method == "GET" && strings.HasSuffix(r.Path, "/allocations") {
			e.advance(4 * time.Minute)
		}
		return 0, "", false
	})
	e.svc.Trigger("acme")
	e.svc.Wait()
	inst := e.instance(t, "acme")
	if inst.State != provision.StateFailed || inst.FailureReason != provision.ReasonTimeout {
		t.Errorf("instance = %+v", inst)
	}
}

func TestTriggerReportsADeploymentFailure(t *testing.T) {
	e := newEnv(t)
	e.tenant(t, "acme", "active", true)
	e.fake.OnRegister = nomadfake.Failed
	e.svc.Trigger("acme")
	e.svc.Wait()
	inst := e.instance(t, "acme")
	if inst.State != provision.StateFailed || inst.FailureReason != provision.ReasonDeployFailed {
		t.Errorf("instance = %+v", inst)
	}
}

func TestStartFailuresAreRecordedWithFixedReasons(t *testing.T) {
	const leak = "LEAK-MARKER-job-spec-echo"
	for _, tc := range []struct {
		name   string
		status int
		want   string
	}{
		{"rejected", 400, provision.ReasonRegister},
		{"denied", 403, provision.ReasonDenied},
		{"down", 503, provision.ReasonUnreachable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.tenant(t, "acme", "active", true)
			e.fake.Override(func(r nomadfake.Request) (int, string, bool) { return tc.status, leak, true })
			err := e.svc.Start(context.Background(), "acme")
			if err == nil {
				t.Fatal("Start succeeded against a failing Nomad")
			}
			if strings.Contains(err.Error(), leak) || strings.Contains(e.logs.String(), leak) {
				t.Error("Nomad's response text reached an error or the log")
			}
			inst := e.instance(t, "acme")
			if inst.State != provision.StateFailed || inst.FailureReason != tc.want {
				t.Errorf("instance = %+v, want failed with %q", inst, tc.want)
			}
		})
	}
}

func TestStartRetriesAfterAFailure(t *testing.T) {
	e := newEnv(t)
	e.tenant(t, "acme", "active", true)
	e.fake.Override(func(nomadfake.Request) (int, string, bool) { return 503, "", true })
	if err := e.svc.Start(context.Background(), "acme"); err == nil {
		t.Fatal("want an error")
	}
	e.fake.Override(nil)
	if err := e.svc.Start(context.Background(), "acme"); err != nil {
		t.Fatal(err)
	}
	if inst := e.instance(t, "acme"); inst.State != provision.StatePending || inst.FailureReason != "" {
		t.Errorf("instance after the retry = %+v", inst)
	}
}

func TestStop(t *testing.T) {
	e := newEnv(t)
	e.tenant(t, "acme", "active", true)
	ctx := context.Background()
	if err := e.svc.Start(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	for range 2 { // repeatable
		if err := e.svc.Stop(ctx, "acme", false); err != nil {
			t.Fatal(err)
		}
	}
	if exists, stopped := e.fake.Has(job("acme")); !exists || !stopped {
		t.Errorf("job exists=%v stopped=%v after Stop", exists, stopped)
	}
	if got := e.instance(t, "acme").State; got != provision.StateStopped {
		t.Errorf("state = %q", got)
	}
	// A stopped instance is not refreshed back to life.
	e.fake.SetPhase(job("acme"), nomadfake.Healthy)
	if _, err := e.svc.Refresh(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if got := e.instance(t, "acme").State; got != provision.StateStopped {
		t.Errorf("state after Refresh = %q", got)
	}
	// Start brings it back.
	if err := e.svc.Start(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if _, stopped := e.fake.Has(job("acme")); stopped {
		t.Error("the job is still stopped after Start")
	}
	if got := e.instance(t, "acme").State; got != provision.StatePending {
		t.Errorf("state after Start = %q", got)
	}
	if err := e.svc.Stop(ctx, "acme", true); err != nil {
		t.Fatal(err)
	}
	if exists, _ := e.fake.Has(job("acme")); exists {
		t.Error("purge left the job")
	}
}

func TestStopFailureKeepsTheState(t *testing.T) {
	e := newEnv(t)
	e.tenant(t, "acme", "active", true)
	ctx := context.Background()
	if err := e.svc.Start(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	e.fake.Override(func(r nomadfake.Request) (int, string, bool) { return 503, "", r.Method == "DELETE" })
	if err := e.svc.Stop(ctx, "acme", false); err == nil {
		t.Fatal("Stop succeeded against a failing Nomad")
	}
	if got := e.instance(t, "acme").State; got == provision.StateStopped {
		t.Error("the instance was recorded stopped although Nomad did not stop it")
	}
}

func TestReconcile(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, id := range []string{"pend", "run", "fail", "stop"} {
		e.tenant(t, id, "active", true)
		if err := e.svc.Start(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	// "pend": became healthy while no one watched (control plane restart).
	e.fake.SetPhase(job("pend"), nomadfake.Healthy)
	// "run": healthy, then turns unhealthy.
	e.fake.SetPhase(job("run"), nomadfake.Healthy)
	e.svc.Reconcile(ctx)
	e.fake.SetPhase(job("run"), nomadfake.Unhealthy)
	// "fail": failed, then Nomad recovers it.
	e.fake.SetPhase(job("fail"), nomadfake.Failed)
	e.svc.Reconcile(ctx)
	if got := e.instance(t, "fail"); got.State != provision.StateFailed {
		t.Fatalf("fail: %+v", got)
	}
	e.fake.SetPhase(job("fail"), nomadfake.Healthy)
	if err := e.svc.Stop(ctx, "stop", false); err != nil {
		t.Fatal(err)
	}
	e.fake.SetPhase(job("stop"), nomadfake.Healthy)
	e.advance(2 * time.Minute)
	e.svc.Reconcile(ctx)

	want := map[string][2]string{
		"pend": {provision.StateRunning, provision.HealthHealthy},
		"run":  {provision.StateRunning, provision.HealthUnhealthy},
		"fail": {provision.StateRunning, provision.HealthHealthy},
		"stop": {provision.StateStopped, provision.HealthUnknown},
	}
	for id, w := range want {
		if got := e.instance(t, id); got.State != w[0] || got.Health != w[1] {
			t.Errorf("%s: state %q health %q, want %v", id, got.State, got.Health, w)
		}
	}
}

func TestReconcileMarksAMissingJobFailed(t *testing.T) {
	e := newEnv(t)
	e.tenant(t, "acme", "active", true)
	ctx := context.Background()
	if err := e.svc.Start(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	e.fake.SetPhase(job("acme"), nomadfake.Healthy)
	e.svc.Reconcile(ctx)
	if err := e.nc.Stop(ctx, job("acme"), true); err != nil { // someone purged it behind our back
		t.Fatal(err)
	}
	e.advance(2 * time.Minute)
	e.svc.Reconcile(ctx)
	inst := e.instance(t, "acme")
	if inst.State != provision.StateFailed || inst.FailureReason != provision.ReasonMissing {
		t.Errorf("instance = %+v", inst)
	}
}

func TestRestart(t *testing.T) {
	e := newEnv(t)
	e.tenant(t, "acme", "active", true)
	ctx := context.Background()
	if err := e.svc.Start(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	e.fake.SetPhase(job("acme"), nomadfake.Healthy)
	if _, err := e.svc.Refresh(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.PrepareRestart(ctx, tx, "acme"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil { // the audit write failed: nothing may change
		t.Fatal(err)
	}
	if got := e.instance(t, "acme").State; got != provision.StateRunning {
		t.Fatalf("a rolled-back PrepareRestart changed the state to %q", got)
	}
	tx, _ = e.pool.Begin(ctx)
	if err := e.svc.PrepareRestart(ctx, tx, "acme"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.instance(t, "acme").State; got != provision.StatePending {
		t.Errorf("state after PrepareRestart = %q", got)
	}
	if err := e.svc.Restart(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	if e.fake.Restarts(job("acme")) != 1 {
		t.Errorf("allocation restarted %d times, want 1", e.fake.Restarts(job("acme")))
	}
}

func TestRestartRefusals(t *testing.T) {
	e := newEnv(t)
	e.tenant(t, "none", "active", true)
	e.tenant(t, "stopped", "active", true)
	ctx := context.Background()
	if err := e.svc.Start(ctx, "stopped"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Stop(ctx, "stopped", false); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]error{"none": provision.ErrNotFound, "stopped": provision.ErrNotAllowed} {
		tx, _ := e.pool.Begin(ctx)
		if err := e.svc.PrepareRestart(ctx, tx, id); !errors.Is(err, want) {
			t.Errorf("PrepareRestart(%s) = %v, want %v", id, err, want)
		}
		_ = tx.Rollback(ctx)
	}
}

// With no running allocation (a failed job) a restart registers the job
// again, which makes Nomad evaluate it.
func TestRestartOfAFailedJobRegistersAgain(t *testing.T) {
	e := newEnv(t)
	e.tenant(t, "acme", "active", true)
	ctx := context.Background()
	if err := e.svc.Start(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	e.fake.SetPhase(job("acme"), nomadfake.Failed)
	before := e.posts()
	if err := e.svc.Restart(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	if e.posts() != before+1 {
		t.Errorf("registrations %d -> %d, want one more", before, e.posts())
	}
}

// A rotated token restarts a running bot so it reads the new one from qm.
func TestTriggerRestartRestartsARunningBot(t *testing.T) {
	e := newEnv(t)
	e.tenant(t, "acme", "active", true)
	ctx := context.Background()
	if err := e.svc.Start(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	e.fake.SetPhase(job("acme"), nomadfake.Healthy)
	if _, err := e.svc.Refresh(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	e.svc.TriggerRestart("acme")
	e.svc.Wait()
	if e.fake.Restarts(job("acme")) != 1 {
		t.Errorf("allocation restarted %d times, want 1", e.fake.Restarts(job("acme")))
	}
	// With no instance at all it simply starts one.
	e.fake.HealthyAfter = 2
	e.tenant(t, "fresh", "active", true)
	e.svc.TriggerRestart("fresh")
	e.svc.Wait()
	if exists, _ := e.fake.Has(job("fresh")); !exists {
		t.Error("TriggerRestart did not start a tenant that had no instance")
	}
}

func TestDisabledService(t *testing.T) {
	e := newEnv(t)
	e.tenant(t, "acme", "active", true)
	off, err := nomad.New(nomad.Config{})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := provision.New(e.pool, off, provision.Options{Version: "not a version"})
	if err != nil {
		t.Fatalf("a disabled service must not check its options: %v", err)
	}
	defer svc.Close()
	if svc.Enabled() {
		t.Error("enabled without Nomad")
	}
	svc.Trigger("acme")
	svc.TriggerRestart("acme")
	svc.Wait()
	if err := svc.Start(context.Background(), "acme"); !errors.Is(err, provision.ErrDisabled) {
		t.Errorf("Start = %v", err)
	}
	if inst, err := svc.Get(context.Background(), "acme"); inst != nil || err != nil {
		t.Errorf("Get = %v, %v", inst, err)
	}
	var nilSvc *provision.Service
	nilSvc.Trigger("acme")
	nilSvc.Close()
	if nilSvc.Enabled() {
		t.Error("nil service is enabled")
	}
	if inst, err := nilSvc.Get(context.Background(), "acme"); inst != nil || err != nil {
		t.Errorf("nil Get = %v, %v", inst, err)
	}
	if countRows(t, e.pool) != 0 {
		t.Error("a disabled service wrote an instance row")
	}
}

func TestNewValidatesWhenEnabled(t *testing.T) {
	e := newEnv(t)
	bad := func(f func(*provision.Options)) provision.Options {
		o := e.opt
		f(&o)
		return o
	}
	for name, o := range map[string]provision.Options{
		"no version":         bad(func(o *provision.Options) { o.Version = "" }),
		"latest":             bad(func(o *provision.Options) { o.Version = "latest" }),
		"wrong namespace":    bad(func(o *provision.Options) { o.Settings.Namespace = "default" }),
		"invalid settings":   bad(func(o *provision.Options) { o.Settings.MemoryMB = 1 }),
		"http qm":            bad(func(o *provision.Options) { o.Settings.QMAddr = "http://qm.example.org" }),
		"no proxy key":       bad(func(o *provision.Options) { o.Settings.ProxyPublicKeys = "" }),
		"zero-value options": {},
	} {
		if _, err := provision.New(e.pool, e.nc, o); err == nil {
			t.Errorf("%s: New succeeded", name)
		}
	}
}

// The job and the table hold no secret: not Nomad's token, not anything the
// control plane holds for qm or the game.
func TestNoSecretInTheJobTheTableOrTheLog(t *testing.T) {
	e := newEnv(t)
	e.tenant(t, "acme", "active", true)
	e.fake.HealthyAfter = 2
	e.svc.Trigger("acme")
	e.svc.Wait()
	for _, r := range e.fake.Requests() {
		// ${NOMAD_SECRETS_DIR} is Nomad's own variable, not a secret.
		body := strings.ReplaceAll(r.Body, "NOMAD_SECRETS_DIR", "")
		if r.Method == "POST" && (strings.Contains(body, nomadToken) || strings.Contains(strings.ToLower(body), "secret")) {
			t.Errorf("the registration body carries a secret: %.200s", r.Body)
		}
		if r.Token != nomadToken {
			t.Error("a request went out without the namespace token")
		}
	}
	var row string
	if err := e.pool.QueryRow(context.Background(), `SELECT t::text FROM tenant_instances t`).Scan(&row); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(row, nomadToken) {
		t.Error("the instance row holds the token")
	}
	if strings.Contains(e.logs.String(), nomadToken) {
		t.Error("the log holds the token")
	}
}

func TestNext(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	timeout := 10 * time.Minute
	fresh := func(state string) provision.Instance {
		return provision.Instance{State: state, Health: provision.HealthUnknown, RequestedAt: now.Add(-time.Minute)}
	}
	old := func(state string) provision.Instance {
		i := fresh(state)
		i.RequestedAt = now.Add(-time.Hour)
		return i
	}
	failedInst := fresh(provision.StateFailed)
	failedInst.FailureReason = provision.ReasonTimeout
	failedInst.Health = provision.HealthUnknown
	tests := []struct {
		name                  string
		inst                  provision.Instance
		phase                 nomad.Phase
		state, health, reason string
	}{
		{"pending to healthy", fresh("pending"), nomad.PhaseHealthy, "running", "healthy", ""},
		{"pending waits", fresh("pending"), nomad.PhasePending, "pending", "starting", ""},
		{"pending times out", old("pending"), nomad.PhasePending, "failed", "unknown", provision.ReasonTimeout},
		{"unhealthy pending times out", old("pending"), nomad.PhaseUnhealthy, "failed", "unhealthy", provision.ReasonTimeout},
		{"unhealthy pending waits", fresh("pending"), nomad.PhaseUnhealthy, "pending", "unhealthy", ""},
		{"running stays running when restarting", old("running"), nomad.PhasePending, "running", "starting", ""},
		{"running unhealthy", old("running"), nomad.PhaseUnhealthy, "running", "unhealthy", ""},
		{"failed deployment", fresh("pending"), nomad.PhaseFailed, "failed", "unhealthy", provision.ReasonDeployFailed},
		{"missing job", fresh("running"), nomad.PhaseAbsent, "failed", "unknown", provision.ReasonMissing},
		{"stopped", fresh("running"), nomad.PhaseStopped, "stopped", "unknown", ""},
		{"failed recovers", failedInst, nomad.PhaseHealthy, "running", "healthy", ""},
		{"failed stays failed while pending", failedInst, nomad.PhasePending, "failed", "unknown", provision.ReasonTimeout},
		{"failed stays failed while unhealthy", failedInst, nomad.PhaseUnhealthy, "failed", "unknown", provision.ReasonTimeout},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, h, r := provision.Next(tc.inst, nomad.Status{Phase: tc.phase}, now, timeout)
			if s != tc.state || h != tc.health || r != tc.reason {
				t.Errorf("Next = (%q, %q, %q), want (%q, %q, %q)", s, h, r, tc.state, tc.health, tc.reason)
			}
		})
	}
}

// A refresh that read Nomad before a stop must not bring the instance back.
func TestRefreshDoesNotOverwriteAConcurrentStop(t *testing.T) {
	e := newEnv(t)
	e.tenant(t, "acme", "active", true)
	ctx := context.Background()
	if err := e.svc.Start(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	e.fake.SetPhase(job("acme"), nomadfake.Healthy)
	e.fake.Override(func(r nomadfake.Request) (int, string, bool) {
		if strings.HasSuffix(r.Path, "/allocations") { // the refresh has read the job; the stop lands now
			if _, err := e.pool.Exec(ctx, `UPDATE tenant_instances SET state = 'stopped' WHERE tenant_id = 'acme'`); err != nil {
				t.Error(err)
			}
		}
		return 0, "", false
	})
	if _, err := e.svc.Refresh(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if got := e.instance(t, "acme").State; got != provision.StateStopped {
		t.Errorf("state = %q: a stale refresh overwrote the stop", got)
	}
}
