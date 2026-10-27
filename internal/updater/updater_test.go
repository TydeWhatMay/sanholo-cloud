package updater_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lhaig/sanholo-cloud/internal/dbtest"
	"github.com/lhaig/sanholo-cloud/internal/nomad"
	"github.com/lhaig/sanholo-cloud/internal/nomad/nomadfake"
	"github.com/lhaig/sanholo-cloud/internal/provision"
	"github.com/lhaig/sanholo-cloud/internal/updater"
)

const (
	nomadToken = "nomad-acl-SECRET-token-9c41"
	oldImage   = "ghcr.io/tydewhatmay/sanholo:v0.7.1"
	newImage   = "ghcr.io/tydewhatmay/sanholo:v0.8.0"
	digest     = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	timeout    = 20 * time.Minute
)

type env struct {
	pool *pgxpool.Pool
	fake *nomadfake.Fake
	nc   *nomad.Client
	prov *provision.Service
	upd  *updater.Service
	opt  updater.Options

	mu   sync.Mutex
	now  time.Time
	made int
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

// newEnv is a migrated database, a fake Nomad and the real provisioner and
// updater over them, with a clock the test moves.
func newEnv(t *testing.T, batch int) *env {
	t.Helper()
	e := &env{pool: dbtest.Migrated(t), now: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	e.fake = nomadfake.New(t, nomadToken, "sanholo")
	var err error
	e.nc, err = nomad.New(nomad.Config{Addr: e.fake.URL(), Token: nomadToken, Namespace: "sanholo", Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	e.prov, err = provision.New(e.pool, e.nc, provision.Options{
		Settings: nomadfake.Settings(), Version: "0.7.1", Timeout: 10 * time.Minute, Poll: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.prov.SetClock(e.clock)
	t.Cleanup(e.prov.Close)
	e.opt = updater.Options{BatchSize: batch, HealthTimeout: timeout}
	e.upd = e.newUpdater()
	return e
}

// newUpdater is a second control plane over the same database and Nomad.
func (e *env) newUpdater() *updater.Service {
	u := updater.New(e.pool, e.prov, e.opt)
	u.SetClock(e.clock)
	return u
}

func job(id string) string { return "sanholo-tenant-" + id }

// tenants creates running tenants at the configured version, oldest first.
func (e *env) tenants(t *testing.T, ids ...string) {
	t.Helper()
	ctx := context.Background()
	for _, id := range ids {
		e.made++
		if _, err := e.pool.Exec(ctx, `INSERT INTO tenants (id, status, created_at) VALUES ($1, 'active', $2)`,
			id, time.Date(2026, 1, 1, 0, 0, e.made, 0, time.UTC)); err != nil {
			t.Fatal(err)
		}
		if _, err := e.pool.Exec(ctx,
			`INSERT INTO game_accounts (tenant_id, game_account_id, replicant_name, replicant_code) VALUES ($1, $2, 'Alpha', 'AAAA1111')`,
			id, "replicant:"+id); err != nil {
			t.Fatal(err)
		}
		if err := e.prov.Start(ctx, id); err != nil {
			t.Fatal(err)
		}
		e.fake.SetPhase(job(id), nomadfake.Healthy)
		inst, err := e.prov.Refresh(ctx, id)
		if err != nil || inst.State != provision.StateRunning {
			t.Fatalf("tenant %s: %+v, %v", id, inst, err)
		}
	}
}

func (e *env) healthy(ids ...string) {
	for _, id := range ids {
		e.fake.SetPhase(job(id), nomadfake.Healthy)
	}
}

func (e *env) tx(t *testing.T, fn func(tx pgx.Tx) error) error {
	t.Helper()
	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (e *env) release(t *testing.T, version, dig string) {
	t.Helper()
	err := e.tx(t, func(tx pgx.Tx) error {
		_, err := e.upd.AddReleaseTx(context.Background(), tx, version, dig, "", "")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (e *env) start(t *testing.T, version string) error {
	t.Helper()
	return e.tx(t, func(tx pgx.Tx) error {
		_, err := e.upd.StartTx(context.Background(), tx, version)
		return err
	})
}

func (e *env) resume(t *testing.T) error {
	t.Helper()
	return e.tx(t, func(tx pgx.Tx) error {
		_, err := e.upd.ResumeTx(context.Background(), tx)
		return err
	})
}

func (e *env) abandon(t *testing.T) error {
	t.Helper()
	return e.tx(t, func(tx pgx.Tx) error {
		_, err := e.upd.AbandonTx(context.Background(), tx)
		return err
	})
}

func (e *env) step(t *testing.T, n int) {
	t.Helper()
	for range n {
		if err := e.upd.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

// posts is how many times the tenant's job was registered.
func (e *env) posts(id string) int {
	n := 0
	for _, r := range e.fake.Requests() {
		if r.Method == "POST" && r.Path == "/v1/jobs" && strings.Contains(r.Body, `"ID":"`+job(id)+`"`) {
			n++
		}
	}
	return n
}

func (e *env) row(t *testing.T, version, id string) updater.Row {
	t.Helper()
	rows, err := e.upd.Rollout(context.Background(), version)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.TenantID == id {
			return r
		}
	}
	t.Fatalf("release %s has no row for %s (rows %+v)", version, id, rows)
	return updater.Row{}
}

func (e *env) rowState(t *testing.T, version, id string) string { return e.row(t, version, id).State }

func (e *env) release1(t *testing.T, version string) updater.Release {
	t.Helper()
	list, err := e.upd.Releases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range list {
		if r.Version == version {
			return r
		}
	}
	t.Fatalf("no release %s", version)
	return updater.Release{}
}

func (e *env) instance(t *testing.T, id string) *provision.Instance {
	t.Helper()
	inst, err := e.prov.Get(context.Background(), id)
	if err != nil || inst == nil {
		t.Fatalf("instance %s: %v, %v", id, inst, err)
	}
	return inst
}

func (e *env) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// untouched asserts the tenant's job was registered once (at the start) and
// still runs the old image.
func (e *env) untouched(t *testing.T, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if n := e.posts(id); n != 1 {
			t.Errorf("tenant %s was registered %d times, want only its original registration", id, n)
		}
		if got := e.fake.Image(job(id)); got != oldImage {
			t.Errorf("tenant %s runs %q", id, got)
		}
		if v := e.instance(t, id).ImageVersion; v != "0.7.1" {
			t.Errorf("tenant %s instance version %q", id, v)
		}
	}
}

func ids(prefix string, from, to int) []string {
	var out []string
	for i := from; i <= to; i++ {
		out = append(out, fmt.Sprintf("%s%02d", prefix, i))
	}
	return out
}

// --- releases --------------------------------------------------------------

func TestAddReleaseValidation(t *testing.T) {
	e := newEnv(t, 5)
	long := strings.Repeat("n", 2001)
	cases := []struct {
		name                        string
		version, digest, notes, url string
		want                        error
	}{
		{"plain", "0.8.0", "", "", "", nil},
		{"leading v", "v0.8.1", "", "", "", nil},
		{"pinned", "0.8.2", digest, "Fixes a thing.\nSecond line.", "https://example.org/r/0.8.2", nil},
		{"prerelease", "0.9.0-rc.1", "", "", "", nil},
		{"duplicate", "0.8.0", "", "", "", updater.ErrExists},
		{"duplicate with v", "v0.8.0", "", "", "", updater.ErrExists},
		{"not semver", "latest", "", "", "", updater.ErrInvalid},
		{"two parts", "0.8", "", "", "", updater.ErrInvalid},
		{"short digest", "1.0.0", "sha256:abc", "", "", updater.ErrInvalid},
		{"wrong algorithm", "1.0.0", "md5:" + strings.Repeat("a", 64), "", "", updater.ErrInvalid},
		{"upper-case digest", "1.0.0", strings.ToUpper(digest), "", "", updater.ErrInvalid},
		{"http link", "1.0.0", "", "", "http://example.org/x", updater.ErrInvalid},
		{"javascript link", "1.0.0", "", "", "javascript:alert(1)", updater.ErrInvalid},
		{"link with a space", "1.0.0", "", "", "https://example.org/a b", updater.ErrInvalid},
		{"notes too long", "1.0.0", "", long, "", updater.ErrInvalid},
		{"control character in notes", "1.0.0", "", "bad\x00note", "", updater.ErrInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := e.tx(t, func(tx pgx.Tx) error {
				_, err := e.upd.AddReleaseTx(context.Background(), tx, tc.version, tc.digest, tc.notes, tc.url)
				return err
			})
			if !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
	if n := e.count(t, `SELECT count(*) FROM releases`); n != 4 {
		t.Errorf("%d releases stored, want only the four accepted", n)
	}
	r := e.release1(t, "0.8.2")
	if r.Digest != digest || r.State != updater.StateAvailable || r.IsTarget {
		t.Errorf("release = %+v", r)
	}
	if r := e.release1(t, "0.8.0"); r.Digest != "" {
		t.Errorf("an unpinned release has digest %q", r.Digest)
	}
}

// --- canary, batches, health waits ----------------------------------------

func TestCanaryGoesFirstAlone(t *testing.T) {
	e := newEnv(t, 5)
	e.tenants(t, "t01", "t02", "t03", "t04")
	e.release(t, "0.8.0", "")
	if err := e.start(t, "0.8.0"); err != nil {
		t.Fatal(err)
	}
	// No canary marked: the oldest tenant goes first, alone in batch 0.
	if r := e.row(t, "0.8.0", "t01"); r.Batch != 0 {
		t.Errorf("the first tenant by creation is in batch %d, want the canary batch 0", r.Batch)
	}
	e.step(t, 1)
	if e.posts("t01") != 2 || e.fake.Image(job("t01")) != newImage {
		t.Fatal("the canary was not moved")
	}
	e.untouched(t, "t02", "t03", "t04")
	// While the canary is not healthy nothing else moves, however often the
	// updater looks.
	e.step(t, 3)
	e.untouched(t, "t02", "t03", "t04")
	if e.posts("t01") != 2 {
		t.Errorf("the canary was registered %d times", e.posts("t01"))
	}
	e.healthy("t01")
	e.step(t, 1)
	if s := e.rowState(t, "0.8.0", "t01"); s != updater.RowDone {
		t.Fatalf("canary row = %s", s)
	}
	e.untouched(t, "t02", "t03", "t04")
	e.step(t, 1)
	for _, id := range []string{"t02", "t03", "t04"} {
		if e.posts(id) != 2 {
			t.Errorf("after the canary, %s was registered %d times", id, e.posts(id))
		}
	}
}

func TestMarkedCanaryGoesFirst(t *testing.T) {
	e := newEnv(t, 5)
	e.tenants(t, "t01", "t02", "t03")
	if err := e.tx(t, func(tx pgx.Tx) error { return e.upd.SetCanaryTx(context.Background(), tx, "t03") }); err != nil {
		t.Fatal(err)
	}
	e.release(t, "0.8.0", "")
	if err := e.start(t, "0.8.0"); err != nil {
		t.Fatal(err)
	}
	if b := e.row(t, "0.8.0", "t03").Batch; b != 0 {
		t.Fatalf("the marked canary is in batch %d", b)
	}
	e.step(t, 1)
	if e.posts("t03") != 2 {
		t.Error("the marked canary was not moved first")
	}
	e.untouched(t, "t01", "t02")

	// Marking another tenant moves the mark; an unknown tenant is refused and
	// changes nothing; an empty id clears it.
	if err := e.tx(t, func(tx pgx.Tx) error { return e.upd.SetCanaryTx(context.Background(), tx, "t02") }); err != nil {
		t.Fatal(err)
	}
	if n := e.count(t, `SELECT count(*) FROM tenants WHERE is_canary`); n != 1 {
		t.Errorf("%d canaries marked", n)
	}
	err := e.tx(t, func(tx pgx.Tx) error { return e.upd.SetCanaryTx(context.Background(), tx, "nope") })
	if !errors.Is(err, updater.ErrNotFound) {
		t.Errorf("unknown tenant: %v", err)
	}
	if n := e.count(t, `SELECT count(*) FROM tenants WHERE is_canary AND id = 't02'`); n != 1 {
		t.Error("a refused mark changed the canary")
	}
	if err := e.tx(t, func(tx pgx.Tx) error { return e.upd.SetCanaryTx(context.Background(), tx, "") }); err != nil {
		t.Fatal(err)
	}
	if n := e.count(t, `SELECT count(*) FROM tenants WHERE is_canary`); n != 0 {
		t.Error("the mark was not cleared")
	}
}

func TestBatchesOfFiveWaitForHealth(t *testing.T) {
	e := newEnv(t, 5)
	all := ids("t", 1, 12)
	e.tenants(t, all...)
	e.release(t, "0.8.0", "")
	if err := e.start(t, "0.8.0"); err != nil {
		t.Fatal(err)
	}
	// 12 tenants: the canary, 5, 5 and the last one.
	sizes := map[int]int{}
	rows, _ := e.upd.Rollout(context.Background(), "0.8.0")
	for _, r := range rows {
		sizes[r.Batch]++
	}
	if len(sizes) != 4 || sizes[0] != 1 || sizes[1] != 5 || sizes[2] != 5 || sizes[3] != 1 {
		t.Fatalf("batch sizes = %v, want canary 1, then 5, 5, 1", sizes)
	}

	moved := func(ids ...string) {
		t.Helper()
		for _, id := range ids {
			if e.posts(id) != 2 {
				t.Errorf("%s registered %d times, want moved once", id, e.posts(id))
			}
		}
	}
	e.step(t, 1)
	moved("t01")
	e.healthy("t01")
	e.step(t, 2) // the canary is done, then batch 1 starts
	moved("t02", "t03", "t04", "t05", "t06")
	e.untouched(t, ids("t", 7, 12)...)

	// Four of five healthy is not enough for the next batch.
	e.healthy("t02", "t03", "t04", "t05")
	e.step(t, 3)
	e.untouched(t, ids("t", 7, 12)...)
	e.healthy("t06")
	e.step(t, 2) // batch 1 done, batch 2 starts
	moved("t07", "t08", "t09", "t10", "t11")
	e.untouched(t, "t12")

	e.healthy("t07", "t08", "t09", "t10", "t11")
	e.step(t, 2)
	moved("t12")
	if r := e.release1(t, "0.8.0"); r.State != updater.StateRolling || r.IsTarget {
		t.Fatalf("release before the last tenant is healthy = %+v", r)
	}
	e.healthy("t12")
	e.step(t, 2)
	r := e.release1(t, "0.8.0")
	if r.State != updater.StateDone || !r.IsTarget || r.FinishedAt == nil {
		t.Fatalf("release after the rollout = %+v", r)
	}
	for _, id := range all {
		if e.rowState(t, "0.8.0", id) != updater.RowDone || e.instance(t, id).ImageVersion != "0.8.0" {
			t.Errorf("tenant %s: row %s, version %s", id, e.rowState(t, "0.8.0", id), e.instance(t, id).ImageVersion)
		}
	}
	// Done is quiet: more steps register nothing.
	before := e.fake.Count("POST", "/v1/jobs")
	e.step(t, 3)
	if e.fake.Count("POST", "/v1/jobs") != before {
		t.Error("a finished rollout registered jobs")
	}
}

func TestBatchSizeIsConfigurable(t *testing.T) {
	e := newEnv(t, 2)
	e.tenants(t, ids("t", 1, 6)...)
	e.release(t, "0.8.0", "")
	if err := e.start(t, "0.8.0"); err != nil {
		t.Fatal(err)
	}
	sizes := map[int]int{}
	rows, _ := e.upd.Rollout(context.Background(), "0.8.0")
	for _, r := range rows {
		sizes[r.Batch]++
	}
	if sizes[0] != 1 || sizes[1] != 2 || sizes[2] != 2 || sizes[3] != 1 {
		t.Errorf("batch sizes with a batch of 2 = %v", sizes)
	}
	if r := e.release1(t, "0.8.0"); r.BatchSize != 2 {
		t.Errorf("stored batch size = %d", r.BatchSize)
	}
}

// --- failure: halt and revert ----------------------------------------------

func TestFailureHaltsRevertsAndLeavesTheRestAlone(t *testing.T) {
	e := newEnv(t, 3)
	e.tenants(t, ids("t", 1, 8)...)
	e.release(t, "0.8.0", "")
	if err := e.start(t, "0.8.0"); err != nil {
		t.Fatal(err)
	}
	e.step(t, 1)
	e.healthy("t01")
	e.step(t, 2) // canary done, batch 1 (t02 t03 t04) registered

	// t02 is fine, t03's deployment fails, t04 is still starting.
	e.healthy("t02")
	e.fake.SetPhase(job("t03"), nomadfake.Failed)
	e.step(t, 1)

	r := e.release1(t, "0.8.0")
	if r.State != updater.StateHalted || r.HaltedTenant != "t03" || r.HaltReason != provision.ReasonDeployFailed {
		t.Fatalf("release = %+v", r)
	}
	// The failed tenant and the sibling that was still updating are back on
	// the old image, in Nomad and in the instance record.
	for _, id := range []string{"t03", "t04"} {
		if got := e.fake.Image(job(id)); got != oldImage {
			t.Errorf("%s runs %q after the revert", id, got)
		}
		if v := e.instance(t, id).ImageVersion; v != "0.7.1" {
			t.Errorf("%s instance version = %q", id, v)
		}
		if s := e.rowState(t, "0.8.0", id); s != updater.RowReverting {
			t.Errorf("%s row = %s", id, s)
		}
	}
	if got := e.row(t, "0.8.0", "t03").Reason; got != provision.ReasonDeployFailed {
		t.Errorf("t03 reason = %q", got)
	}
	if got := e.row(t, "0.8.0", "t04").Reason; got != updater.ReasonSibling {
		t.Errorf("t04 reason = %q", got)
	}
	// The healthy ones stay on the new version; the rest were never touched.
	if e.fake.Image(job("t01")) != newImage || e.fake.Image(job("t02")) != newImage {
		t.Error("a tenant that was healthy on the new version was reverted")
	}
	e.untouched(t, ids("t", 5, 8)...)
	for _, id := range ids("t", 5, 8) {
		if s := e.rowState(t, "0.8.0", id); s != updater.RowPending {
			t.Errorf("%s row = %s", id, s)
		}
	}

	// A halted rollout moves nothing, however often the updater looks; the
	// reverts are confirmed once the old version is healthy.
	e.step(t, 3)
	e.untouched(t, ids("t", 5, 8)...)
	if s := e.rowState(t, "0.8.0", "t03"); s != updater.RowReverting {
		t.Errorf("t03 is %s before the old version is healthy", s)
	}
	e.healthy("t03", "t04")
	e.step(t, 1)
	for _, id := range []string{"t03", "t04"} {
		if s := e.rowState(t, "0.8.0", id); s != updater.RowReverted {
			t.Errorf("%s row = %s, want reverted", id, s)
		}
	}
	e.untouched(t, ids("t", 5, 8)...)
	if e.release1(t, "0.8.0").IsTarget || e.count(t, `SELECT count(*) FROM releases WHERE is_target`) != 0 {
		t.Error("a halted release became the fleet target")
	}
}

func TestNomadAutoRevertIsDetectedAndVerified(t *testing.T) {
	e := newEnv(t, 5)
	e.tenants(t, "t01", "t02", "t03")
	e.fake.FailImage("v0.8.0", true) // the deployment fails and Nomad reverts it itself
	e.release(t, "0.8.0", "")
	if err := e.start(t, "0.8.0"); err != nil {
		t.Fatal(err)
	}
	e.step(t, 2) // the canary is registered, Nomad has already reverted it
	r := e.release1(t, "0.8.0")
	if r.State != updater.StateHalted || r.HaltedTenant != "t01" || r.HaltReason != updater.ReasonNomadReverted {
		t.Fatalf("release = %+v", r)
	}
	if got := e.fake.Image(job("t01")); got != oldImage {
		t.Errorf("t01 runs %q", got)
	}
	// Nomad already holds the old spec: the explicit revert registers
	// nothing more.
	if e.posts("t01") != 2 {
		t.Errorf("t01 registered %d times, want the update only (Nomad's revert is the second version)", e.posts("t01"))
	}
	// The revert counts only once the old version is seen healthy.
	e.step(t, 1)
	if s := e.rowState(t, "0.8.0", "t01"); s != updater.RowReverted {
		t.Errorf("t01 row = %s", s)
	}
	if v := e.instance(t, "t01").ImageVersion; v != "0.7.1" {
		t.Errorf("instance version = %s", v)
	}
	e.untouched(t, "t02", "t03")
}

func TestHaltOnTheCanaryTouchesNobodyElse(t *testing.T) {
	e := newEnv(t, 5)
	e.tenants(t, "t01", "t02", "t03")
	e.fake.FailImage("v0.8.0", false) // fails, and Nomad does not revert
	e.release(t, "0.8.0", "")
	if err := e.start(t, "0.8.0"); err != nil {
		t.Fatal(err)
	}
	e.step(t, 2)
	r := e.release1(t, "0.8.0")
	if r.State != updater.StateHalted || r.HaltedTenant != "t01" || r.HaltReason != provision.ReasonDeployFailed {
		t.Fatalf("release = %+v", r)
	}
	// The updater registered the old spec again itself: update, then revert.
	if e.posts("t01") != 3 || e.fake.Image(job("t01")) != oldImage {
		t.Errorf("t01: %d registrations, image %q", e.posts("t01"), e.fake.Image(job("t01")))
	}
	e.untouched(t, "t02", "t03")
}

func TestRegistrationRefusedHaltsWithAFixedReason(t *testing.T) {
	e := newEnv(t, 5)
	e.tenants(t, "t01", "t02", "t03")
	e.fake.Override(func(r nomadfake.Request) (int, string, bool) {
		if r.Method == "POST" && strings.Contains(r.Body, `"ID":"`+job("t01")+`"`) && strings.Contains(r.Body, "0.8.0") {
			return 400, "SECRET-NOMAD-DIAGNOSTIC: group bot: volume missing", true
		}
		return 0, "", false
	})
	e.release(t, "0.8.0", "")
	if err := e.start(t, "0.8.0"); err != nil {
		t.Fatal(err)
	}
	e.step(t, 1)
	r := e.release1(t, "0.8.0")
	if r.State != updater.StateHalted || r.HaltedTenant != "t01" || r.HaltReason != provision.ReasonRegister {
		t.Fatalf("release = %+v", r)
	}
	e.untouched(t, "t02", "t03")
	// The refused registration changed nothing in Nomad.
	if e.fake.Version(job("t01")) != 1 || e.fake.Image(job("t01")) != oldImage || e.instance(t, "t01").ImageVersion != "0.7.1" {
		t.Error("a refused registration left t01 changed")
	}
	// Nomad's text is stored nowhere.
	for _, q := range []string{
		`SELECT count(*) FROM releases WHERE halt_reason LIKE '%SECRET%' OR halted_tenant LIKE '%SECRET%'`,
		`SELECT count(*) FROM rollout_tenants WHERE reason LIKE '%SECRET%'`,
		`SELECT count(*) FROM audit_log WHERE details::text LIKE '%SECRET%'`,
		`SELECT count(*) FROM tenant_instances WHERE failure_reason LIKE '%SECRET%'`,
	} {
		if e.count(t, q) != 0 {
			t.Errorf("Nomad's text was stored: %s", q)
		}
	}
}

func TestRefusedRegistrationLeavesPendingSiblingsAlone(t *testing.T) {
	e := newEnv(t, 3)
	e.tenants(t, ids("t", 1, 6)...)
	e.fake.Override(func(r nomadfake.Request) (int, string, bool) {
		if r.Method == "POST" && strings.Contains(r.Body, `"ID":"`+job("t03")+`"`) && strings.Contains(r.Body, "0.8.0") {
			return 400, "", true
		}
		return 0, "", false
	})
	e.release(t, "0.8.0", "")
	if err := e.start(t, "0.8.0"); err != nil {
		t.Fatal(err)
	}
	e.step(t, 1)
	e.healthy("t01")
	e.step(t, 2) // the canary is done; batch 1 (t02 t03 t04): t02 registered, t03 refused
	r := e.release1(t, "0.8.0")
	if r.State != updater.StateHalted || r.HaltedTenant != "t03" {
		t.Fatalf("release = %+v", r)
	}
	// t04 is in the same batch but was not reached: untouched, still pending.
	e.untouched(t, "t04", "t05", "t06")
	if s := e.rowState(t, "0.8.0", "t04"); s != updater.RowPending {
		t.Errorf("t04 = %s", s)
	}
	// t02 was updating when the batch halted, so it goes back too.
	if s := e.rowState(t, "0.8.0", "t02"); s != updater.RowReverting || e.fake.Image(job("t02")) != oldImage {
		t.Errorf("t02 = %s, image %q", s, e.fake.Image(job("t02")))
	}
}

func TestTimeoutHaltsOnlyAfterTheTimeout(t *testing.T) {
	e := newEnv(t, 5)
	e.tenants(t, "t01", "t02")
	e.release(t, "0.8.0", "")
	if err := e.start(t, "0.8.0"); err != nil {
		t.Fatal(err)
	}
	e.step(t, 1)
	e.advance(timeout - time.Minute)
	e.step(t, 1)
	if s := e.release1(t, "0.8.0").State; s != updater.StateRolling {
		t.Fatalf("halted before the timeout: %s", s)
	}
	e.advance(2 * time.Minute)
	e.step(t, 1)
	r := e.release1(t, "0.8.0")
	if r.State != updater.StateHalted || r.HaltReason != provision.ReasonTimeout || r.HaltedTenant != "t01" {
		t.Fatalf("release = %+v", r)
	}
	e.untouched(t, "t02")
}

func TestUnreadableNomadIsNotAFailureUntilTheTimeout(t *testing.T) {
	e := newEnv(t, 5)
	e.tenants(t, "t01", "t02")
	e.release(t, "0.8.0", "")
	if err := e.start(t, "0.8.0"); err != nil {
		t.Fatal(err)
	}
	e.step(t, 1)
	var down atomic.Bool
	down.Store(true)
	e.fake.Override(func(r nomadfake.Request) (int, string, bool) {
		if down.Load() && r.Method == "GET" {
			return 503, "SECRET-NOMAD-DOWN", true
		}
		return 0, "", false
	})
	e.step(t, 2)
	if s := e.release1(t, "0.8.0").State; s != updater.StateRolling {
		t.Fatalf("a Nomad outage halted the rollout: %s", s)
	}
	down.Store(false)
	e.healthy("t01")
	e.step(t, 1)
	if s := e.rowState(t, "0.8.0", "t01"); s != updater.RowDone {
		t.Errorf("t01 = %s after Nomad came back", s)
	}
}

func TestRevertThatNeverGetsHealthyIsFailedForTheOperator(t *testing.T) {
	e := newEnv(t, 5)
	e.tenants(t, "t01", "t02")
	e.release(t, "0.8.0", "")
	if err := e.start(t, "0.8.0"); err != nil {
		t.Fatal(err)
	}
	e.step(t, 1)
	e.fake.SetPhase(job("t01"), nomadfake.Failed)
	e.step(t, 1)
	if s := e.rowState(t, "0.8.0", "t01"); s != updater.RowReverting {
		t.Fatalf("t01 = %s", s)
	}
	// While a revert is under way the operator can neither resume nor
	// abandon.
	if err := e.resume(t); !errors.Is(err, updater.ErrState) {
		t.Errorf("resume during a revert: %v", err)
	}
	if err := e.abandon(t); !errors.Is(err, updater.ErrState) {
		t.Errorf("abandon during a revert: %v", err)
	}
	e.advance(timeout + time.Minute)
	e.step(t, 1)
	if s := e.rowState(t, "0.8.0", "t01"); s != updater.RowFailed {
		t.Errorf("t01 = %s, want failed (the revert did not become healthy)", s)
	}
	if err := e.abandon(t); err != nil {
		t.Errorf("abandon after the revert gave up: %v", err)
	}
}

func TestRevertWaitsForNomadToBeReachable(t *testing.T) {
	e := newEnv(t, 5)
	e.tenants(t, "t01")
	e.release(t, "0.8.0", "")
	if err := e.start(t, "0.8.0"); err != nil {
		t.Fatal(err)
	}
	e.step(t, 1)
	e.fake.SetPhase(job("t01"), nomadfake.Failed)
	var down atomic.Bool
	e.fake.Override(func(r nomadfake.Request) (int, string, bool) {
		if down.Load() && r.Method == "POST" {
			return 503, "", true
		}
		return 0, "", false
	})
	down.Store(true)
	e.step(t, 1) // the failure is recorded, the revert cannot be registered
	if s := e.rowState(t, "0.8.0", "t01"); s != updater.RowRevertPending {
		t.Fatalf("t01 = %s", s)
	}
	e.step(t, 2)
	if s := e.rowState(t, "0.8.0", "t01"); s != updater.RowRevertPending {
		t.Fatalf("t01 = %s while Nomad is down", s)
	}
	down.Store(false)
	e.step(t, 1)
	if s := e.rowState(t, "0.8.0", "t01"); s != updater.RowReverting {
		t.Fatalf("t01 = %s after Nomad came back", s)
	}
	if e.fake.Image(job("t01")) != oldImage {
		t.Error("the revert was never registered")
	}
}

// --- halted rollouts need the operator -------------------------------------

func TestResume(t *testing.T) {
	e := newEnv(t, 5)
	e.tenants(t, "t01", "t02", "t03")
	e.release(t, "0.8.0", "")
	if err := e.resume(t); !errors.Is(err, updater.ErrState) {
		t.Errorf("resume with nothing halted: %v", err)
	}
	if err := e.start(t, "0.8.0"); err != nil {
		t.Fatal(err)
	}
	if err := e.resume(t); !errors.Is(err, updater.ErrState) {
		t.Errorf("resume of a rolling rollout: %v", err)
	}
	e.step(t, 1)
	e.fake.SetPhase(job("t01"), nomadfake.Failed)
	e.step(t, 1)
	e.healthy("t01")
	e.step(t, 1)
	// Halted and idle: more steps do nothing.
	posts := e.fake.Count("POST", "/v1/jobs")
	e.step(t, 3)
	if e.fake.Count("POST", "/v1/jobs") != posts {
		t.Fatal("a halted rollout registered jobs")
	}
	if err := e.resume(t); err != nil {
		t.Fatal(err)
	}
	r := e.release1(t, "0.8.0")
	if r.State != updater.StateRolling || r.HaltedTenant != "" || r.HaltReason != "" {
		t.Fatalf("release after resume = %+v", r)
	}
	if s := e.rowState(t, "0.8.0", "t01"); s != updater.RowPending {
		t.Fatalf("t01 = %s, want pending (tried again)", s)
	}
	e.step(t, 1)
	if e.fake.Image(job("t01")) != newImage {
		t.Fatal("the failed tenant was not tried again")
	}
	e.healthy("t01")
	e.step(t, 2)
	e.healthy("t02", "t03")
	e.step(t, 3)
	e.healthy("t02", "t03")
	e.step(t, 3)
	if r := e.release1(t, "0.8.0"); r.State != updater.StateDone || !r.IsTarget {
		t.Fatalf("release = %+v", r)
	}
}

func TestAbandon(t *testing.T) {
	e := newEnv(t, 5)
	e.tenants(t, "t01", "t02")
	e.release(t, "0.8.0", "")
	if err := e.abandon(t); !errors.Is(err, updater.ErrState) {
		t.Errorf("abandon with nothing halted: %v", err)
	}
	if err := e.start(t, "0.8.0"); err != nil {
		t.Fatal(err)
	}
	if err := e.abandon(t); !errors.Is(err, updater.ErrState) {
		t.Errorf("abandon of a rolling rollout: %v", err)
	}
	e.step(t, 1)
	e.fake.SetPhase(job("t01"), nomadfake.Failed)
	e.step(t, 1)
	e.healthy("t01")
	e.step(t, 1)
	if err := e.abandon(t); err != nil {
		t.Fatal(err)
	}
	r := e.release1(t, "0.8.0")
	if r.State != updater.StateAvailable || r.IsTarget || r.HaltedTenant != "" {
		t.Fatalf("release after abandon = %+v", r)
	}
	if e.count(t, `SELECT count(*) FROM rollout_tenants`) != 0 {
		t.Error("the rollout rows were kept")
	}
	e.untouched(t, "t02")
	if e.fake.Image(job("t01")) != oldImage || e.instance(t, "t01").ImageVersion != "0.7.1" {
		t.Error("abandoning left t01 on the new version")
	}
	// The slot is free: the same release can be started again, and another.
	e.release(t, "0.9.0", "")
	if err := e.start(t, "0.9.0"); err != nil {
		t.Errorf("start after abandon: %v", err)
	}
}

// --- idempotent, restart-safe ----------------------------------------------

func TestAlreadyOnTheReleaseIsANoOp(t *testing.T) {
	e := newEnv(t, 5)
	e.tenants(t, "t01", "t02", "t03")
	e.release(t, "0.7.1", "") // what everyone already runs
	if err := e.start(t, "0.7.1"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"t01", "t02", "t03"} {
		if s := e.rowState(t, "0.7.1", id); s != updater.RowDone {
			t.Errorf("%s = %s", id, s)
		}
	}
	e.step(t, 2)
	e.untouched(t, "t01", "t02", "t03")
	if r := e.release1(t, "0.7.1"); r.State != updater.StateDone || !r.IsTarget {
		t.Fatalf("release = %+v", r)
	}
}

func TestRepeatedUpdateRegistersOnce(t *testing.T) {
	e := newEnv(t, 5)
	e.tenants(t, "t01")
	ctx := context.Background()
	for range 3 {
		if err := e.prov.Update(ctx, "t01", "0.8.0", ""); err != nil {
			t.Fatal(err)
		}
	}
	if e.posts("t01") != 2 {
		t.Errorf("registered %d times, want the start and one update", e.posts("t01"))
	}
	if err := e.prov.Update(ctx, "t01", "0.8.0", digest); err != nil {
		t.Fatal(err)
	}
	if e.posts("t01") != 3 {
		t.Errorf("a digest is a new spec, got %d registrations", e.posts("t01"))
	}
}

func TestRestartResumesMidRollout(t *testing.T) {
	e := newEnv(t, 2)
	e.tenants(t, ids("t", 1, 6)...)
	e.release(t, "0.8.0", "")
	if err := e.start(t, "0.8.0"); err != nil {
		t.Fatal(err)
	}
	e.step(t, 1)
	e.healthy("t01")
	e.step(t, 2) // batch 1: t02 t03 registered
	if e.posts("t02") != 2 || e.posts("t03") != 2 {
		t.Fatal("batch 1 not started")
	}
	posts := e.fake.Count("POST", "/v1/jobs")

	// The control plane restarts: a new service over the same database.
	e.upd = e.newUpdater()
	e.step(t, 3)
	if e.fake.Count("POST", "/v1/jobs") != posts {
		t.Error("the restarted updater registered jobs again for tenants already updating")
	}
	e.untouched(t, ids("t", 4, 6)...)

	e.healthy("t02", "t03")
	e.upd = e.newUpdater() // and again
	e.step(t, 2)
	if e.posts("t04") != 2 || e.posts("t05") != 2 {
		t.Error("the restarted updater did not carry on with the next batch")
	}
	e.healthy("t04", "t05")
	e.step(t, 2)
	e.healthy("t06")
	e.step(t, 3)
	if r := e.release1(t, "0.8.0"); r.State != updater.StateDone {
		t.Fatalf("release = %+v", r)
	}
	if e.posts("t01") != 2 {
		t.Errorf("the canary was registered %d times across restarts", e.posts("t01"))
	}
}

func TestRestartMidRevertKeepsReverting(t *testing.T) {
	e := newEnv(t, 5)
	e.tenants(t, "t01", "t02")
	e.release(t, "0.8.0", "")
	if err := e.start(t, "0.8.0"); err != nil {
		t.Fatal(err)
	}
	e.step(t, 1)
	e.fake.SetPhase(job("t01"), nomadfake.Failed)
	e.step(t, 1)
	e.upd = e.newUpdater()
	e.healthy("t01")
	e.step(t, 1)
	if s := e.rowState(t, "0.8.0", "t01"); s != updater.RowReverted {
		t.Errorf("t01 = %s after a restart mid-revert", s)
	}
	if r := e.release1(t, "0.8.0"); r.State != updater.StateHalted {
		t.Errorf("a restart changed the halted release: %+v", r)
	}
}

func TestOnlyOneRolloutAtATime(t *testing.T) {
	e := newEnv(t, 5)
	e.tenants(t, "t01", "t02")
	e.release(t, "0.8.0", "")
	e.release(t, "0.9.0", "")

	var wg sync.WaitGroup
	results := make(chan error, 12)
	for i := range 12 {
		version := []string{"0.8.0", "0.9.0"}[i%2]
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := context.Background()
			tx, err := e.pool.Begin(ctx)
			if err != nil {
				results <- err
				return
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := e.upd.StartTx(ctx, tx, version); err != nil {
				results <- err
				return
			}
			results <- tx.Commit(ctx)
		}()
	}
	wg.Wait()
	close(results)
	ok := 0
	for err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, updater.ErrRolloutActive), errors.Is(err, updater.ErrState):
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d concurrent starts succeeded, want exactly one", ok)
	}
	if n := e.count(t, `SELECT count(*) FROM releases WHERE state IN ('rolling', 'halted')`); n != 1 {
		t.Errorf("%d rollouts active", n)
	}
	if n := e.count(t, `SELECT count(DISTINCT release_version) FROM rollout_tenants`); n != 1 {
		t.Errorf("rollout rows for %d releases", n)
	}
	// A different release is refused while this one is rolling or halted.
	other := "0.9.0"
	if e.release1(t, "0.9.0").State == updater.StateRolling {
		other = "0.8.0"
	}
	if err := e.start(t, other); !errors.Is(err, updater.ErrRolloutActive) {
		t.Errorf("second rollout: %v", err)
	}
}

func TestStepsOfTwoProcessesDoNotOverlap(t *testing.T) {
	e := newEnv(t, 5)
	e.tenants(t, ids("t", 1, 4)...)
	e.release(t, "0.8.0", "")
	if err := e.start(t, "0.8.0"); err != nil {
		t.Fatal(err)
	}
	var inflight, overlap atomic.Int32
	e.fake.Override(func(r nomadfake.Request) (int, string, bool) {
		if inflight.Add(1) > 1 {
			overlap.Add(1)
		}
		time.Sleep(3 * time.Millisecond)
		inflight.Add(-1)
		return 0, "", false
	})
	other := e.newUpdater() // a second control-plane process
	var wg sync.WaitGroup
	for i := range 8 {
		u := e.upd
		if i%2 == 1 {
			u = other
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 4 {
				if err := u.Step(context.Background()); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	if overlap.Load() != 0 {
		t.Errorf("%d Nomad requests overlapped: two steps ran at once", overlap.Load())
	}
	if e.posts("t01") != 2 {
		t.Errorf("canary registered %d times", e.posts("t01"))
	}
}

// --- skipped tenants, targets, digests --------------------------------------

func TestSuspendedAndStoppedTenantsAreSkippedAndCaughtUp(t *testing.T) {
	e := newEnv(t, 5)
	e.tenants(t, "t01", "t02", "t03", "t04")
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `UPDATE tenants SET status = 'suspended', status_before_suspend = 'active' WHERE id = 't02'`); err != nil {
		t.Fatal(err)
	}
	if err := e.prov.Stop(ctx, "t03", false); err != nil {
		t.Fatal(err)
	}
	e.release(t, "0.8.0", "")
	if err := e.start(t, "0.8.0"); err != nil {
		t.Fatal(err)
	}
	if r := e.row(t, "0.8.0", "t02"); r.State != updater.RowSkipped || r.Reason != updater.SkipSuspended {
		t.Errorf("suspended tenant row = %+v", r)
	}
	if r := e.row(t, "0.8.0", "t03"); r.State != updater.RowSkipped || r.Reason != updater.SkipStopped {
		t.Errorf("stopped tenant row = %+v", r)
	}
	e.step(t, 1)
	e.healthy("t01")
	e.step(t, 2)
	e.healthy("t04")
	e.step(t, 3)
	if r := e.release1(t, "0.8.0"); r.State != updater.StateDone || !r.IsTarget {
		t.Fatalf("release = %+v", r)
	}
	e.untouched(t, "t02", "t03") // skipped means not touched

	// A stopped instance that is started again takes the fleet target.
	if err := e.prov.Start(ctx, "t03"); err != nil {
		t.Fatal(err)
	}
	if got := e.fake.Image(job("t03")); got != newImage {
		t.Errorf("restarted tenant runs %q, want the target %q", got, newImage)
	}
	if v := e.instance(t, "t03").ImageVersion; v != "0.8.0" {
		t.Errorf("restarted instance version = %q", v)
	}

	// The suspended one comes back: a plain start leaves it on its version (the
	// updater owns that), and the next rollout of the target moves it.
	if _, err := e.pool.Exec(ctx, `UPDATE tenants SET status = 'active', status_before_suspend = NULL WHERE id = 't02'`); err != nil {
		t.Fatal(err)
	}
	if err := e.prov.Start(ctx, "t02"); err != nil {
		t.Fatal(err)
	}
	if v := e.instance(t, "t02").ImageVersion; v != "0.7.1" {
		t.Errorf("a plain start moved a running instance to %q", v)
	}
	if err := e.start(t, "0.8.0"); err != nil { // catch-up rollout of the target
		t.Fatal(err)
	}
	if s := e.rowState(t, "0.8.0", "t02"); s != updater.RowPending {
		t.Fatalf("t02 = %s in the catch-up", s)
	}
	e.step(t, 1)
	e.healthy("t02")
	e.step(t, 3)
	if v := e.instance(t, "t02").ImageVersion; v != "0.8.0" {
		t.Errorf("t02 version = %q after the catch-up", v)
	}
}

func TestSuspendedDuringTheRolloutIsSkippedNotFailed(t *testing.T) {
	e := newEnv(t, 5)
	e.tenants(t, "t01", "t02")
	e.release(t, "0.8.0", "")
	if err := e.start(t, "0.8.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(context.Background(), `UPDATE tenants SET status = 'suspended', status_before_suspend = 'active' WHERE id = 't01'`); err != nil {
		t.Fatal(err)
	}
	e.step(t, 1)
	if r := e.row(t, "0.8.0", "t01"); r.State != updater.RowSkipped || r.Reason != updater.SkipChanged {
		t.Errorf("t01 row = %+v", r)
	}
	if s := e.release1(t, "0.8.0").State; s != updater.StateRolling {
		t.Errorf("release = %s", s)
	}
	e.untouched(t, "t01")
}

func TestNewInstancesStartAtTheTarget(t *testing.T) {
	e := newEnv(t, 5)
	ctx := context.Background()
	e.tenants(t, "t01", "t02")
	e.release(t, "0.8.0", digest)

	// Before any rollout has finished a new instance starts at the configured
	// version.
	e.tenants(t, "t03")
	if v := e.instance(t, "t03").ImageVersion; v != "0.7.1" {
		t.Fatalf("new instance before a target = %s", v)
	}

	if err := e.start(t, "0.8.0"); err != nil {
		t.Fatal(err)
	}
	e.step(t, 1)
	// During the rollout the target is still the old one.
	e.tenants(t, "t04")
	if v := e.instance(t, "t04").ImageVersion; v != "0.7.1" {
		t.Fatalf("new instance mid-rollout = %s, want the old target", v)
	}
	// The rollout is not done while an instance has no row: the sweep adds
	// t04 and moves it too.
	for range 10 {
		for _, id := range []string{"t01", "t02", "t03", "t04"} {
			e.healthy(id)
		}
		e.step(t, 1)
	}
	if r := e.release1(t, "0.8.0"); r.State != updater.StateDone || !r.IsTarget {
		t.Fatalf("release = %+v", r)
	}
	for _, id := range []string{"t01", "t02", "t03", "t04"} {
		if inst := e.instance(t, id); inst.ImageVersion != "0.8.0" || inst.ImageDigest != digest {
			t.Errorf("%s: %s %s", id, inst.ImageVersion, inst.ImageDigest)
		}
	}
	if e.rowState(t, "0.8.0", "t04") != updater.RowDone {
		t.Error("the tenant started mid-rollout was not carried to the release")
	}

	// A tenant created now starts on the target, pinned to its digest.
	if _, err := e.pool.Exec(ctx, `INSERT INTO tenants (id, status) VALUES ('new1', 'active')`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO game_accounts (tenant_id, game_account_id, replicant_name, replicant_code) VALUES ('new1', 'replicant:new1', 'A', 'B')`); err != nil {
		t.Fatal(err)
	}
	if err := e.prov.Start(ctx, "new1"); err != nil {
		t.Fatal(err)
	}
	if got := e.fake.Image(job("new1")); got != "ghcr.io/tydewhatmay/sanholo@"+digest {
		t.Errorf("new instance image = %q", got)
	}
	if inst := e.instance(t, "new1"); inst.ImageVersion != "0.8.0" || inst.ImageDigest != digest {
		t.Errorf("new instance = %+v", inst)
	}
}

func TestPinningTheVersionTenantsAlreadyRunMovesThem(t *testing.T) {
	e := newEnv(t, 5)
	e.tenants(t, "t01")
	e.release(t, "0.7.1", digest) // same version, now pinned
	if err := e.start(t, "0.7.1"); err != nil {
		t.Fatal(err)
	}
	if s := e.rowState(t, "0.7.1", "t01"); s != updater.RowPending {
		t.Fatalf("t01 = %s: a pin on the running version must still be applied", s)
	}
	e.step(t, 1)
	if got := e.fake.Image(job("t01")); got != "ghcr.io/tydewhatmay/sanholo@"+digest {
		t.Errorf("image = %q", got)
	}
}

func TestDigestIsPinnedAndRevertedToTheTag(t *testing.T) {
	e := newEnv(t, 5)
	e.tenants(t, "t01", "t02")
	e.release(t, "0.8.0", digest)
	if err := e.start(t, "0.8.0"); err != nil {
		t.Fatal(err)
	}
	e.step(t, 1)
	if got := e.fake.Image(job("t01")); got != "ghcr.io/tydewhatmay/sanholo@"+digest {
		t.Fatalf("pinned image = %q", got)
	}
	if inst := e.instance(t, "t01"); inst.ImageVersion != "0.8.0" || inst.ImageDigest != digest {
		t.Errorf("instance = %+v", inst)
	}
	e.fake.SetPhase(job("t01"), nomadfake.Failed)
	e.step(t, 1)
	if got := e.fake.Image(job("t01")); got != oldImage {
		t.Errorf("reverted image = %q, want the old tag", got)
	}
	if inst := e.instance(t, "t01"); inst.ImageVersion != "0.7.1" || inst.ImageDigest != "" {
		t.Errorf("reverted instance = %+v", inst)
	}
}

// --- audit -----------------------------------------------------------------

func TestHaltAndCompleteAreAudited(t *testing.T) {
	e := newEnv(t, 5)
	e.tenants(t, "t01")
	e.release(t, "0.8.0", "")
	if err := e.start(t, "0.8.0"); err != nil {
		t.Fatal(err)
	}
	e.step(t, 1)
	e.fake.SetPhase(job("t01"), nomadfake.Failed)
	e.step(t, 1)
	e.healthy("t01")
	e.step(t, 1)
	if err := e.resume(t); err != nil {
		t.Fatal(err)
	}
	e.step(t, 1)
	e.healthy("t01")
	e.step(t, 2)

	var halt, complete string
	if err := e.pool.QueryRow(context.Background(),
		`SELECT details::text FROM audit_log WHERE action = 'rollout.halt' AND actor = 'updater' AND target = 'release:0.8.0'`).Scan(&halt); err != nil {
		t.Fatalf("no halt audit row: %v", err)
	}
	if !strings.Contains(halt, `"tenant": "t01"`) || !strings.Contains(halt, provision.ReasonDeployFailed) {
		t.Errorf("halt details = %s", halt)
	}
	if err := e.pool.QueryRow(context.Background(),
		`SELECT details::text FROM audit_log WHERE action = 'rollout.complete' AND actor = 'updater'`).Scan(&complete); err != nil {
		t.Fatalf("no complete audit row: %v", err)
	}
}

func TestHaltAndAuditShareATransaction(t *testing.T) {
	e := newEnv(t, 5)
	e.tenants(t, "t01", "t02")
	e.release(t, "0.8.0", "")
	if err := e.start(t, "0.8.0"); err != nil {
		t.Fatal(err)
	}
	e.step(t, 1)
	e.fake.SetPhase(job("t01"), nomadfake.Failed)
	for _, q := range []string{
		`CREATE FUNCTION audit_boom() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'audit refused'; END $$`,
		`CREATE TRIGGER audit_boom BEFORE INSERT ON audit_log FOR EACH ROW EXECUTE FUNCTION audit_boom()`,
	} {
		if _, err := e.pool.Exec(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.upd.Step(context.Background()); err == nil {
		t.Fatal("a halt whose audit row failed reported success")
	}
	if r := e.release1(t, "0.8.0"); r.State != updater.StateRolling || r.HaltedTenant != "" {
		t.Errorf("release halted without its audit row: %+v", r)
	}
	if s := e.rowState(t, "0.8.0", "t01"); s != updater.RowUpdating {
		t.Errorf("t01 = %s", s)
	}
}

func TestFinishAndAuditShareATransaction(t *testing.T) {
	e := newEnv(t, 5)
	e.tenants(t, "t01")
	e.release(t, "0.8.0", "")
	if err := e.start(t, "0.8.0"); err != nil {
		t.Fatal(err)
	}
	e.step(t, 1)
	e.healthy("t01")
	e.step(t, 1)
	for _, q := range []string{
		`CREATE FUNCTION audit_boom() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'audit refused'; END $$`,
		`CREATE TRIGGER audit_boom BEFORE INSERT ON audit_log FOR EACH ROW EXECUTE FUNCTION audit_boom()`,
	} {
		if _, err := e.pool.Exec(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.upd.Step(context.Background()); err == nil {
		t.Fatal("a completion whose audit row failed reported success")
	}
	if r := e.release1(t, "0.8.0"); r.State != updater.StateRolling || r.IsTarget {
		t.Errorf("release completed without its audit row: %+v", r)
	}
}

// --- disabled ---------------------------------------------------------------

func TestStepWithoutNomadDoesNothing(t *testing.T) {
	pool := dbtest.Migrated(t)
	nc, _ := nomad.New(nomad.Config{})
	prov, err := provision.New(pool, nc, provision.Options{})
	if err != nil {
		t.Fatal(err)
	}
	u := updater.New(pool, prov, updater.Options{})
	if err := u.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := updater.New(pool, nil, updater.Options{}).Step(context.Background()); err != nil {
		t.Fatal(err)
	}
}
