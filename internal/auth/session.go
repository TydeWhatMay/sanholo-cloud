package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	// SessionTTL is the sliding lifetime of a session.
	SessionTTL = 30 * 24 * time.Hour
	// refreshAfter limits how often a request extends the session.
	refreshAfter = 5 * time.Minute
)

// How a session was opened. /admin accepts only MethodPasskey.
const (
	MethodMagicLink = "magic_link"
	MethodPasskey   = "passkey"
)

// ErrNoSession means the cookie names no live session.
var ErrNoSession = errors.New("no session")

// Session is a live server-side session joined to its account.
type Session struct {
	ID         string // public row id, safe to put in a page
	AccountID  string
	TenantID   string
	Email      string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
	UserAgent  string
	// Method is MethodMagicLink or MethodPasskey.
	Method     string
	IsOperator bool
	// Refreshed is true when this lookup extended the expiry, so the caller
	// should re-issue the cookie with the new lifetime.
	Refreshed bool
}

func (s *Service) createSession(ctx context.Context, tx pgx.Tx, accountID, userAgent, method string, now time.Time) (string, time.Time, error) {
	secret := NewSecret()
	expires := now.Add(SessionTTL)
	if _, err := tx.Exec(ctx, `
		INSERT INTO account_sessions (secret_hash, account_id, created_at, last_seen_at, expires_at, user_agent, method)
		VALUES ($1, $2, $3, $3, $4, $5, $6)`,
		HashSecret(secret), accountID, now, expires, truncateUA(userAgent), method); err != nil {
		return "", time.Time{}, fmt.Errorf("auth: create session: %w", err)
	}
	return secret, expires, nil
}

func truncateUA(s string) string {
	out := make([]rune, 0, 120)
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		if len(out) == 120 {
			break
		}
		out = append(out, r)
	}
	return string(out)
}

// LookupSession finds the live session for a cookie secret and slides its
// expiry. An unknown, revoked or expired secret is ErrNoSession.
func (s *Service) LookupSession(ctx context.Context, secret string) (Session, error) {
	if secret == "" {
		return Session{}, ErrNoSession
	}
	now := s.now()
	var ses Session
	err := s.pool.QueryRow(ctx, `
		SELECT s.id::text, s.account_id::text, a.tenant_id, a.email, s.created_at, s.last_seen_at, s.expires_at, s.user_agent, s.method, a.is_operator
		FROM account_sessions s JOIN accounts a ON a.id = s.account_id
		WHERE s.secret_hash = $1 AND s.expires_at > $2`, HashSecret(secret), now,
	).Scan(&ses.ID, &ses.AccountID, &ses.TenantID, &ses.Email, &ses.CreatedAt, &ses.LastSeenAt, &ses.ExpiresAt, &ses.UserAgent, &ses.Method, &ses.IsOperator)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrNoSession
	}
	if err != nil {
		return Session{}, fmt.Errorf("auth: lookup session: %w", err)
	}
	if now.Sub(ses.LastSeenAt) >= refreshAfter {
		ses.ExpiresAt = now.Add(SessionTTL)
		ses.LastSeenAt = now
		ses.Refreshed = true
		if _, err := s.pool.Exec(ctx,
			`UPDATE account_sessions SET last_seen_at = $2, expires_at = $3 WHERE id = $1`, ses.ID, now, ses.ExpiresAt); err != nil {
			return Session{}, fmt.Errorf("auth: slide session: %w", err)
		}
	}
	return ses, nil
}

// ListSessions returns the account's live sessions, newest first.
func (s *Service) ListSessions(ctx context.Context, accountID string) ([]Session, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, created_at, last_seen_at, expires_at, user_agent
		FROM account_sessions WHERE account_id = $1 AND expires_at > $2
		ORDER BY created_at DESC`, accountID, s.now())
	if err != nil {
		return nil, fmt.Errorf("auth: list sessions: %w", err)
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		ses := Session{AccountID: accountID}
		if err := rows.Scan(&ses.ID, &ses.CreatedAt, &ses.LastSeenAt, &ses.ExpiresAt, &ses.UserAgent); err != nil {
			return nil, fmt.Errorf("auth: list sessions: %w", err)
		}
		out = append(out, ses)
	}
	return out, rows.Err()
}

// RevokeSession deletes one of the account's own sessions. An id that is not
// a uuid or belongs to another account revokes nothing and reports false.
func (s *Service) RevokeSession(ctx context.Context, accountID, sessionID string) (bool, error) {
	if _, err := uuid.Parse(sessionID); err != nil {
		return false, nil
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM account_sessions WHERE id = $1 AND account_id = $2`, sessionID, accountID)
	if err != nil {
		return false, fmt.Errorf("auth: revoke session: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// RevokeSecret deletes the session a cookie names (sign out).
func (s *Service) RevokeSecret(ctx context.Context, secret string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM account_sessions WHERE secret_hash = $1`, HashSecret(secret)); err != nil {
		return fmt.Errorf("auth: sign out: %w", err)
	}
	return nil
}
