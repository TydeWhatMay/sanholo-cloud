package updater_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/lhaig/sanholo-cloud/internal/nomad/nomadfake"
	"github.com/lhaig/sanholo-cloud/internal/provision"
	"github.com/lhaig/sanholo-cloud/internal/updater"
)

// withSoak turns the soaks on (the other tests run without) and returns the
// environment.
func withSoak(e *env) *env {
	e.opt.CanarySoak, e.opt.BatchSoak = time.Hour, 10*time.Minute
	e.upd = e.newUpdater()
	return e
}

// Nomad's deployment verdict comes 30 seconds in; the client says "starting"
// for up to ten minutes. The canary is therefore watched for an hour before
// it counts as done, and the next batch waits for that.
func TestSoakHoldsTheCanaryForAnHourAndABatchForTenMinutes(t *testing.T) {
	e := withSoak(newEnv(t, 1))
	e.tenants(t, "t01", "t02")
	e.release(t, "0.8.0", "")
	if err := e.start(t, "0.8.0"); err != nil {
		t.Fatal(err)
	}
	e.step(t, 1)
	e.healthy("t01")
	e.step(t, 1) // the deployment is healthy: the soak starts
	if r := e.row(t, "0.8.0", "t01"); r.State != updater.RowUpdating || r.SoakStartedAt == nil || r.SoakAlloc == "" {
		t.Fatalf("canary row after the deployment turned healthy: %+v", r)
	}
	e.advance(59 * time.Minute)
	e.step(t, 2)
	if got := e.rowState(t, "0.8.0", "t01"); got != updater.RowUpdating {
		t.Fatalf("canary after 59 minutes: %q, want still updating", got)
	}
	if e.posts("t02") != 1 {
		t.Error("the next batch started before the canary's soak ended")
	}
	e.advance(2 * time.Minute)
	e.step(t, 1)
	if got := e.rowState(t, "0.8.0", "t01"); got != updater.RowDone {
		t.Fatalf("canary after an hour: %q, want done", got)
	}
	e.step(t, 1) // batch 1 begins
	if e.posts("t02") != 2 {
		t.Fatal("the next batch did not start after the canary")
	}
	e.healthy("t02")
	e.step(t, 1)
	e.advance(9 * time.Minute)
	e.step(t, 2)
	if got := e.rowState(t, "0.8.0", "t02"); got != updater.RowUpdating {
		t.Fatalf("batch tenant after 9 minutes: %q, want still updating", got)
	}
	e.advance(2 * time.Minute)
	e.step(t, 2)
	if got := e.rowState(t, "0.8.0", "t02"); got != updater.RowDone {
		t.Fatalf("batch tenant after 11 minutes: %q, want done", got)
	}
}

// During the soak a failing live check, a task restart or another allocation
// is a failed update: the rollout halts and the tenant goes back.
func TestSoakFailureHaltsAndRevertsTheTenant(t *testing.T) {
	for _, tc := range []struct {
		name   string
		break_ func(e *env)
		reason string
	}{
		{"failing check", func(e *env) { e.fake.SetCheck(job("t01"), "failure") }, provision.ReasonSoakCheck},
		{"task restart", func(e *env) { e.fake.BumpRestarts(job("t01")) }, provision.ReasonSoakRestart},
		{"replaced allocation", func(e *env) {
			e.fake.ReplaceAlloc(job("t01"))
			e.fake.SetPhase(job("t01"), nomadfake.Healthy)
		}, provision.ReasonSoakReplaced},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := withSoak(newEnv(t, 1))
			e.tenants(t, "t01", "t02")
			e.release(t, "0.8.0", "")
			if err := e.start(t, "0.8.0"); err != nil {
				t.Fatal(err)
			}
			e.step(t, 1)
			e.healthy("t01")
			e.step(t, 1)
			e.advance(20 * time.Minute)
			e.step(t, 1)
			if got := e.rowState(t, "0.8.0", "t01"); got != updater.RowUpdating {
				t.Fatalf("a quiet soak ended the update early: %q", got)
			}
			tc.break_(e)
			e.step(t, 1)
			rel := e.release1(t, "0.8.0")
			if rel.State != updater.StateHalted || rel.HaltedTenant != "t01" || rel.HaltReason != tc.reason {
				t.Fatalf("release = %+v, want halted on t01 with %q", rel, tc.reason)
			}
			if got := e.fake.Image(job("t01")); got != oldImage {
				t.Errorf("the failed tenant was not put back: %q", got)
			}
			if e.posts("t02") != 1 {
				t.Error("a tenant not yet reached was touched")
			}
			if e.instance(t, "t01").UnhealthyAt == nil {
				t.Error("the canary's health failure was not recorded")
			}
		})
	}
}

// Nomad being unreadable is not a failed soak, and it does not end one.
func TestSoakWaitsWhenNomadCannotBeRead(t *testing.T) {
	e := withSoak(newEnv(t, 1))
	e.tenants(t, "t01")
	e.release(t, "0.8.0", "")
	if err := e.start(t, "0.8.0"); err != nil {
		t.Fatal(err)
	}
	e.step(t, 1)
	e.healthy("t01")
	e.step(t, 1)
	e.advance(2 * time.Hour)
	e.fake.Override(func(r nomadfake.Request) (int, string, bool) { return 503, "", true })
	e.step(t, 2)
	e.fake.Override(nil)
	if got := e.rowState(t, "0.8.0", "t01"); got != updater.RowUpdating {
		t.Fatalf("with Nomad down the row is %q, want still updating", got)
	}
	e.step(t, 1)
	if got := e.rowState(t, "0.8.0", "t01"); got != updater.RowDone {
		t.Fatalf("once Nomad answers the row is %q, want done", got)
	}
}

func (e *env) markUnhealthy(t *testing.T, id string, ago time.Duration) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(),
		`UPDATE tenant_instances SET last_unhealthy_at = $2 WHERE tenant_id = $1`, id, e.clock().Add(-ago)); err != nil {
		t.Fatal(err)
	}
}

func TestRolloutIsRefusedWhileTheCanaryWasUnhealthyInTheLastHour(t *testing.T) {
	e := newEnv(t, 5)
	e.tenants(t, "t01", "t02")
	e.release(t, "0.8.0", "")
	if err := e.tx(t, func(tx pgx.Tx) error { return e.upd.SetCanaryTx(context.Background(), tx, "t02") }); err != nil {
		t.Fatal(err)
	}
	e.markUnhealthy(t, "t02", 30*time.Minute)
	if err := e.start(t, "0.8.0"); !errors.Is(err, updater.ErrCanaryUnhealthy) {
		t.Fatalf("start = %v, want ErrCanaryUnhealthy", err)
	}
	if r := e.release1(t, "0.8.0"); r.State != updater.StateAvailable {
		t.Errorf("a refused start left the release %q", r.State)
	}
	if n := e.count(t, `SELECT count(*) FROM rollout_tenants`); n != 0 {
		t.Errorf("a refused start left %d rows", n)
	}
	// An unhealthy tenant that is not the canary does not hold it back, and
	// neither does a failure more than an hour old.
	e.markUnhealthy(t, "t02", 0)
	e.markUnhealthy(t, "t01", 5*time.Minute)
	e.markUnhealthy(t, "t02", 61*time.Minute)
	if err := e.start(t, "0.8.0"); err != nil {
		t.Fatalf("start with a clean canary: %v", err)
	}
}

// The marked canary counts even when it is not the tenant that goes first
// (here it is suspended, so the rollout skips it).
func TestRolloutIsRefusedWhileAMarkedCanaryThatIsSkippedWasUnhealthy(t *testing.T) {
	e := newEnv(t, 5)
	e.tenants(t, "t01", "t02")
	e.release(t, "0.8.0", "")
	if err := e.tx(t, func(tx pgx.Tx) error { return e.upd.SetCanaryTx(context.Background(), tx, "t02") }); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(context.Background(), `UPDATE tenants SET status = 'suspended', status_before_suspend = 'active' WHERE id = 't02'`); err != nil {
		t.Fatal(err)
	}
	e.markUnhealthy(t, "t02", 10*time.Minute)
	if err := e.start(t, "0.8.0"); !errors.Is(err, updater.ErrCanaryUnhealthy) {
		t.Fatalf("start = %v, want ErrCanaryUnhealthy", err)
	}
}

func TestRolloutWithoutAMarkedCanaryChecksTheTenantItPicked(t *testing.T) {
	e := newEnv(t, 5)
	e.tenants(t, "t01", "t02")
	e.release(t, "0.8.0", "")
	e.markUnhealthy(t, "t01", 10*time.Minute) // the oldest tenant becomes the canary
	if err := e.start(t, "0.8.0"); !errors.Is(err, updater.ErrCanaryUnhealthy) {
		t.Fatalf("start = %v, want ErrCanaryUnhealthy", err)
	}
}

func TestRolloutDoesNotCompleteWhileTheCanaryWasUnhealthyInTheLastHour(t *testing.T) {
	e := newEnv(t, 5)
	e.tenants(t, "t01")
	e.release(t, "0.8.0", "")
	if err := e.start(t, "0.8.0"); err != nil {
		t.Fatal(err)
	}
	e.step(t, 1)
	e.healthy("t01")
	e.step(t, 1)
	if got := e.rowState(t, "0.8.0", "t01"); got != updater.RowDone {
		t.Fatalf("canary row = %q", got)
	}
	e.markUnhealthy(t, "t01", 5*time.Minute)
	e.step(t, 3)
	if r := e.release1(t, "0.8.0"); r.State != updater.StateRolling || r.IsTarget {
		t.Fatalf("release = %+v: it completed with a canary that was unhealthy five minutes ago", r)
	}
	e.advance(time.Hour)
	e.step(t, 1)
	if r := e.release1(t, "0.8.0"); r.State != updater.StateDone || !r.IsTarget {
		t.Fatalf("release = %+v, want done and the target once the canary had been clean for an hour", r)
	}
}

// The control plane restarts bots too (a rotated token, a retry, an admin
// restart) and Nomad counts those in the restart count like crashes. That is
// not the update's fault: the soak starts over instead of halting.
func TestARestartTheControlPlaneAskedForDoesNotFailTheSoak(t *testing.T) {
	e := withSoak(newEnv(t, 1))
	e.tenants(t, "t01")
	e.release(t, "0.8.0", "")
	if err := e.start(t, "0.8.0"); err != nil {
		t.Fatal(err)
	}
	e.step(t, 1)
	e.healthy("t01")
	e.step(t, 1)
	e.advance(20 * time.Minute)
	if err := e.prov.Restart(context.Background(), "t01"); err != nil { // e.g. a token rotated
		t.Fatal(err)
	}
	e.prov.Wait()
	e.step(t, 1)
	if r := e.release1(t, "0.8.0"); r.State != updater.StateRolling {
		t.Fatalf("release = %+v: a restart the control plane asked for halted the rollout", r)
	}
	if r := e.row(t, "0.8.0", "t01"); r.State != updater.RowUpdating || r.SoakStartedAt != nil {
		t.Fatalf("row = %+v, want updating with the soak started over", r)
	}
	if e.instance(t, "t01").UnhealthyAt != nil {
		t.Error("the canary was marked unhealthy for a restart the control plane asked for")
	}
	e.step(t, 1) // new baseline
	e.advance(59 * time.Minute)
	e.step(t, 1)
	if got := e.rowState(t, "0.8.0", "t01"); got != updater.RowUpdating {
		t.Fatalf("after 59 minutes of the new soak: %q", got)
	}
	e.advance(2 * time.Minute)
	e.step(t, 1)
	if got := e.rowState(t, "0.8.0", "t01"); got != updater.RowDone {
		t.Fatalf("after the new soak: %q, want done", got)
	}
}

// A canary that fails for any reason, not only in the soak, holds the next
// rollout back; a failure further down the line does not.
func TestEveryCanaryHaltIsRecordedAsUnhealthy(t *testing.T) {
	e := newEnv(t, 1)
	e.tenants(t, "t01", "t02")
	e.release(t, "0.8.0", "")
	if err := e.start(t, "0.8.0"); err != nil {
		t.Fatal(err)
	}
	e.step(t, 1)
	e.fake.SetPhase(job("t01"), nomadfake.Failed) // a failed deployment, no soak involved
	e.step(t, 1)
	if r := e.release1(t, "0.8.0"); r.State != updater.StateHalted {
		t.Fatalf("release = %+v", r)
	}
	if e.instance(t, "t01").UnhealthyAt == nil {
		t.Error("a canary whose deployment failed is not recorded as unhealthy")
	}

	e = newEnv(t, 1)
	e.tenants(t, "t01", "t02")
	e.release(t, "0.8.0", "")
	if err := e.start(t, "0.8.0"); err != nil {
		t.Fatal(err)
	}
	e.step(t, 1)
	e.healthy("t01")
	e.step(t, 2) // canary done, batch 1 begins
	e.fake.SetPhase(job("t02"), nomadfake.Failed)
	e.step(t, 1)
	if r := e.release1(t, "0.8.0"); r.State != updater.StateHalted || r.HaltedTenant != "t02" {
		t.Fatalf("release = %+v", r)
	}
	if e.instance(t, "t02").UnhealthyAt != nil {
		t.Error("a failure in a later batch was recorded as a canary failure")
	}
}

func (e *env) override(t *testing.T, version string) {
	t.Helper()
	if err := e.tx(t, func(tx pgx.Tx) error { return e.upd.OverrideCanaryTx(context.Background(), tx, version) }); err != nil {
		t.Fatal(err)
	}
}

func TestTheOperatorCanOverrideTheCanaryGateAtStart(t *testing.T) {
	e := newEnv(t, 5)
	e.tenants(t, "t01", "t02")
	e.release(t, "0.8.0", "")
	if err := e.tx(t, func(tx pgx.Tx) error { return e.upd.SetCanaryTx(context.Background(), tx, "t01") }); err != nil {
		t.Fatal(err)
	}
	e.markUnhealthy(t, "t01", 10*time.Minute)
	if err := e.start(t, "0.8.0"); !errors.Is(err, updater.ErrCanaryUnhealthy) {
		t.Fatalf("start = %v", err)
	}
	rel := e.release1(t, "0.8.0")
	hold, err := e.upd.CanaryHold(context.Background(), rel)
	if err != nil || hold == nil || hold.Tenant != "t01" || !hold.Until.Equal(e.clock().Add(50*time.Minute)) {
		t.Fatalf("hold = %+v, %v", hold, err)
	}
	e.override(t, "0.8.0")
	if h, _ := e.upd.CanaryHold(context.Background(), e.release1(t, "0.8.0")); h != nil {
		t.Errorf("an overridden gate still holds: %+v", h)
	}
	if err := e.start(t, "0.8.0"); err != nil {
		t.Fatalf("start after the override: %v", err)
	}
	if err := e.tx(t, func(tx pgx.Tx) error { return e.upd.OverrideCanaryTx(context.Background(), tx, "9.9.9") }); !errors.Is(err, updater.ErrNotFound) {
		t.Errorf("override of an unknown release = %v", err)
	}
}

func TestTheOperatorCanOverrideAHeldCompletion(t *testing.T) {
	e := newEnv(t, 5)
	e.tenants(t, "t01")
	e.release(t, "0.8.0", "")
	if err := e.start(t, "0.8.0"); err != nil {
		t.Fatal(err)
	}
	e.step(t, 1)
	e.healthy("t01")
	e.step(t, 1)
	e.markUnhealthy(t, "t01", 5*time.Minute)
	e.step(t, 3)
	if r := e.release1(t, "0.8.0"); r.State != updater.StateRolling {
		t.Fatalf("release = %+v, want held", r)
	}
	e.override(t, "0.8.0")
	e.step(t, 1)
	if r := e.release1(t, "0.8.0"); r.State != updater.StateDone || !r.IsTarget {
		t.Fatalf("release after the override = %+v", r)
	}
	if err := e.tx(t, func(tx pgx.Tx) error { return e.upd.OverrideCanaryTx(context.Background(), tx, "0.8.0") }); !errors.Is(err, updater.ErrState) {
		t.Errorf("override of a done release = %v, want ErrState", err)
	}
}

// A release held back by the canary gate says so in the log, and not on
// every pass.
func TestAHeldReleaseIsLoggedOnceInAWhile(t *testing.T) {
	var logs bytes.Buffer
	e := newEnv(t, 5)
	e.opt.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	e.upd = e.newUpdater()
	e.tenants(t, "t01")
	e.release(t, "0.8.0", "")
	if err := e.start(t, "0.8.0"); err != nil {
		t.Fatal(err)
	}
	e.step(t, 1)
	e.healthy("t01")
	e.step(t, 1)
	e.markUnhealthy(t, "t01", 5*time.Minute)
	e.step(t, 5)
	if n := strings.Count(logs.String(), "held back"); n != 1 {
		t.Fatalf("%d log lines for a held release over five passes, want 1:\n%s", n, logs.String())
	}
	e.advance(11 * time.Minute)
	e.step(t, 2)
	if n := strings.Count(logs.String(), "held back"); n != 2 {
		t.Errorf("%d log lines after eleven minutes, want 2", n)
	}
}
