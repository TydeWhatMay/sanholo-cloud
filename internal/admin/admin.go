// Package admin is the operator's side of the control plane: tenant,
// waiting-list and invite management, suspension, operator accounts and the
// audit log. Every change writes its audit row in the same transaction. No
// function here reads or returns a game token: the database holds none.
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lhaig/sanholo-cloud/internal/auth"
	"github.com/lhaig/sanholo-cloud/internal/mail"
)

// ActorCLI is the audit actor of the operator command line.
const ActorCLI = "cli"

var (
	// ErrNotFound means the tenant, account, waiting-list entry or invite
	// does not exist.
	ErrNotFound = errors.New("not found")
	// ErrState means the change does not apply to the current state.
	ErrState = errors.New("not applicable in the current state")
	// ErrNoPasskey means the account has no passkey, so it cannot be made an
	// operator: /admin accepts passkey sessions only.
	ErrNoPasskey = errors.New("the account has no passkey")
	// ErrOwnTenant means an operator tried to suspend their own tenant,
	// which would lock them out of /admin.
	ErrOwnTenant = errors.New("an operator cannot suspend their own tenant")
)

// Service is the admin operations over one PostgreSQL pool.
type Service struct {
	pool *pgxpool.Pool
	auth *auth.Service
	now  func() time.Time
}

// New returns a Service. a issues invites.
func New(pool *pgxpool.Pool, a *auth.Service) *Service {
	return &Service{pool: pool, auth: a, now: time.Now}
}

// audit writes one row inside tx. details must not hold secrets.
func (s *Service) audit(ctx context.Context, tx pgx.Tx, actor, action, target string, details map[string]any) error {
	if details == nil {
		details = map[string]any{}
	}
	raw, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("admin: audit details: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO audit_log (at, actor, action, target, details) VALUES ($1, $2, $3, $4, $5)`,
		s.now(), actor, action, target, raw); err != nil {
		return fmt.Errorf("admin: audit: %w", err)
	}
	return nil
}

// inTx runs fn and commits, or rolls everything back, audit row included.
func (s *Service) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("admin: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("admin: commit: %w", err)
	}
	return nil
}

// SetOperator grants or revokes operator rights for the account with this
// address. It is reachable only from the command line. Granting needs the
// account to hold a passkey already (ErrNoPasskey): the account signs in by
// link, adds a passkey, and only then is made an operator.
func (s *Service) SetOperator(ctx context.Context, actor, email string, on bool) error {
	email, err := auth.ValidateEmail(email)
	if err != nil {
		return err
	}
	action := "operator.revoke"
	if on {
		action = "operator.grant"
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		var id string
		var hasPasskey bool
		err := tx.QueryRow(ctx, `
			SELECT id::text, EXISTS (SELECT 1 FROM passkeys WHERE account_id = accounts.id)
			FROM accounts WHERE email_hash = $1 FOR UPDATE`, auth.HashEmail(email)).Scan(&id, &hasPasskey)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("admin: set operator: %w", err)
		}
		if on && !hasPasskey {
			return ErrNoPasskey
		}
		if _, err := tx.Exec(ctx, `UPDATE accounts SET is_operator = $2 WHERE id = $1`, id, on); err != nil {
			return fmt.Errorf("admin: set operator: %w", err)
		}
		return s.audit(ctx, tx, actor, action, "account:"+id, nil)
	})
}

// Tenant is one row of the tenant list. Instance state, health and load are
// filled in when provisioning exists.
type Tenant struct {
	ID        string
	Email     string
	CreatedAt time.Time
	Status    string
}

// Tenants lists every tenant, newest first.
func (s *Service) Tenants(ctx context.Context) ([]Tenant, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t.id, coalesce(a.email, ''), t.created_at, t.status
		FROM tenants t LEFT JOIN accounts a ON a.tenant_id = t.id
		ORDER BY t.created_at DESC, t.id`)
	if err != nil {
		return nil, fmt.Errorf("admin: tenants: %w", err)
	}
	defer rows.Close()
	var out []Tenant
	for rows.Next() {
		var t Tenant
		if err := rows.Scan(&t.ID, &t.Email, &t.CreatedAt, &t.Status); err != nil {
			return nil, fmt.Errorf("admin: tenants: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// WaitlistEntry is one waiting-list address and the state of its latest
// invite: none, outstanding, used, expired or revoked.
type WaitlistEntry struct {
	Email       string
	CreatedAt   time.Time
	InviteState string
}

// Waitlist lists the waiting list, oldest first.
func (s *Service) Waitlist(ctx context.Context) ([]WaitlistEntry, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT w.email, w.created_at, coalesce((
			SELECT CASE WHEN i.revoked_at IS NOT NULL THEN 'revoked'
			            WHEN i.used_at IS NOT NULL THEN 'used'
			            WHEN i.expires_at <= $1 THEN 'expired'
			            ELSE 'outstanding' END
			FROM invites i WHERE lower(i.email) = lower(w.email)
			ORDER BY i.created_at DESC, i.id DESC LIMIT 1), 'none')
		FROM waitlist w ORDER BY w.created_at, w.id`, s.now())
	if err != nil {
		return nil, fmt.Errorf("admin: waitlist: %w", err)
	}
	defer rows.Close()
	var out []WaitlistEntry
	for rows.Next() {
		var w WaitlistEntry
		if err := rows.Scan(&w.Email, &w.CreatedAt, &w.InviteState); err != nil {
			return nil, fmt.Errorf("admin: waitlist: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// IssueInvite invites an address on the waiting list: it revokes any live
// invite for the address, stores a new one (hash only) and queues the email
// that carries the code, all with the audit row in one transaction. The code
// is in that email and nowhere else.
func (s *Service) IssueInvite(ctx context.Context, actor, email string) error {
	return s.invite(ctx, actor, email, "invite.issue", `SELECT email FROM waitlist WHERE lower(email) = lower($1)`)
}

// ResendInvite does the same for an address that already has an invite.
func (s *Service) ResendInvite(ctx context.Context, actor, email string) error {
	return s.invite(ctx, actor, email, "invite.resend", `SELECT email FROM invites WHERE lower(email) = lower($1) ORDER BY created_at DESC, id DESC LIMIT 1`)
}

// invite mails the address as stored on the waiting list (or on the earlier
// invite), not as typed into the admin form.
func (s *Service) invite(ctx context.Context, actor, email, action, lookup string) error {
	email, err := auth.ValidateEmail(email)
	if err != nil {
		return err
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		var stored string
		err := tx.QueryRow(ctx, lookup, email).Scan(&stored)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("admin: invite: %w", err)
		}
		email = stored
		var taken bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM accounts WHERE email_hash = $1)`, auth.HashEmail(email)).Scan(&taken); err != nil {
			return fmt.Errorf("admin: invite: %w", err)
		}
		if taken {
			return auth.ErrAccountExists
		}
		revoked, err := s.auth.RevokeInvitesTx(ctx, tx, email)
		if err != nil {
			return err
		}
		inviteID, code, expires, err := s.auth.IssueInviteTx(ctx, tx, email)
		if err != nil {
			return err
		}
		if err := mail.Enqueue(ctx, tx, auth.InviteMessage(email, s.auth.SignupLink(code), expires), expires); err != nil {
			return err
		}
		// The address is personal data and audit rows outlive an account
		// (US-018), so the row names the invite by its id. The invite row is
		// deleted 14 days after it expires, and the id then links to nothing.
		return s.audit(ctx, tx, actor, action, "invite:"+inviteID, map[string]any{
			"expires": expires.UTC().Format(time.RFC3339), "revoked": revoked,
		})
	})
}

// Suspend marks a pending or active tenant suspended. It keeps its place in
// the beta cap and remembers the status to return to. The actor (an account
// id when it comes from the web) cannot suspend their own tenant.
func (s *Service) Suspend(ctx context.Context, actor, tenantID string) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		var own bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM accounts WHERE id::text = $1 AND tenant_id = $2)`, actor, tenantID).Scan(&own); err != nil {
			return fmt.Errorf("admin: suspend: %w", err)
		}
		if own {
			return ErrOwnTenant
		}
		tag, err := tx.Exec(ctx, `
			UPDATE tenants SET status_before_suspend = status, status = 'suspended'
			WHERE id = $1 AND status IN ('pending', 'active')`, tenantID)
		if err != nil {
			return fmt.Errorf("admin: suspend: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return s.missingOrState(ctx, tx, tenantID)
		}
		return s.audit(ctx, tx, actor, "tenant.suspend", "tenant:"+tenantID, nil)
	})
}

// Unsuspend returns a suspended tenant to the status it had.
func (s *Service) Unsuspend(ctx context.Context, actor, tenantID string) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE tenants SET status = coalesce(status_before_suspend, 'active'), status_before_suspend = NULL
			WHERE id = $1 AND status = 'suspended'`, tenantID)
		if err != nil {
			return fmt.Errorf("admin: unsuspend: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return s.missingOrState(ctx, tx, tenantID)
		}
		return s.audit(ctx, tx, actor, "tenant.unsuspend", "tenant:"+tenantID, nil)
	})
}

func (s *Service) missingOrState(ctx context.Context, tx pgx.Tx, tenantID string) error {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tenants WHERE id = $1)`, tenantID).Scan(&exists); err != nil {
		return fmt.Errorf("admin: tenant: %w", err)
	}
	if !exists {
		return ErrNotFound
	}
	return ErrState
}

// AuditEntry is one audit row.
type AuditEntry struct {
	At      time.Time
	Actor   string
	Action  string
	Target  string
	Details string
}

// Audit returns the newest rows first.
func (s *Service) Audit(ctx context.Context, limit int) ([]AuditEntry, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT at, actor, action, target, details::text FROM audit_log ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("admin: audit: %w", err)
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.At, &e.Actor, &e.Action, &e.Target, &e.Details); err != nil {
			return nil, fmt.Errorf("admin: audit: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
