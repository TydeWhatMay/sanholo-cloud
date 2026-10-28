// Package connect is the onboarding of a customer's game account (US-011):
// validate an API token with one game read, hold the validated connection
// for confirmation, then store the credentials in qm and the non-secret
// facts in PostgreSQL.
//
// The token lives in three places only: the request that carries it, the
// in-memory pending entry (PendingTTL, then gone) and the qm write. It is
// never in PostgreSQL, a log line, an error or an audit row.
package connect

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lhaig/sanholo-cloud/internal/deletionlog"
	"github.com/lhaig/sanholo-cloud/internal/game"
	"github.com/lhaig/sanholo-cloud/internal/provision"
	"github.com/lhaig/sanholo-cloud/internal/secrets"
)

var (
	// ErrNotAvailable: secrets are not configured (development), so nothing
	// can be connected. No game request was made.
	ErrNotAvailable = errors.New("connecting a game account is not available")
	// ErrRejected: the game did not accept the token.
	ErrRejected = errors.New("the game did not accept the token")
	// ErrGameUnreachable: the game could not be reached; retrying may work.
	ErrGameUnreachable = errors.New("the game could not be reached")
	// ErrNoReplicant: the token is valid but the game account has no
	// replicant to run.
	ErrNoReplicant = errors.New("the game account has no replicant")
	// ErrTaken: the game account is connected to another tenant.
	ErrTaken = errors.New("the game account is connected to another tenant")
	// ErrLimit: the tenant already has a different game account (beta limit).
	ErrLimit = errors.New("beta limit: one game account")
	// ErrExpired: there is no live pending connection for the request.
	ErrExpired = errors.New("the pending connection is missing or expired")
	// ErrStore: the credentials could not be stored; nothing was recorded.
	ErrStore = errors.New("the credentials could not be stored")
	// ErrBusy: too many connections are waiting for confirmation.
	ErrBusy = errors.New("too many pending connections")
	// ErrDeleting: the tenant's account is being deleted; nothing can be
	// connected to it any more.
	ErrDeleting = errors.New("the account is being deleted")
)

// Service connects game accounts. It is safe for concurrent use.
type Service struct {
	pool *pgxpool.Pool
	sec  *secrets.Client
	game *game.Client
	prov *provision.Service
	// dlog records account deletions outside the database (nil: none).
	dlog *deletionlog.Log
	// lockWait is how long a disconnect waits for the tenant's lock.
	lockWait time.Duration
	log      *slog.Logger
	now      func() time.Time
	pend     *pendingStore
}

// New returns a Service. maxPerTenant is BETA_MAX_GAME_ACCOUNTS_PER_TENANT:
// qm holds one game credential per tenant and game_accounts.tenant_id is
// unique, so only 1 is supported, and anything else is refused here rather
// than silently ignored. gc may be nil (the service is then unavailable).
func New(pool *pgxpool.Pool, sec *secrets.Client, gc *game.Client, log *slog.Logger, maxPerTenant int) (*Service, error) {
	if maxPerTenant != 1 {
		return nil, fmt.Errorf("connect: BETA_MAX_GAME_ACCOUNTS_PER_TENANT=%d is not supported yet: the secrets layout holds one game account per tenant", maxPerTenant)
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Service{pool: pool, sec: sec, game: gc, log: log, now: time.Now, lockWait: 30 * time.Second}
	s.pend = newPendingStore(func() time.Time { return s.now() })
	return s, nil
}

// SetClock replaces the time source (tests).
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// Available reports whether a game account can be connected: qm is
// configured and so is the game client.
func (s *Service) Available() bool { return s != nil && s.game != nil && s.sec.Enabled() }

// Preview is what the customer confirms.
type Preview struct {
	ReplicantName string
	ReplicantCode string
	// Reconnect is true when the tenant already has this game account and
	// the new token will replace the stored one.
	Reconnect bool
}

// Connected is the stored, non-secret record.
type Connected struct {
	ReplicantName string
	ReplicantCode string
	ConnectedAt   time.Time
	// Reconnect is true when this confirmation replaced the token of a game
	// account the tenant already had (Confirm only).
	Reconnect bool
}

// Begin validates token with one game read and, if the game account may be
// connected to this tenant, holds the connection for Confirm. It returns the
// secret for the confirmation cookie. Nothing is written to qm or the
// database.
func (s *Service) Begin(ctx context.Context, accountID, tenantID, token string) (Preview, string, error) {
	if !s.Available() {
		return Preview{}, "", ErrNotAvailable
	}
	acct, err := s.game.Me(ctx, token)
	switch {
	case errors.Is(err, game.ErrRejected):
		return Preview{}, "", ErrRejected
	case errors.Is(err, game.ErrNoReplicant):
		return Preview{}, "", ErrNoReplicant
	case errors.Is(err, game.ErrUnavailable), errors.Is(err, game.ErrProtocol):
		s.log.Warn("connect: game read failed", "tenant", tenantID)
		return Preview{}, "", ErrGameUnreachable
	case err != nil:
		return Preview{}, "", ErrGameUnreachable
	}
	reconnect, err := s.check(ctx, tenantID, acct.ID)
	if err != nil {
		return Preview{}, "", err
	}
	secret, err := s.pend.put(pendingEntry{accountID: accountID, tenantID: tenantID, token: token, acct: acct})
	if err != nil {
		return Preview{}, "", err
	}
	return Preview{ReplicantName: acct.ReplicantName, ReplicantCode: acct.ReplicantCode, Reconnect: reconnect}, secret, nil
}

// check applies the ownership rules to what is already recorded. The unique
// constraints enforce them again at write time.
func (s *Service) check(ctx context.Context, tenantID, gameAccountID string) (reconnect bool, err error) {
	rows, err := s.pool.Query(ctx,
		`SELECT tenant_id, game_account_id FROM game_accounts WHERE tenant_id = $1 OR game_account_id = $2`, tenantID, gameAccountID)
	if err != nil {
		return false, fmt.Errorf("connect: check: %w", err)
	}
	defer rows.Close()
	var ownLimit bool
	for rows.Next() {
		var t, g string
		if err := rows.Scan(&t, &g); err != nil {
			return false, fmt.Errorf("connect: check: %w", err)
		}
		switch {
		case g == gameAccountID && t != tenantID:
			return false, ErrTaken
		case g == gameAccountID:
			reconnect = true
		default:
			ownLimit = true
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("connect: check: %w", err)
	}
	if ownLimit {
		return false, ErrLimit
	}
	return reconnect, nil
}

// Confirm stores the pending connection named by secret. The database row
// is written first inside a transaction, then qm, then the transaction
// commits, so a lost race for the game account (unique violation) writes
// nothing to qm, and a failed qm write leaves no row.
func (s *Service) Confirm(ctx context.Context, accountID, tenantID, secret string) (Connected, error) {
	if !s.Available() {
		return Connected{}, ErrNotAvailable
	}
	e, ok := s.pend.get(secret, accountID)
	if !ok || e.tenantID != tenantID {
		return Connected{}, ErrExpired
	}
	// One attempt per validation, whatever the outcome: the token is not
	// held any longer than needed, and a retry asks for it again.
	defer s.pend.drop(secret, accountID)
	return s.store(ctx, e)
}

// Cancel discards a pending connection.
func (s *Service) Cancel(accountID, secret string) {
	if s != nil && secret != "" {
		s.pend.drop(secret, accountID)
	}
}

func (s *Service) store(ctx context.Context, e pendingEntry) (Connected, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Connected{}, fmt.Errorf("connect: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Serialise connects for one tenant; the unique constraints serialise
	// them across tenants.
	// An account deletion marks the tenant 'deleting' under this same row
	// lock, so a connect that was waiting for confirmation either commits
	// before the mark (and the deletion removes what it wrote) or finds the
	// mark here and writes nothing, to qm or to the database.
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM tenants WHERE id = $1 FOR UPDATE`, e.tenantID).Scan(&status); err != nil {
		return Connected{}, fmt.Errorf("connect: tenant: %w", err)
	}
	if status == "deleting" {
		return Connected{}, ErrDeleting
	}
	var existing string
	reconnect := false
	switch err := tx.QueryRow(ctx, `SELECT game_account_id FROM game_accounts WHERE tenant_id = $1`, e.tenantID).Scan(&existing); {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return Connected{}, fmt.Errorf("connect: existing: %w", err)
	case existing != e.acct.ID:
		return Connected{}, ErrLimit
	default:
		reconnect = true
	}

	now := s.now()
	if reconnect {
		_, err = tx.Exec(ctx,
			`UPDATE game_accounts SET replicant_name = $2, replicant_code = $3, connected_at = $4 WHERE tenant_id = $1`,
			e.tenantID, e.acct.ReplicantName, e.acct.ReplicantCode, now)
	} else {
		_, err = tx.Exec(ctx,
			`INSERT INTO game_accounts (tenant_id, game_account_id, replicant_name, replicant_code, connected_at) VALUES ($1, $2, $3, $4, $5)`,
			e.tenantID, e.acct.ID, e.acct.ReplicantName, e.acct.ReplicantCode, now)
	}
	if err != nil {
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23505" {
			return Connected{}, ErrTaken
		}
		return Connected{}, fmt.Errorf("connect: record: %w", err)
	}

	// A purge still pending from an earlier disconnect would remove what this
	// connect is about to write; it is moot now (a later disconnect or account
	// deletion purges everything again). A pending volume deletion is not
	// cancelled: the start that follows carries it out before it creates the
	// tenant's new volume, so another game account never inherits the data.
	if _, err := tx.Exec(ctx, `DELETE FROM pending_cleanups WHERE tenant_key = $1 AND kind = 'secret'`, e.tenantID); err != nil {
		return Connected{}, fmt.Errorf("connect: cancel pending deletions: %w", err)
	}

	cr := secrets.Credentials{APIToken: e.token, ReplicantCode: e.acct.ReplicantCode}
	// A reconnect is a plain new version of the same secret: qm keeps 16 and
	// prunes the oldest, so there is no separate rotate step.
	if err = s.sec.Put(ctx, e.tenantID, cr); err != nil {
		s.log.Warn("connect: qm write failed", "tenant", e.tenantID, "error", err)
		if !reconnect && mayHaveWritten(err) {
			s.purge(ctx, e.tenantID)
		}
		return Connected{}, ErrStore
	}
	if err := tx.Commit(ctx); err != nil {
		s.log.Warn("connect: commit failed after the qm write", "tenant", e.tenantID)
		if !reconnect {
			s.purge(ctx, e.tenantID)
		}
		return Connected{}, ErrStore
	}
	s.log.Info("connect: game account connected", "tenant", e.tenantID, "reconnect", reconnect)
	return Connected{ReplicantName: e.acct.ReplicantName, ReplicantCode: e.acct.ReplicantCode, ConnectedAt: now, Reconnect: reconnect}, nil
}

// mayHaveWritten: the qm request may have landed although we saw an error
// (a timeout or an unreadable answer), so a first connect must not leave a
// secret behind with no row.
func mayHaveWritten(err error) bool {
	return errors.Is(err, secrets.ErrUnavailable) || errors.Is(err, secrets.ErrProtocol)
}

// purge removes what this tenant has in qm. It is called only for a first
// connect, when the tenant had no game account recorded, so nothing of value
// is lost. Best effort: a failure is logged (the error carries no secret)
// and does not change what the customer sees. The purge contract is final
// (ADR 0024); until the route is deployed on the production qm every purge
// fails with ErrDenied (403). The stray secret is the tenant's own, unused,
// and removed by the next purge.
func (s *Service) purge(ctx context.Context, tenantID string) {
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	if err := s.sec.PurgeTenant(pctx, tenantID); err != nil {
		s.log.Error("connect: could not purge a stray secret", "tenant", tenantID, "error", err)
	}
}

// Get returns the tenant's connected game account, or nil.
func (s *Service) Get(ctx context.Context, tenantID string) (*Connected, error) {
	var c Connected
	err := s.pool.QueryRow(ctx,
		`SELECT replicant_name, replicant_code, connected_at FROM game_accounts WHERE tenant_id = $1`, tenantID,
	).Scan(&c.ReplicantName, &c.ReplicantCode, &c.ConnectedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("connect: get: %w", err)
	}
	return &c, nil
}

// SetLockWait replaces how long a disconnect waits for the tenant's lock
// (tests).
func (s *Service) SetLockWait(d time.Duration) { s.lockWait = d }
