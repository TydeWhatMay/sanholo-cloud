package server

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lhaig/sanholo-cloud/internal/nomad/nomadfake"
	"github.com/lhaig/sanholo-cloud/internal/secrets/qmfake"
)

const purgeURL = "/qm/v1/secret/purge/"

func (p *provEnv) purges() int { return p.qm.Count("DELETE", purgeURL) }

// disconnected: a connected customer who has disconnected while qm refuses
// the purge, so a secret deletion is pending (attempts 1).
func (p *provEnv) disconnectedWithPendingPurge(t *testing.T) (c *client, tenant string) {
	t.Helper()
	c, tenant, _ = p.connected(t)
	p.qm.SetDeleteAllowed(false)
	if r := c.post("/account/game/disconnect", nil); r.status != 200 {
		t.Fatalf("disconnect: %d %s", r.status, r.body)
	}
	return c, tenant
}

// A disconnect while a start is being followed used to leave the follower
// polling for an instance that was gone, so the tenant stayed "in flight" and
// the next connect started nothing.
func TestDisconnectWhileAStartIsSupervisedLeavesTheTenantStartable(t *testing.T) {
	p := newProvEnv(t)
	c, tenant := p.user(t, "ann@example.org")
	job := "sanholo-tenant-" + tenant
	connectGame(t, c, tokenA) // the bot stays pending: the follower is polling
	eventually(t, "the job", func() bool { e, _ := p.fake.Has(job); return e })
	if r := c.post("/account/game/disconnect", nil); r.status != 200 {
		t.Fatalf("disconnect: %d %s", r.status, r.body)
	}
	if exists, _ := p.fake.Has(job); exists {
		t.Fatal("setup: the job survived the disconnect")
	}
	connectGame(t, c, tokenA)
	eventually(t, "a new job after the second connect", func() bool { e, _ := p.fake.Has(job); return e })
	eventually(t, "a new instance record", func() bool { return p.instance(t, tenant) != nil })
	p.fake.SetPhase(job, nomadfake.Healthy)
	p.prov.Wait()
}

// The tenant's lock is held from the stop to the commit of the deleted rows:
// a start that passed its checks earlier cannot register the job afterwards,
// nor recreate the instance row.
func TestDisconnectWaitsForAStartInFlight(t *testing.T) {
	p := newProvEnv(t)
	c, tenant := p.user(t, "ann@example.org")
	job := "sanholo-tenant-" + tenant
	reached, release := make(chan struct{}, 1), make(chan struct{})
	var once, releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	p.fake.Override(func(r nomadfake.Request) (int, string, bool) {
		if r.Method == "POST" && r.Path == "/v1/jobs" {
			first := false
			once.Do(func() { first = true })
			if first {
				reached <- struct{}{}
				<-release
			}
		}
		return 0, "", false
	})
	connectGame(t, c, tokenA)
	<-reached
	var accountID string
	if err := p.pool.QueryRow(context.Background(), `SELECT id::text FROM accounts WHERE tenant_id = $1`, tenant).Scan(&accountID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := p.conn.Disconnect(context.Background(), accountID, tenant); done <- err }()
	time.Sleep(250 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("Disconnect finished (%v) while a start was in flight", err)
	default:
	}
	if n := p.fake.Count("DELETE", "/v1/job/"); n != 0 {
		t.Errorf("%d stops reached Nomad while the start was in flight", n)
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	p.prov.Wait()
	if exists, _ := p.fake.Has(job); exists {
		t.Error("the job exists after the disconnect: the start registered it after the stop")
	}
	if p.instance(t, tenant) != nil {
		t.Error("the instance record was recreated after the disconnect")
	}
}

// A failing deletion backs off: a minute after the first failure, doubling,
// never more than six hours.
func TestPendingDeletionsBackOff(t *testing.T) {
	p := newProvEnv(t)
	_, _ = p.disconnectedWithPendingPurge(t)
	ctx := context.Background()
	if got := p.purges(); got != 1 {
		t.Fatalf("%d purge calls after the disconnect, want 1", got)
	}
	steps := []struct {
		advance time.Duration
		want    int
	}{
		{0, 1}, {30 * time.Second, 1}, {31 * time.Second, 2}, // due one minute after the first failure
		{time.Minute, 2}, {time.Minute + time.Second, 3}, // then two minutes
		{3 * time.Minute, 3}, {time.Minute + time.Second, 4}, // then four
	}
	for i, st := range steps {
		p.clock.advance(st.advance)
		p.conn.RetryCleanups(ctx)
		if got := p.purges(); got != st.want {
			t.Fatalf("step %d: %d purge calls, want %d", i, got, st.want)
		}
	}
}

func TestPendingDeletionBackoffIsCappedAtSixHours(t *testing.T) {
	p := newProvEnv(t)
	_, tenant := p.disconnectedWithPendingPurge(t)
	ctx := context.Background()
	set := func(attempts int, ago time.Duration) {
		if _, err := p.pool.Exec(ctx, `UPDATE pending_cleanups SET attempts = $2, last_attempt_at = $3 WHERE tenant_key = $1`, tenant, attempts, p.clock.now().Add(-ago)); err != nil {
			t.Fatal(err)
		}
	}
	before := p.purges()
	set(40, 5*time.Hour+50*time.Minute)
	p.conn.RetryCleanups(ctx)
	if p.purges() != before {
		t.Error("retried before the six-hour cap")
	}
	set(40, 6*time.Hour+time.Minute)
	p.conn.RetryCleanups(ctx)
	if p.purges() != before+1 {
		t.Error("not retried after six hours, whatever the attempt count")
	}
}

// A pile of secret purges that qm keeps refusing must not starve the
// volume deletions.
func TestStuckPurgesDoNotStarveVolumeDeletions(t *testing.T) {
	p := newProvEnv(t)
	c, tenant, _ := p.connected(t)
	ctx := context.Background()
	p.fake.Override(func(r nomadfake.Request) (int, string, bool) {
		return 503, "", r.Method == "DELETE" && strings.HasPrefix(r.Path, "/v1/volume/host/")
	})
	if r := c.post("/account/game/disconnect", nil); r.status != 200 {
		t.Fatalf("disconnect: %d", r.status)
	}
	p.fake.Override(nil)
	p.qm.SetDeleteAllowed(false)
	for i := range 250 {
		if _, err := p.pool.Exec(ctx, `INSERT INTO pending_cleanups (tenant_key, kind, requested_at) VALUES ($1, 'secret', '2020-01-01')`, fmt.Sprintf("stuck-%03d", i)); err != nil {
			t.Fatal(err)
		}
	}
	p.clock.advance(2 * time.Minute)
	p.conn.RetryCleanups(ctx)
	if len(p.fake.Volumes()) != 0 || rows(t, p.pool, `SELECT count(*) FROM pending_cleanups WHERE tenant_key = $1 AND kind = 'volume'`, tenant) != 0 {
		t.Errorf("the volume deletion waited behind the stuck purges: volumes %v", p.fake.Volumes())
	}
}

// A deletion that keeps failing is logged at error level once at each
// threshold, so an operator can see it.
func TestAStuckDeletionIsLoggedAtTheThresholds(t *testing.T) {
	p := newProvEnv(t)
	_, tenant := p.disconnectedWithPendingPurge(t)
	ctx := context.Background()
	stuck := func() int { return strings.Count(p.logs.String(), "a deletion is stuck") }
	for _, attempts := range []int{3, 4, 5, 8} {
		if _, err := p.pool.Exec(ctx, `UPDATE pending_cleanups SET attempts = $2, last_attempt_at = $3 WHERE tenant_key = $1`, tenant, attempts, p.clock.now().Add(-7*time.Hour)); err != nil {
			t.Fatal(err)
		}
		before := stuck()
		p.conn.RetryCleanups(ctx) // attempts becomes attempts+1
		want := before
		if attempts+1 == 5 {
			want++
		}
		if stuck() != want {
			t.Errorf("after attempt %d: %d stuck lines, want %d", attempts+1, stuck(), want)
		}
	}
}

// A purge that is already on its way when the customer connects again must
// not delete the new token: the connect waits for it.
func TestALatePurgeCannotDeleteANewConnectionsToken(t *testing.T) {
	p := newProvEnv(t)
	c, tenant := p.disconnectedWithPendingPurge(t)
	if v := validate(c, tokenA); v.status != 200 {
		t.Fatalf("validate: %d %s", v.status, v.body)
	}
	p.qm.SetDeleteAllowed(true)
	reached, release := make(chan struct{}, 1), make(chan struct{})
	var once, releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	p.qm.SetOverride(func(r qmfake.Request) (int, string, bool) {
		if r.Method == "DELETE" && strings.Contains(r.URL, purgeURL) {
			first := false
			once.Do(func() { first = true })
			if first {
				reached <- struct{}{}
				<-release
			}
		}
		return 0, "", false
	})
	p.clock.advance(2 * time.Minute)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); p.conn.RetryCleanups(context.Background()) }()
	<-reached // the purge is on its way
	confirmed := make(chan result, 1)
	go func() { confirmed <- c.post("/account/game/confirm", nil) }()
	time.Sleep(300 * time.Millisecond)
	select {
	case <-confirmed:
		t.Fatal("the connect went through while a purge for the tenant was in flight")
	default:
	}
	releaseOnce.Do(func() { close(release) })
	wg.Wait()
	if r := <-confirmed; r.status != 200 {
		t.Fatalf("confirm: %d %s", r.status, r.body)
	}
	if _, ok := p.qm.Stored(qmPath(tenant)); !ok {
		t.Error("the new connection's token was deleted by the late purge")
	}
	eventually(t, "the job", func() bool { e, _ := p.fake.Has("sanholo-tenant-" + tenant); return e })
	p.fake.SetPhase("sanholo-tenant-"+tenant, nomadfake.Healthy)
	p.prov.Wait()
}

// The lock order is the tenant's provisioning lock, then the tenants row;
// connect, disconnect and the cleanup all take the row before anything else
// in the database. Run together they finish.
func TestCleanupConnectAndDisconnectDoNotDeadlock(t *testing.T) {
	p := newProvEnv(t)
	c, tenant := p.disconnectedWithPendingPurge(t)
	p.qm.SetDeleteAllowed(true)
	var accountID string
	if err := p.pool.QueryRow(context.Background(), `SELECT id::text FROM accounts WHERE tenant_id = $1`, tenant).Scan(&accountID); err != nil {
		t.Fatal(err)
	}
	if v := validate(c, tokenA); v.status != 200 {
		t.Fatalf("validate: %d", v.status)
	}
	p.clock.advance(time.Hour)
	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for range 3 {
			wg.Add(1)
			go func() { defer wg.Done(); p.conn.RetryCleanups(context.Background()) }()
		}
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = p.conn.Disconnect(context.Background(), accountID, tenant) }()
		go func() { defer wg.Done(); c.post("/account/game/confirm", nil) }()
		wg.Wait()
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("deadlock between the cleanup, a connect and a disconnect")
	}
	p.fake.Override(nil)
	if exists, _ := p.fake.Has("sanholo-tenant-" + tenant); exists {
		p.fake.SetPhase("sanholo-tenant-"+tenant, nomadfake.Healthy)
	}
	p.prov.Wait()
}

// A different game account connected soon after a disconnect must not
// inherit the old account's data volume, whose deletion Nomad refused.
func TestAnotherAccountDoesNotInheritAPendingVolume(t *testing.T) {
	p := newProvEnv(t)
	c, tenant, job := p.connected(t)
	p.fake.Override(func(r nomadfake.Request) (int, string, bool) {
		return 503, "", r.Method == "DELETE" && strings.HasPrefix(r.Path, "/v1/volume/host/")
	})
	if r := c.post("/account/game/disconnect", nil); r.status != 200 {
		t.Fatalf("disconnect: %d", r.status)
	}
	p.fake.Override(nil)
	if len(p.fake.Volumes()) != 1 {
		t.Fatalf("setup: volumes %v", p.fake.Volumes())
	}
	connectGame(t, c, tokenB) // another game account
	eventually(t, "the new job", func() bool { e, _ := p.fake.Has(job); return e })
	p.fake.SetPhase(job, nomadfake.Healthy)
	p.prov.Wait()
	var order []string
	for _, rq := range p.fake.Requests() {
		switch {
		case rq.Method == "DELETE" && strings.HasPrefix(rq.Path, "/v1/volume/host/"):
			order = append(order, "delete")
		case rq.Method == "PUT" && rq.Path == "/v1/volume/host/create":
			order = append(order, "create")
		}
	}
	// create (first connect), delete refused, delete (second connect), create.
	if strings.Join(order, ",") != "create,delete,delete,create" {
		t.Errorf("volume calls = %v: the old volume was not deleted before the new one was made", order)
	}
	if rows(t, p.pool, `SELECT count(*) FROM pending_cleanups WHERE tenant_key = $1 AND kind = 'volume'`, tenant) != 0 {
		t.Error("the volume deletion is still pending")
	}
	if len(p.fake.Volumes()) != 1 {
		t.Errorf("volumes = %v, want only the new one", p.fake.Volumes())
	}
}
