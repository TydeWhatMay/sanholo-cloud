// Package dbtest gives tests an isolated PostgreSQL schema. The gate runs the
// tests through scripts/test-db.sh, which exports TEST_DATABASE_URL; without
// it the DB tests skip.
package dbtest

import (
	"context"
	"crypto/rand"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lhaig/sanholo-cloud/internal/db"
)

// Pool returns a pool whose connections all use a fresh, empty schema that is
// dropped when the test ends. Migrations are not applied.
func Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; run through scripts/test-db.sh")
	}
	ctx := context.Background()
	schema := "t_" + strings.ToLower(rand.Text())[:16]

	admin, err := db.Open(ctx, url)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal("parse TEST_DATABASE_URL")
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := db.OpenConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// Migrated is Pool with the real migrations applied.
func Migrated(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := Pool(t)
	if err := db.Migrate(context.Background(), pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}
