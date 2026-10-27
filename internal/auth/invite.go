package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/lhaig/sanholo-cloud/internal/mail"
	"github.com/lhaig/sanholo-cloud/internal/tenant"
)

// InviteTTL is how long an invite stays redeemable.
const InviteTTL = 14 * 24 * time.Hour

var (
	ErrInviteRequired = errors.New("an invite is required")
	ErrInviteInvalid  = errors.New("invite not found")
	ErrInviteUsed     = errors.New("invite already used")
	ErrInviteExpired  = errors.New("invite expired")
	ErrBetaFull       = errors.New("the beta is full")
	ErrAccountExists  = errors.New("an account already exists for this address")
)

// JoinWaitlist stores the address once, compared case-insensitively. A
// repeat is not an error and not reported: callers show the same page.
func (s *Service) JoinWaitlist(ctx context.Context, email string) error {
	email, err := ValidateEmail(email)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO waitlist (email, created_at) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		email, s.now())
	if err != nil {
		return fmt.Errorf("auth: waitlist: %w", err)
	}
	return nil
}

// IssueInvite creates a single-use invite bound to email and returns the
// plaintext code. This is the only moment the code exists in clear: the
// database keeps its SHA-256.
func (s *Service) IssueInvite(ctx context.Context, email string) (code string, expires time.Time, err error) {
	return s.issueInvite(ctx, s.pool, email)
}

// IssueInviteTx is IssueInvite inside the caller's transaction, so the
// invite, its email and an audit row commit together.
func (s *Service) IssueInviteTx(ctx context.Context, tx pgx.Tx, email string) (code string, expires time.Time, err error) {
	return s.issueInvite(ctx, tx, email)
}

// RevokeInvitesTx revokes every unused invite for the address, so a resend
// leaves exactly one live code. It reports how many it revoked.
func (s *Service) RevokeInvitesTx(ctx context.Context, tx pgx.Tx, email string) (int64, error) {
	tag, err := tx.Exec(ctx, `
		UPDATE invites SET revoked_at = $2
		WHERE lower(email) = lower($1) AND used_at IS NULL AND revoked_at IS NULL`, email, s.now())
	if err != nil {
		return 0, fmt.Errorf("auth: revoke invites: %w", err)
	}
	return tag.RowsAffected(), nil
}

// SignupLink is the page an invite code opens.
func (s *Service) SignupLink(code string) string {
	return s.opt.BaseURL + "/signup?code=" + code
}

// InviteMessage is the invite email, in the company register.
func InviteMessage(to, link string, expires time.Time) mail.Message {
	return mail.Message{
		To:      to,
		Subject: "Your SanHolo invite",
		Body: "TydeWhatMay invites you to the SanHolo beta. You asked to join the waiting list with this address.\n\n" +
			link + "\n\n" +
			"The link works once. It expires on " + expires.UTC().Format("2006-01-02") + ".\n" +
			"If you did not ask to join, ignore this email. Nothing happens until the link is opened.\n",
	}
}

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func (s *Service) issueInvite(ctx context.Context, db execer, email string) (code string, expires time.Time, err error) {
	email, err = ValidateEmail(email)
	if err != nil {
		return "", time.Time{}, err
	}
	code = NewSecret()
	now := s.now()
	expires = now.Add(InviteTTL)
	_, err = db.Exec(ctx,
		`INSERT INTO invites (code_hash, email, created_at, expires_at) VALUES ($1, $2, $3, $4)`,
		HashSecret(code), email, now, expires)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("auth: issue invite: %w", err)
	}
	return code, expires, nil
}

// InviteEmail returns the address an unused, unexpired invite is bound to,
// without consuming it (the sign-up page shows it).
func (s *Service) InviteEmail(ctx context.Context, code string) (string, error) {
	var email string
	var expires time.Time
	var used, revoked *time.Time
	err := s.pool.QueryRow(ctx,
		`SELECT email, expires_at, used_at, revoked_at FROM invites WHERE code_hash = $1`, HashSecret(code),
	).Scan(&email, &expires, &used, &revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrInviteInvalid
	}
	if err != nil {
		return "", fmt.Errorf("auth: invite lookup: %w", err)
	}
	if revoked != nil {
		return "", ErrInviteInvalid
	}
	if used != nil {
		return "", ErrInviteUsed
	}
	if !s.now().Before(expires) {
		return "", ErrInviteExpired
	}
	return email, nil
}

// signupLockKey serialises tenant creation across instances so the cap
// holds under concurrent redemptions.
const signupLockKey int64 = 0x53414e48535550 // "SANHSUP"

// Redeem creates a tenant and its account in one transaction. A valid
// invite is always required; the account takes the address the invite is
// bound to, so an address is never accepted unproven. Nothing is created
// unless every check passes, and a failed redemption leaves the invite
// unused.
//
// The cap is checked under a transaction-scoped advisory lock, so two
// redemptions at the boundary cannot both pass: the second waits, then sees
// the first one's tenant in its count.
func (s *Service) Redeem(ctx context.Context, code string) (tenantID string, err error) {
	if code == "" {
		return "", ErrInviteRequired
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("auth: redeem: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, signupLockKey); err != nil {
		return "", fmt.Errorf("auth: redeem: lock: %w", err)
	}
	now := s.now()
	var email string
	{
		var expires time.Time
		var used, revoked *time.Time
		err := tx.QueryRow(ctx,
			`SELECT email, expires_at, used_at, revoked_at FROM invites WHERE code_hash = $1 FOR UPDATE`, HashSecret(code),
		).Scan(&email, &expires, &used, &revoked)
		switch {
		case errors.Is(err, pgx.ErrNoRows), err == nil && revoked != nil:
			return "", ErrInviteInvalid
		case err != nil:
			return "", fmt.Errorf("auth: redeem: invite: %w", err)
		case used != nil:
			return "", ErrInviteUsed
		case !now.Before(expires):
			return "", ErrInviteExpired
		}
	}
	var active int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM tenants WHERE status IN ('pending', 'active', 'suspended')`).Scan(&active); err != nil {
		return "", fmt.Errorf("auth: redeem: count: %w", err)
	}
	if active >= s.opt.MaxTenants {
		return "", ErrBetaFull
	}
	tenantID = tenant.NewID()
	if _, err := tx.Exec(ctx, `INSERT INTO tenants (id, created_at) VALUES ($1, $2)`, tenantID, now); err != nil {
		return "", fmt.Errorf("auth: redeem: tenant: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO accounts (tenant_id, email, email_hash, created_at) VALUES ($1, $2, $3, $4)`,
		tenantID, email, HashEmail(email), now); err != nil {
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23505" {
			return "", ErrAccountExists
		}
		return "", fmt.Errorf("auth: redeem: account: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE invites SET used_at = $2 WHERE code_hash = $1`, HashSecret(code), now); err != nil {
		return "", fmt.Errorf("auth: redeem: mark used: %w", err)
	}
	// The waiting-list entry has done its job; the privacy notice says it
	// goes when the account is created.
	if _, err := tx.Exec(ctx, `DELETE FROM waitlist WHERE lower(email) = lower($1)`, email); err != nil {
		return "", fmt.Errorf("auth: redeem: waitlist: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("auth: redeem: commit: %w", err)
	}
	return tenantID, nil
}
