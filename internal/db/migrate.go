package db

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Migrations are plain SQL files, applied in filename order. This is a small
// runner instead of goose or golang-migrate on purpose: the need is "apply
// numbered files once, under a lock", which is about sixty lines, and each
// of those libraries brings its own dependency tree (and, for goose, its own
// dialect and bookkeeping table) into a service that holds customer data.
// Files are forward-only; a mistake is fixed by the next migration.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

// migrateLockKey is the advisory lock that serialises migrators.
const migrateLockKey int64 = 0x53414e484f4c4f // "SANHOLO"

// Migrate applies every pending migration. It is idempotent, and two
// instances starting together never migrate at once: the second waits on the
// advisory lock and then finds nothing to do.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return migrate(ctx, pool, migrationFiles, "migrations")
}

func migrate(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS, dir string) error {
	names, err := fs.Glob(fsys, dir+"/*.sql")
	if err != nil {
		return fmt.Errorf("db: list migrations: %w", err)
	}
	sort.Strings(names)

	// The advisory lock belongs to one session, so hold one connection for
	// the whole run and release the lock on that same connection.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("db: migrate: acquire connection: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrateLockKey); err != nil {
		return fmt.Errorf("db: migrate: lock: %w", err)
	}
	defer func() {
		// ctx may be cancelled; the unlock must still go out.
		_, _ = conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrateLockKey)
	}()

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    text PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("db: migrate: bookkeeping table: %w", err)
	}
	for _, path := range names {
		version := strings.TrimSuffix(path[strings.LastIndex(path, "/")+1:], ".sql")
		var applied bool
		if err := conn.QueryRow(ctx,
			"SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)", version).Scan(&applied); err != nil {
			return fmt.Errorf("db: migrate %s: check: %w", version, err)
		}
		if applied {
			continue
		}
		body, err := fs.ReadFile(fsys, path)
		if err != nil {
			return fmt.Errorf("db: migrate %s: read: %w", version, err)
		}
		// One transaction per file: the schema change and its bookkeeping
		// row commit together or not at all.
		tx, err := conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf("db: migrate %s: begin: %w", version, err)
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("db: migrate %s: %w", version, err)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", version); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("db: migrate %s: record: %w", version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("db: migrate %s: commit: %w", version, err)
		}
	}
	return nil
}
