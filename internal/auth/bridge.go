package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	// HandoffTTL is how long a handoff code lives.
	HandoffTTL = 60 * time.Second
	// BridgeSessionMax is the longest a Bridge session lives. It never
	// outlives the account session it came from.
	BridgeSessionMax = 24 * time.Hour
)

// ErrHandoffInvalid means the code is unknown, used, expired, for another
// tenant, or its account session or tenant is gone.
var ErrHandoffInvalid = errors.New("handoff code not valid")

// CreateHandoff makes a one-time code that opens the signed-in account's own
// Bridge. The tenant comes from the account row, not from the caller, so a
// code can never name another customer's Bridge.
func (s *Service) CreateHandoff(ctx context.Context, accountSessionID string) (code, tenantID string, err error) {
	now := s.now()
	code = NewSecret()
	err = s.pool.QueryRow(ctx, `
		INSERT INTO bridge_handoffs (code_hash, account_session_id, created_at, expires_at)
		SELECT $1, s.id, $3, $4
		FROM account_sessions s JOIN accounts a ON a.id = s.account_id JOIN tenants t ON t.id = a.tenant_id
		WHERE s.id = $2 AND s.expires_at > $3 AND t.status IN ('pending', 'active')
		RETURNING (SELECT a.tenant_id FROM accounts a WHERE a.id = (SELECT account_id FROM account_sessions WHERE id = $2))`,
		HashSecret(code), accountSessionID, now, now.Add(HandoffTTL)).Scan(&tenantID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNoSession
	}
	if err != nil {
		return "", "", fmt.Errorf("auth: create handoff: %w", err)
	}
	return code, tenantID, nil
}

// RedeemHandoff spends a code for tenantID (the tenant of the host it
// arrived on) and opens a Bridge session. A wrong tenant, a spent or expired
// code, a revoked account session and a suspended or deleting tenant all give
// ErrHandoffInvalid, and a wrong-tenant attempt does not spend the code.
func (s *Service) RedeemHandoff(ctx context.Context, code, tenantID string) (secret string, expires time.Time, err error) {
	if code == "" {
		return "", time.Time{}, ErrHandoffInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("auth: redeem handoff: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	now := s.now()
	var accountSessionID string
	var sessionExpires time.Time
	err = tx.QueryRow(ctx, `
		UPDATE bridge_handoffs h SET used_at = $3
		FROM account_sessions s, accounts a, tenants t
		WHERE h.code_hash = $1 AND h.used_at IS NULL AND h.expires_at > $3
		  AND s.id = h.account_session_id AND s.expires_at > $3
		  AND a.id = s.account_id AND a.tenant_id = $2
		  AND t.id = a.tenant_id AND t.status IN ('pending', 'active')
		RETURNING h.account_session_id::text, s.expires_at`, HashSecret(code), tenantID, now).Scan(&accountSessionID, &sessionExpires)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", time.Time{}, ErrHandoffInvalid
	}
	if err != nil {
		return "", time.Time{}, fmt.Errorf("auth: redeem handoff: %w", err)
	}
	secret = NewSecret()
	expires = now.Add(BridgeSessionMax)
	if sessionExpires.Before(expires) {
		expires = sessionExpires
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO bridge_sessions (secret_hash, account_session_id, created_at, expires_at)
		VALUES ($1, $2, $3, $4)`, HashSecret(secret), accountSessionID, now, expires); err != nil {
		return "", time.Time{}, fmt.Errorf("auth: redeem handoff: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", time.Time{}, fmt.Errorf("auth: redeem handoff: commit: %w", err)
	}
	return secret, expires, nil
}

// BridgeSession is a live Bridge session.
type BridgeSession struct {
	TenantID  string
	ExpiresAt time.Time
	// Unavailable is true when the tenant is suspended or being deleted:
	// the caller answers 404 for it.
	Unavailable bool
}

// LookupBridgeSession finds the live Bridge session for a cookie secret. It
// is ErrNoSession for an unknown or expired secret and when the account
// session it came from is gone or expired. The tenant is returned so the
// caller can compare it with the host's.
func (s *Service) LookupBridgeSession(ctx context.Context, secret string) (BridgeSession, error) {
	if secret == "" {
		return BridgeSession{}, ErrNoSession
	}
	now := s.now()
	var b BridgeSession
	err := s.pool.QueryRow(ctx, `
		SELECT a.tenant_id, b.expires_at, t.status NOT IN ('pending', 'active')
		FROM bridge_sessions b
		JOIN account_sessions s ON s.id = b.account_session_id
		JOIN accounts a ON a.id = s.account_id
		JOIN tenants t ON t.id = a.tenant_id
		WHERE b.secret_hash = $1 AND b.expires_at > $2 AND s.expires_at > $2`, HashSecret(secret), now,
	).Scan(&b.TenantID, &b.ExpiresAt, &b.Unavailable)
	if errors.Is(err, pgx.ErrNoRows) {
		return BridgeSession{}, ErrNoSession
	}
	if err != nil {
		return BridgeSession{}, fmt.Errorf("auth: lookup bridge session: %w", err)
	}
	return b, nil
}

// RevokeBridgeSecret deletes the Bridge session a cookie names.
func (s *Service) RevokeBridgeSecret(ctx context.Context, secret string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM bridge_sessions WHERE secret_hash = $1`, HashSecret(secret)); err != nil {
		return fmt.Errorf("auth: revoke bridge session: %w", err)
	}
	return nil
}
