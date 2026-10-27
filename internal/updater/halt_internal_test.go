package updater

import (
	"context"
	"testing"

	"github.com/lhaig/sanholo-cloud/internal/dbtest"
)

// A canary is marked unhealthy only by a halt that took effect: one that finds
// the release no longer rolling (abandoned meanwhile) changes nothing.
func TestHaltMarksTheCanaryOnlyWhenItTakesEffect(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Migrated(t)
	s := New(pool, nil, Options{})
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"a", "b"} {
		exec(`INSERT INTO tenants (id, status) VALUES ($1, 'active')`, id)
		exec(`INSERT INTO tenant_instances (tenant_id, job_id, image_version, state) VALUES ($1, $2, '0.7.1', 'running')`, id, "sanholo-tenant-"+id)
	}
	exec(`INSERT INTO releases (version, state) VALUES ('0.8.0', 'available'), ('0.9.0', 'rolling')`)
	exec(`INSERT INTO rollout_tenants (release_version, tenant_id, batch, state, previous_version) VALUES ('0.8.0', 'a', 0, 'updating', '0.7.1'), ('0.9.0', 'b', 0, 'updating', '0.7.1')`)
	marked := func(id string) bool {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM tenant_instances WHERE tenant_id = $1 AND last_unhealthy_at IS NOT NULL`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	// The release was abandoned (available) before the halt got to it.
	_ = s.halt(ctx, Release{Version: "0.8.0"}, Row{TenantID: "a", Batch: 0}, "x")
	if marked("a") {
		t.Error("a halt that did nothing marked the canary unhealthy")
	}
	_ = s.halt(ctx, Release{Version: "0.9.0"}, Row{TenantID: "b", Batch: 0}, "x")
	if !marked("b") {
		t.Error("a halt that took effect did not mark the canary unhealthy")
	}
}
