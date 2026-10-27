package provision_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lhaig/sanholo-cloud/internal/nomad/nomadfake"
	"github.com/lhaig/sanholo-cloud/internal/provision"
)

func waitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// gate holds the first registration inside Nomad until released, so a test
// can run other operations while a start is in flight.
type gate struct {
	reached chan struct{}
	release chan struct{}
	seen    atomic.Int32
}

func holdFirstRegistration(e *env) *gate {
	g := &gate{reached: make(chan struct{}, 1), release: make(chan struct{})}
	e.fake.Override(func(r nomadfake.Request) (int, string, bool) {
		if r.Method == "POST" && r.Path == "/v1/jobs" && g.seen.Add(1) == 1 {
			g.reached <- struct{}{}
			<-g.release
		}
		return 0, "", false
	})
	return g
}

func (e *env) running(t *testing.T, id string) {
	t.Helper()
	ctx := context.Background()
	e.tenant(t, id, "active", true)
	if err := e.svc.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	e.fake.SetPhase(job(id), nomadfake.Healthy)
	if inst, err := e.svc.Refresh(ctx, id); err != nil || inst.State != provision.StateRunning {
		t.Fatalf("%s: %+v %v", id, inst, err)
	}
}

func TestStartCreatesTheVolumeBeforeTheJob(t *testing.T) {
	e := newEnv(t)
	e.tenant(t, "acme", "active", true)
	ctx := context.Background()
	if err := e.svc.Start(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	create, register := -1, -1
	for i, r := range e.fake.Requests() {
		switch {
		case r.Method == "PUT" && r.Path == "/v1/volume/host/create":
			create = i
		case r.Method == "POST" && r.Path == "/v1/jobs":
			register = i
		}
	}
	if create < 0 || register < 0 || create > register {
		t.Fatalf("volume create at %d, job register at %d: the volume must come first", create, register)
	}
	spec := e.fake.VolumeSpec("sanholo-tenant-acme")
	if spec["PluginID"] != "mkdir" || spec["Namespace"] != "sanholo" {
		t.Errorf("volume spec = %v", spec)
	}
	// Repeating Start reuses the volume.
	if err := e.svc.Start(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if n := e.fake.Count("PUT", "/v1/volume/host/create"); n != 1 {
		t.Errorf("%d volume creations, want 1", n)
	}
	// The nomad job is placed: the fake leaves a job without its volume
	// unplaced, so an allocation shows the volume is there.
	e.fake.SetPhase(job("acme"), nomadfake.Healthy)
	if st, err := e.svc.Refresh(ctx, "acme"); err != nil || st.State != provision.StateRunning {
		t.Errorf("not running with its volume: %+v %v", st, err)
	}
}

func TestVolumeFailureIsRecordedAndTheJobIsNotRegistered(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		want   string
	}{
		{"cannot place", 500, provision.ReasonVolume},
		{"rejected", 400, provision.ReasonVolume},
		{"denied", 403, provision.ReasonDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.tenant(t, "acme", "active", true)
			e.fake.Override(func(r nomadfake.Request) (int, string, bool) {
				return tc.status, "LEAK-volume-text", r.Path == "/v1/volume/host/create"
			})
			err := e.svc.Start(context.Background(), "acme")
			if err == nil {
				t.Fatal("Start succeeded without a volume")
			}
			if e.posts() != 0 {
				t.Error("the job was registered without its volume")
			}
			inst := e.instance(t, "acme")
			if inst.State != provision.StateFailed || inst.FailureReason != tc.want {
				t.Errorf("instance = %+v, want failed with %q", inst, tc.want)
			}
			if strings.Contains(e.logs.String(), "LEAK-volume-text") || strings.Contains(err.Error(), "LEAK-volume-text") {
				t.Error("Nomad's text reached the log or the error")
			}
		})
	}
}

// A Start that read the instance before an Update must not register its old
// version over the Update's.
func TestUpdateWaitsForAStartInFlight(t *testing.T) {
	e := newEnv(t)
	e.tenant(t, "acme", "active", true)
	g := holdFirstRegistration(e)
	ctx := context.Background()
	var wg sync.WaitGroup
	var startErr, updErr error
	wg.Add(1)
	go func() { defer wg.Done(); startErr = e.svc.Start(ctx, "acme") }()
	<-g.reached
	wg.Add(1)
	go func() { defer wg.Done(); updErr = e.svc.Update(ctx, "acme", "0.8.0", "") }()
	time.Sleep(200 * time.Millisecond)
	if n := e.posts(); n != 1 {
		t.Errorf("%d registrations while the start was in flight, want 1: the update did not wait", n)
	}
	close(g.release)
	wg.Wait()
	if startErr != nil || updErr != nil {
		t.Fatalf("start %v, update %v", startErr, updErr)
	}
	if got := e.fake.Image(job("acme")); got != "ghcr.io/tydewhatmay/sanholo:v0.8.0" {
		t.Errorf("the job runs %q: the old start overwrote the update", got)
	}
	if got := e.instance(t, "acme").ImageVersion; got != "0.8.0" {
		t.Errorf("instance version = %q", got)
	}
}

func TestRestartAskedDuringAStartIsQueuedNotDropped(t *testing.T) {
	e := newEnv(t)
	e.tenant(t, "acme", "active", true)
	g := holdFirstRegistration(e)
	e.svc.Trigger("acme")
	<-g.reached
	e.svc.TriggerRestart("acme") // a token rotated while the start runs
	close(g.release)
	// The start is done; the queued restart finds no running allocation and
	// registers the job again.
	waitFor(t, "the queued restart", func() bool { return e.posts() >= 2 })
	e.fake.SetPhase(job("acme"), nomadfake.Healthy)
	e.svc.Wait()
	if n := e.posts(); n != 2 {
		t.Errorf("%d registrations, want 2 (start, queued restart)", n)
	}
}

func TestLockIsPerTenant(t *testing.T) {
	e := newEnv(t)
	e.tenant(t, "acme", "active", true)
	e.tenant(t, "beta", "active", true)
	g := holdFirstRegistration(e)
	ctx := context.Background()
	done := make(chan error, 1)
	go func() { done <- e.svc.Start(ctx, "acme") }()
	<-g.reached
	finished := make(chan error, 1)
	go func() { finished <- e.svc.Start(ctx, "beta") }()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a start of one tenant waits for another tenant's")
	}
	close(g.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// Nomad decides the deployment verdict once; the reconcile worker sees a
// check that fails later, and the recovery, through the live checks.
func TestReconcileFollowsTheLiveChecks(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.running(t, "acme")
	if inst := e.instance(t, "acme"); inst.UnhealthyAt != nil {
		t.Fatalf("unhealthy before anything failed: %+v", inst)
	}
	e.fake.SetCheck(job("acme"), "failure")
	e.advance(2 * time.Minute)
	e.svc.Reconcile(ctx)
	inst := e.instance(t, "acme")
	if inst.State != provision.StateRunning || inst.Health != provision.HealthUnhealthy || inst.UnhealthyAt == nil {
		t.Fatalf("after a failing check: %+v", inst)
	}
	seen := *inst.UnhealthyAt
	e.fake.SetCheck(job("acme"), "success")
	e.advance(2 * time.Minute)
	e.svc.Reconcile(ctx)
	inst = e.instance(t, "acme")
	if inst.Health != provision.HealthHealthy {
		t.Errorf("after the check recovered: %+v", inst)
	}
	if inst.UnhealthyAt == nil || !inst.UnhealthyAt.Equal(seen) {
		t.Errorf("the time of the last failure was not kept: %v, want %v", inst.UnhealthyAt, seen)
	}
}

func (e *env) release(t *testing.T, version, state string, target bool) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(),
		`INSERT INTO releases (version, state, is_target) VALUES ($1, $2, $3)`, version, state, target); err != nil {
		t.Fatal(err)
	}
}

func (e *env) otherSettings(t *testing.T, mut func(*provision.Options)) *provision.Service {
	t.Helper()
	o := e.opt
	mut(&o)
	return e.service(t, o)
}

// A rotated proxy key changes every tenant's job. The reconcile worker finds
// the jobs that differ from the expected spec and registers them again.
func TestReconcileReRegistersADriftedJob(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.running(t, "acme")
	before := e.posts()
	rotated := e.otherSettings(t, func(o *provision.Options) {
		o.Settings.ProxyPublicKeys = nomadfake.ProxyKeyPEM(2) + nomadfake.ProxyKeyPEM(1)
	})
	rotated.Reconcile(ctx)
	if e.posts() != before+1 {
		t.Fatalf("registrations %d -> %d, want one more", before, e.posts())
	}
	if !strings.Contains(string(mustJSON(t, e.fake.Job(job("acme")))), strings.TrimSpace(strings.Split(nomadfake.ProxyKeyPEM(2), "\n")[1])) {
		t.Error("the new key is not in the registered job")
	}
	// Converged: another pass registers nothing.
	rotated.Reconcile(ctx)
	if e.posts() != before+1 {
		t.Errorf("a converged job was registered again (%d)", e.posts())
	}
}

func TestDriftIsFixedAFewTenantsAtATime(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, id := range []string{"t01", "t02", "t03"} {
		e.running(t, id)
	}
	before := e.posts()
	rotated := e.otherSettings(t, func(o *provision.Options) {
		o.Settings.ProxyPublicKeys = nomadfake.ProxyKeyPEM(2)
		o.DriftPerPass = 2
	})
	rotated.Reconcile(ctx)
	if got := e.posts() - before; got != 2 {
		t.Errorf("first pass registered %d jobs, want 2", got)
	}
	rotated.Reconcile(ctx)
	if got := e.posts() - before; got != 3 {
		t.Errorf("two passes registered %d jobs, want 3", got)
	}
}

func TestDriftIsLeftToTheUpdaterWhileATenantIsInARollout(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.running(t, "acme")
	e.running(t, "beta")
	e.release(t, "0.8.0", "rolling", false)
	for id, st := range map[string]string{"acme": "updating", "beta": "done"} {
		if _, err := e.pool.Exec(ctx, `INSERT INTO rollout_tenants (release_version, tenant_id, batch, state, previous_version) VALUES ('0.8.0', $1, 0, $2, '0.7.1')`, id, st); err != nil {
			t.Fatal(err)
		}
	}
	rotated := e.otherSettings(t, func(o *provision.Options) { o.Settings.ProxyPublicKeys = nomadfake.ProxyKeyPEM(2) })
	before := e.fake.Version(job("acme"))
	beforeBeta := e.fake.Version(job("beta"))
	rotated.Reconcile(ctx)
	if e.fake.Version(job("acme")) != before {
		t.Error("the reconcile worker re-registered a tenant the updater is moving")
	}
	if e.fake.Version(job("beta")) == beforeBeta {
		t.Error("a tenant whose row is done was left drifted")
	}
}

// A tenant that was not running when a rollout was planned is skipped; when
// it runs later, still on the old version, the reconcile worker moves it.
func TestReconcileMovesALaggingInstanceToTheFleetTarget(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.running(t, "acme")
	e.release(t, "0.8.0", "done", true)
	e.svc.Reconcile(ctx)
	inst := e.instance(t, "acme")
	if inst.ImageVersion != "0.8.0" || inst.State != provision.StatePending {
		t.Fatalf("instance = %+v, want moved to 0.8.0 and pending", inst)
	}
	if got := e.fake.Image(job("acme")); got != "ghcr.io/tydewhatmay/sanholo:v0.8.0" {
		t.Errorf("job image = %q", got)
	}
}

func TestLaggingInstanceStaysPutWhileARolloutIsUnderWay(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.running(t, "acme")
	e.release(t, "0.8.0", "done", true)
	e.release(t, "0.9.0", "rolling", false)
	e.svc.Reconcile(ctx)
	if got := e.instance(t, "acme").ImageVersion; got != "0.7.1" {
		t.Errorf("version = %q: the worker moved a tenant while a rollout was under way", got)
	}
}

func TestSoakVerdicts(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.running(t, "acme")
	base, err := e.svc.SoakBaseline(ctx, "acme")
	if err != nil || base.AllocID == "" {
		t.Fatalf("baseline = %+v %v", base, err)
	}
	verdict := func() provision.SoakVerdict {
		t.Helper()
		v, err := e.svc.CheckSoak(ctx, "acme", base)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if v := verdict(); v != provision.SoakOK {
		t.Errorf("healthy: %q", v)
	}
	e.fake.SetCheck(job("acme"), "pending")
	if v := verdict(); v != provision.SoakOK {
		t.Errorf("a check that has not run yet: %q, want ok", v)
	}
	e.fake.SetCheck(job("acme"), "failure")
	if v := verdict(); v != provision.SoakFailing {
		t.Errorf("failing check: %q", v)
	}
	e.fake.SetCheck(job("acme"), "")
	e.fake.BumpRestarts(job("acme"))
	if v := verdict(); v != provision.SoakRestarted {
		t.Errorf("restart: %q", v)
	}
	e.fake.ReplaceAlloc(job("acme"))
	e.fake.SetPhase(job("acme"), nomadfake.Healthy)
	if v := verdict(); v != provision.SoakReplaced {
		t.Errorf("replacement: %q", v)
	}
	if err := e.svc.Stop(ctx, "acme", false); err != nil {
		t.Fatal(err)
	}
	if v := verdict(); v != provision.SoakFailing {
		t.Errorf("stopped: %q", v)
	}
}

func TestDeleteVolumeNeedsTheJobStoppedFirst(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.running(t, "acme")
	if err := e.svc.DeleteVolume(ctx, "acme"); err == nil {
		t.Fatal("the volume of a running job was deleted")
	}
	if err := e.svc.Stop(ctx, "acme", false); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Stop(ctx, "acme", true); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.DeleteVolume(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if len(e.fake.Volumes()) != 0 {
		t.Errorf("volumes left: %v", e.fake.Volumes())
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func jobMeta(t *testing.T, e *env, id string) map[string]any {
	t.Helper()
	m, _ := e.fake.Job(job(id))["Meta"].(map[string]any)
	return m
}

// On real Nomad, registering an identical spec is only a new evaluation: it
// does not revive a failed job. A restart with no running allocation changes
// the job meta so Nomad makes a new version, which replaces the allocation.
func TestRestartWithoutARunningAllocationForcesANewJobVersion(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.tenant(t, "acme", "active", true)
	if err := e.svc.Start(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	spec := jobMeta(t, e, "acme")["sanholo.spec"]
	if _, ok := jobMeta(t, e, "acme")["sanholo.restart"]; ok {
		t.Fatal("a job that was never restarted carries a restart counter")
	}
	e.fake.SetPhase(job("acme"), nomadfake.Failed)
	for i, want := range []string{"1", "2"} {
		v := e.fake.Version(job("acme"))
		if err := e.svc.Restart(ctx, "acme"); err != nil {
			t.Fatal(err)
		}
		if got := e.fake.Version(job("acme")); got != v+1 {
			t.Errorf("restart %d: job version %d -> %d, want a new version", i+1, v, got)
		}
		m := jobMeta(t, e, "acme")
		if m["sanholo.restart"] != want {
			t.Errorf("restart %d: counter %v, want %s", i+1, m["sanholo.restart"], want)
		}
		if m["sanholo.spec"] != spec {
			t.Errorf("restart %d: the spec hash changed; a restart must not look like drift", i+1)
		}
		e.fake.SetPhase(job("acme"), nomadfake.Failed)
	}
	if got := e.instance(t, "acme").RestartSeq; got != 2 {
		t.Errorf("stored counter = %d, want 2", got)
	}
}

// A restart that has a running allocation to restart changes nothing in the
// job.
func TestRestartOfARunningAllocationLeavesTheJobAlone(t *testing.T) {
	e := newEnv(t)
	e.running(t, "acme")
	posts := e.posts()
	if err := e.svc.Restart(context.Background(), "acme"); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	if e.posts() != posts || e.fake.Restarts(job("acme")) != 1 || e.instance(t, "acme").RestartSeq != 0 {
		t.Errorf("posts %d -> %d, restarts %d, counter %d", posts, e.posts(), e.fake.Restarts(job("acme")), e.instance(t, "acme").RestartSeq)
	}
}

// The counter is not drift, and no later render undoes it: a repeated start,
// the reconcile worker (drift and fleet-target moves) and the updater's
// update and revert all carry the stored value.
func TestTheRestartCounterSurvivesEveryRender(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.running(t, "acme")
	e.fake.SetPhase(job("acme"), nomadfake.Failed)
	if err := e.svc.Restart(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	e.fake.SetPhase(job("acme"), nomadfake.Failed)
	e.svc.Wait()
	if jobMeta(t, e, "acme")["sanholo.restart"] != "1" {
		t.Fatal("setup: no counter")
	}
	posts := e.posts()
	// Not drift, not a different version.
	if err := e.svc.Start(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if st, err := e.svc.CheckUpdate(ctx, "acme", "0.7.1", ""); err != nil || st == provision.UpdateReverted {
		t.Errorf("CheckUpdate after a restart = %q, %v: the bump looks like a revert", st, err)
	}
	e.fake.SetPhase(job("acme"), nomadfake.Healthy)
	if _, err := e.svc.Refresh(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	e.svc.Reconcile(ctx)
	if e.posts() != posts {
		t.Errorf("start or reconcile registered the job %d more times for a restart counter", e.posts()-posts)
	}
	// Drift fix by the reconcile worker.
	rotated := e.otherSettings(t, func(o *provision.Options) { o.Settings.ProxyPublicKeys = nomadfake.ProxyKeyPEM(2) })
	rotated.Reconcile(ctx)
	if e.posts() != posts+1 || jobMeta(t, e, "acme")["sanholo.restart"] != "1" {
		t.Errorf("after a drift fix: posts +%d, counter %v", e.posts()-posts, jobMeta(t, e, "acme")["sanholo.restart"])
	}
	// The updater's update and its revert.
	for _, v := range []string{"0.8.0", "0.7.1"} {
		if err := e.svc.Update(ctx, "acme", v, ""); err != nil {
			t.Fatal(err)
		}
		if got := jobMeta(t, e, "acme")["sanholo.restart"]; got != "1" {
			t.Errorf("after an update to %s the counter is %v, want 1", v, got)
		}
	}
	// A move to the fleet target.
	e.fake.SetPhase(job("acme"), nomadfake.Healthy)
	if _, err := e.svc.Refresh(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	e.release(t, "0.9.0", "done", true)
	e.advance(2 * time.Minute)
	e.svc.Reconcile(ctx)
	if e.fake.Image(job("acme")) != "ghcr.io/tydewhatmay/sanholo:v0.9.0" || jobMeta(t, e, "acme")["sanholo.restart"] != "1" {
		t.Errorf("fleet-target move: image %q counter %v", e.fake.Image(job("acme")), jobMeta(t, e, "acme")["sanholo.restart"])
	}
}
