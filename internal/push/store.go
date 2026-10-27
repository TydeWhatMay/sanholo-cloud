package push

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MaxPerTenant is the most subscriptions (browsers) one tenant may hold.
const MaxPerTenant = 10

var (
	// ErrCapReached means the tenant already has MaxPerTenant subscriptions.
	ErrCapReached = errors.New("push: subscription limit reached")
	// ErrEndpointTaken means the endpoint belongs to another tenant.
	ErrEndpointTaken = errors.New("push: endpoint belongs to another tenant")
)

// Store keeps subscriptions in PostgreSQL.
type Store struct{ pool *pgxpool.Pool }

// NewStore returns a Store.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// stored is a subscription row as the sender needs it.
type stored struct {
	ID       string
	TenantID string
	Sub      Subscription
}

// Add stores sub for tenantID, made by accountID in accountSessionID. The
// endpoint is unique: the same tenant sending it again updates the keys and
// is not counted twice; another tenant's endpoint is refused. The tenant row
// is locked while counting so two requests cannot both take the last slot.
func (s *Store) Add(ctx context.Context, tenantID, accountID, accountSessionID string, sub Subscription) (created bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("push: add: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM tenants WHERE id = $1 FOR UPDATE`, tenantID); err != nil {
		return false, fmt.Errorf("push: add: %w", err)
	}
	var owner string
	err = tx.QueryRow(ctx, `SELECT tenant_id FROM push_subscriptions WHERE endpoint = $1`, sub.Endpoint).Scan(&owner)
	switch {
	case err == nil && owner != tenantID:
		return false, ErrEndpointTaken
	case err == nil:
		_, err = tx.Exec(ctx, `UPDATE push_subscriptions SET p256dh = $2, auth = $3, account_id = $4, account_session_id = $5
			WHERE endpoint = $1`, sub.Endpoint, sub.Keys.P256dh, sub.Keys.Auth, accountID, accountSessionID)
	case errors.Is(err, pgx.ErrNoRows):
		var n int
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM push_subscriptions WHERE tenant_id = $1`, tenantID).Scan(&n); err != nil {
			return false, fmt.Errorf("push: add: %w", err)
		}
		if n >= MaxPerTenant {
			return false, ErrCapReached
		}
		created = true
		_, err = tx.Exec(ctx, `INSERT INTO push_subscriptions (tenant_id, account_id, account_session_id, endpoint, p256dh, auth)
			VALUES ($1, $2, $3, $4, $5, $6)`, tenantID, accountID, accountSessionID, sub.Endpoint, sub.Keys.P256dh, sub.Keys.Auth)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return false, ErrEndpointTaken
	}
	if err != nil {
		return false, fmt.Errorf("push: add: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("push: add: commit: %w", err)
	}
	return created, nil
}

// Remove deletes the tenant's subscription for endpoint, if any.
func (s *Store) Remove(ctx context.Context, tenantID, endpoint string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM push_subscriptions WHERE tenant_id = $1 AND endpoint = $2`, tenantID, endpoint); err != nil {
		return fmt.Errorf("push: remove: %w", err)
	}
	return nil
}

func (s *Store) removeID(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM push_subscriptions WHERE id = $1`, id)
	return err
}

// Count is how many subscriptions the tenant has.
func (s *Store) Count(ctx context.Context, tenantID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM push_subscriptions WHERE tenant_id = $1`, tenantID).Scan(&n)
	return n, err
}

func (s *Store) forTenant(ctx context.Context, tenantID string) ([]stored, error) {
	return s.query(ctx, `SELECT id::text, tenant_id, endpoint, p256dh, auth FROM push_subscriptions
		WHERE tenant_id = $1 ORDER BY created_at`, tenantID)
}

// forOperators are the subscriptions of every tenant that has an operator
// account.
func (s *Store) forOperators(ctx context.Context) ([]stored, error) {
	return s.query(ctx, `SELECT p.id::text, p.tenant_id, p.endpoint, p.p256dh, p.auth FROM push_subscriptions p
		WHERE EXISTS (SELECT 1 FROM accounts a WHERE a.tenant_id = p.tenant_id AND a.is_operator)
		ORDER BY p.created_at`)
}

func (s *Store) query(ctx context.Context, sql string, args ...any) ([]stored, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []stored
	for rows.Next() {
		var r stored
		if err := rows.Scan(&r.ID, &r.TenantID, &r.Sub.Endpoint, &r.Sub.Keys.P256dh, &r.Sub.Keys.Auth); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TenantActive reports whether tenantID exists and may use the service
// (pending or active, not being deleted).
func (s *Store) TenantActive(ctx context.Context, tenantID string) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tenants WHERE id = $1 AND status IN ('pending', 'active'))`, tenantID).Scan(&ok)
	return ok, err
}
