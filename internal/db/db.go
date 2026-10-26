// Package db opens the control plane's PostgreSQL pool, applies the embedded
// migrations and answers schema questions. All queries are parameterised.
package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Open connects a pool and verifies it with a ping.
func Open(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		// The parse error can quote the URL, password included.
		return nil, fmt.Errorf("db: invalid connection settings")
	}
	return OpenConfig(ctx, cfg)
}

// OpenConfig is Open for a prepared pool config (tests set search_path).
func OpenConfig(ctx context.Context, cfg *pgxpool.Config) (*pgxpool.Pool, error) {
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db: open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return pool, nil
}

// TenantTables lists the base tables in the current schema that have a
// tenant_id column. US-018's deletion check counts rows in each, so the list
// comes from the schema and is never kept by hand. The tenants table itself
// is keyed by id, not tenant_id, and is handled separately by deletion.
func TenantTables(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	rows, err := pool.Query(ctx, `
		SELECT c.table_name
		FROM information_schema.columns c
		JOIN information_schema.tables t
		  ON t.table_schema = c.table_schema AND t.table_name = c.table_name
		WHERE c.table_schema = current_schema()
		  AND c.column_name = 'tenant_id'
		  AND t.table_type = 'BASE TABLE'
		ORDER BY c.table_name`)
	if err != nil {
		return nil, fmt.Errorf("db: list tenant tables: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("db: scan tenant table: %w", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: list tenant tables: %w", err)
	}
	return out, nil
}
