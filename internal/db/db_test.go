package db_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/lhaig/sanholo-cloud/internal/db"
	"github.com/lhaig/sanholo-cloud/internal/dbtest"
	"github.com/lhaig/sanholo-cloud/internal/tenant"
)

// migrationCount is the number of embedded migration files: each applies
// exactly once however many times Migrate runs.
func migrationCount(t *testing.T) int {
	t.Helper()
	files, err := filepath.Glob("migrations/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("list migrations: %v", err)
	}
	return len(files)
}

func TestMigrateTwice(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	for i := range 2 {
		if err := db.Migrate(ctx, pool); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if want := migrationCount(t); n != want {
		t.Fatalf("schema_migrations rows = %d, want %d after two runs", n, want)
	}
}

// Two instances starting together: both succeed, the migration applies once.
func TestMigrateConcurrent(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = db.Migrate(ctx, pool)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("migrator %d: %v", i, err)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if want := migrationCount(t); n != want {
		t.Fatalf("schema_migrations rows = %d, want %d", n, want)
	}
}

func TestTenantIDConstraint(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Migrated(t)
	cases := []struct {
		id string
		ok bool
	}{
		{"a", true},
		{"ab-cd", true},
		{strings.Repeat("a", 63), true},
		{tenant.NewID(), true},
		{"", false},
		{"-a", false},
		{"a-", false},
		{"Abc", false},
		{"a_b", false},
		{"a\n", false},
		{strings.Repeat("a", 64), false},
	}
	for _, tc := range cases {
		_, err := pool.Exec(ctx, "INSERT INTO tenants (id) VALUES ($1)", tc.id)
		if tc.ok && err != nil {
			t.Errorf("id %q rejected: %v", tc.id, err)
		}
		if !tc.ok {
			var pgErr *pgconn.PgError
			if err == nil {
				t.Errorf("id %q accepted, want CHECK violation", tc.id)
			} else if !asPg(err, &pgErr) || pgErr.Code != "23514" {
				t.Errorf("id %q: error %v, want check_violation", tc.id, err)
			}
		}
		// The Go rule and the database rule must agree.
		if tenant.Valid(tc.id) != tc.ok {
			t.Errorf("tenant.Valid(%q) disagrees with the table", tc.id)
		}
	}
}

func TestAccountEmailUniqueCaseInsensitive(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Migrated(t)
	for _, id := range []string{"t1", "t2"} {
		if _, err := pool.Exec(ctx, "INSERT INTO tenants (id) VALUES ($1)", id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, "INSERT INTO accounts (tenant_id, email, email_hash) VALUES ($1, $2, 'h1')", "t1", "Ann@Example.com"); err != nil {
		t.Fatal(err)
	}
	_, err := pool.Exec(ctx, "INSERT INTO accounts (tenant_id, email, email_hash) VALUES ($1, $2, 'h2')", "t2", "ann@example.COM")
	var pgErr *pgconn.PgError
	if err == nil || !asPg(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("duplicate email differing in case: err = %v, want unique_violation", err)
	}
	// An account needs a real tenant.
	_, err = pool.Exec(ctx, "INSERT INTO accounts (tenant_id, email, email_hash) VALUES ($1, $2, 'h3')", "nope", "x@example.com")
	if err == nil || !asPg(err, &pgErr) || pgErr.Code != "23503" {
		t.Fatalf("orphan account: err = %v, want foreign_key_violation", err)
	}
}

func TestTenantTables(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Migrated(t)

	got, err := db.TenantTables(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "accounts,game_accounts,rollout_tenants,tenant_instances" {
		t.Fatalf("TenantTables = %v, want [accounts game_accounts rollout_tenants tenant_instances]", got)
	}

	// A future table with tenant_id appears with no hand-kept list; a view
	// and a table without the column do not.
	for _, q := range []string{
		"CREATE TABLE sessions (id int, tenant_id text)",
		"CREATE TABLE unrelated (id int)",
		"CREATE VIEW accounts_view AS SELECT tenant_id FROM accounts",
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	got, err = db.TenantTables(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "accounts,game_accounts,rollout_tenants,sessions,tenant_instances" {
		t.Fatalf("TenantTables = %v, want [accounts game_accounts rollout_tenants sessions tenant_instances]", got)
	}
}

func asPg(err error, target **pgconn.PgError) bool { return errors.As(err, target) }
