package auth

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/lhaig/sanholo-cloud/internal/mail"
)

const (
	// LinkTTL is how long a sign-in link works.
	LinkTTL = 15 * time.Minute
	// SigninLimit is the magic-link requests allowed per email and per
	// client IP in SigninWindow.
	SigninLimit  = 5
	SigninWindow = time.Hour
)

var (
	ErrRateLimited = errors.New("too many sign-in requests")
	ErrLinkInvalid = errors.New("sign-in link is invalid, used or expired")
)

// RequestSignIn records a sign-in request. It does the same work whatever
// the address is: validate, count against both limits, insert one
// signin_requests row holding only the address hash. Whether an account
// exists is decided later, by ProcessSignInRequests, off the request path,
// so nothing on this path (rows, queries, mail) depends on it.
func (s *Service) RequestSignIn(ctx context.Context, email, clientIP string) error {
	email, err := ValidateEmail(email)
	if err != nil {
		return err
	}
	h := HashEmail(email)
	okEmail, err := s.Allow(ctx, "signin:email:"+h, SigninLimit, SigninWindow)
	if err != nil {
		return err
	}
	okIP, err := s.Allow(ctx, "signin:ip:"+clientIP, SigninLimit, SigninWindow)
	if err != nil {
		return err
	}
	if !okEmail || !okIP {
		return ErrRateLimited
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO signin_requests (email_hash, created_at) VALUES ($1, $2)`, h, s.now()); err != nil {
		return fmt.Errorf("auth: signin request: %w", err)
	}
	return nil
}

// ProcessSignInRequests turns pending requests into links and outbox mail.
// A request whose address has an account gets one single-use token (stored
// hashed) and one queued email; any other request is just marked done. A
// request older than LinkTTL is dropped. It returns how many links it made.
func (s *Service) ProcessSignInRequests(ctx context.Context) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("auth: process: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		SELECT id, email_hash, created_at FROM signin_requests
		WHERE processed_at IS NULL ORDER BY id LIMIT 20
		FOR UPDATE SKIP LOCKED`)
	if err != nil {
		return 0, fmt.Errorf("auth: process: select: %w", err)
	}
	type req struct {
		id      int64
		hash    string
		created time.Time
	}
	var reqs []req
	for rows.Next() {
		var r req
		if err := rows.Scan(&r.id, &r.hash, &r.created); err != nil {
			rows.Close()
			return 0, fmt.Errorf("auth: process: scan: %w", err)
		}
		reqs = append(reqs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("auth: process: rows: %w", err)
	}

	now := s.now()
	made := 0
	for _, r := range reqs {
		if _, err := tx.Exec(ctx, `UPDATE signin_requests SET processed_at = $2 WHERE id = $1`, r.id, now); err != nil {
			return made, fmt.Errorf("auth: process: mark: %w", err)
		}
		if now.Sub(r.created) >= LinkTTL {
			continue
		}
		var accountID, email string
		err := tx.QueryRow(ctx, `
			SELECT id::text, email FROM accounts WHERE email_hash = $1`, r.hash).Scan(&accountID, &email)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return made, fmt.Errorf("auth: process: account: %w", err)
		}
		token := NewSecret()
		expires := now.Add(LinkTTL)
		if _, err := tx.Exec(ctx,
			`INSERT INTO magic_links (token_hash, account_id, created_at, expires_at) VALUES ($1, $2, $3, $4)`,
			HashSecret(token), accountID, now, expires); err != nil {
			return made, fmt.Errorf("auth: process: link: %w", err)
		}
		if err := mail.Enqueue(ctx, tx, SigninMessage(email, s.opt.BaseURL+"/signin/verify?token="+token), expires); err != nil {
			return made, err
		}
		made++
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("auth: process: commit: %w", err)
	}
	return made, nil
}

// SigninMessage is the sign-in email, in the company register.
func SigninMessage(to, link string) mail.Message {
	return mail.Message{
		To:      to,
		Subject: "Your SanHolo sign-in link",
		Body: "TydeWhatMay sends this link because someone asked to sign in to SanHolo with this address.\n\n" +
			link + "\n\n" +
			"The link works once. It expires in 15 minutes.\n" +
			"If you did not ask for it, ignore this email. Nothing happens until the link is opened.\n",
	}
}

// ConsumeMagicLink spends a token and opens a session. The token is spent in
// the same transaction that creates the session, so it cannot be used twice
// and a failure creates nothing. It returns the plaintext session secret for
// the cookie; only its hash is stored.
func (s *Service) ConsumeMagicLink(ctx context.Context, token, userAgent string) (secret string, expires time.Time, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("auth: consume: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	now := s.now()
	var accountID string
	err = tx.QueryRow(ctx, `
		UPDATE magic_links SET used_at = $2
		WHERE token_hash = $1 AND used_at IS NULL AND expires_at > $2
		RETURNING account_id::text`, HashSecret(token), now).Scan(&accountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", time.Time{}, ErrLinkInvalid
	}
	if err != nil {
		return "", time.Time{}, fmt.Errorf("auth: consume: %w", err)
	}
	secret, expires, err = s.createSession(ctx, tx, accountID, userAgent, now)
	if err != nil {
		return "", time.Time{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", time.Time{}, fmt.Errorf("auth: consume: commit: %w", err)
	}
	return secret, expires, nil
}

// Cleanup removes spent and expired rows.
func (s *Service) Cleanup(ctx context.Context) error {
	now := s.now()
	for _, q := range []string{
		`DELETE FROM signin_requests WHERE processed_at < $1 - interval '1 day'`,
		`DELETE FROM magic_links WHERE expires_at < $1 - interval '1 day'`,
		`DELETE FROM account_sessions WHERE expires_at < $1`,
		`DELETE FROM rate_counters WHERE window_start < $1 - interval '2 days'`,
		`DELETE FROM mail_outbox WHERE (sent_at IS NOT NULL OR failed_at IS NOT NULL) AND created_at < $1 - interval '7 days'`,
	} {
		if _, err := s.pool.Exec(ctx, q, now); err != nil {
			return fmt.Errorf("auth: cleanup: %w", err)
		}
	}
	return nil
}

// Run processes sign-in requests every interval and cleans up hourly, until
// ctx ends.
func (s *Service) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	lastClean := time.Time{}
	for {
		if _, err := s.ProcessSignInRequests(ctx); err != nil && ctx.Err() == nil {
			log.Printf("auth worker: %v", err)
		}
		if time.Since(lastClean) > time.Hour {
			if err := s.Cleanup(ctx); err != nil && ctx.Err() == nil {
				log.Printf("auth worker: %v", err)
			}
			lastClean = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
