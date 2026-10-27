package connect

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/lhaig/sanholo-cloud/internal/audit"
	"github.com/lhaig/sanholo-cloud/internal/db"
	"github.com/lhaig/sanholo-cloud/internal/deletionlog"
	"github.com/lhaig/sanholo-cloud/internal/mail"
)

var (
	// ErrEmailMismatch: the address typed to confirm is not the account's.
	// Nothing was changed.
	ErrEmailMismatch = errors.New("the address does not match the account")
	// ErrNoAccount: the tenant has no account to delete (already deleted, or
	// the id is not the caller's).
	ErrNoAccount = errors.New("there is no account to delete")
	// ErrLog: the deletion could not be written to the deletion log. The
	// tenant is marked deleting and nothing else was removed; the request
	// can be repeated and a background pass repeats it too.
	ErrLog = errors.New("the deletion could not be recorded")
)

// Actors of a deletion in the audit log: the account itself (its id), the
// background pass that finishes a deletion that stopped half way, and the
// command line (`deletions reapply`).
const (
	ActorSystem = "system"
	ActorCLI    = "cli"
)

// resumeAfter is how long a deletion that stopped half way is left alone
// before the background pass finishes it, so the pass never races the
// request that started it.
const resumeAfter = 5 * time.Minute

// confirmationTTL bounds how long the confirmation email may wait for
// delivery. The outbox then fails it (erasing its text) and the existing
// outbox retention deletes the row, address included.
const confirmationTTL = 24 * time.Hour

// SetDeletionLog gives the service the log that account deletions are
// recorded in (see package deletionlog).
func (s *Service) SetDeletionLog(l *deletionlog.Log) { s.dlog = l }

// Deleted reports what a deletion left to finish.
type Deleted struct {
	// SecretPending: the token is still in qm (until qm ships its purge
	// endpoint every purge is refused). Recorded and retried.
	SecretPending bool
	// VolumePending: the same for the bot's data volume.
	VolumePending bool
	// Check is the first verification, run when the deletion ended.
	Check Check
}

// DeleteAccount deletes the account of tenantID (US-018) after the address
// typed by the customer was compared with the account's. See deleteTenant.
func (s *Service) DeleteAccount(ctx context.Context, accountID, tenantID, typedEmail string) (Deleted, error) {
	a, err := s.accountOf(ctx, tenantID)
	if err != nil {
		return Deleted{}, err
	}
	if a.id != accountID {
		return Deleted{}, ErrNoAccount
	}
	if !strings.EqualFold(strings.TrimSpace(typedEmail), a.email) {
		return Deleted{}, ErrEmailMismatch
	}
	return s.deleteTenant(ctx, tenantID, accountID, true)
}

// DeleteTenant deletes the tenant's account on behalf of the operator or the
// background pass (actor ActorCLI or ActorSystem), without confirmation mail.
// Used by `deletions reapply` after a restore. ErrNoAccount when there is
// nothing left to delete.
func (s *Service) DeleteTenant(ctx context.Context, actor, tenantID string) (Deleted, error) {
	return s.deleteTenant(ctx, tenantID, actor, false)
}

type accountRef struct{ id, email, emailHash string }

func (s *Service) accountOf(ctx context.Context, tenantID string) (accountRef, error) {
	var a accountRef
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, email, email_hash FROM accounts WHERE tenant_id = $1 ORDER BY created_at, id LIMIT 1`, tenantID,
	).Scan(&a.id, &a.email, &a.emailHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ErrNoAccount
	}
	if err != nil {
		return a, fmt.Errorf("connect: delete: %w", err)
	}
	return a, nil
}

// deleteTenant is the account deletion. Order, which reuses the disconnect's:
//
//  1. take the tenant's provisioning lock (nothing may start the bot, change
//     its job or connect a game account meanwhile);
//  2. one transaction marks the tenant 'deleting' (Bridge access and every
//     start end here), records the deletion and audits it;
//  3. the deletion is written to the deletion log (restore hook);
//  4. the bot is stopped, then purged from Nomad. A failure leaves the
//     tenant 'deleting' with everything else intact: repeating the request
//     or the background pass (resumeDeletions) finishes it;
//  5. one transaction deletes every row of the tenant and the account
//     (tables found from the schema), the rows keyed by the address, records
//     the qm and volume deletions as pending and queues the confirmation;
//  6. the lock is released, the volume and then the qm deletion are
//     attempted (pending_cleanups with backoff when they cannot complete),
//     and the check job verifies the result.
//
// What stays is the tombstone: the tenants row (random id, status
// 'deleting', creation date) and a tenant_deletions row, until every cleanup
// has finished and a verification has passed. Neither holds personal data.
func (s *Service) deleteTenant(ctx context.Context, tenantID, actor string, notify bool) (Deleted, error) {
	var out Deleted
	a, err := s.accountOf(ctx, tenantID)
	if err != nil {
		return out, err
	}
	unlock, err := s.lockTenant(ctx, tenantID)
	if err != nil {
		return out, err
	}
	defer func() { unlock() }()

	now := s.now()
	if err := s.markDeleting(ctx, tenantID, a.id, actor, now); err != nil {
		return out, err
	}
	s.pend.dropAccount(a.id)
	if err := s.dlog.Append(deletionlog.Entry{At: now.UTC(), Tenant: tenantID, Account: a.id}); err != nil {
		s.log.Error("connect: could not write the deletion log", "tenant", tenantID, "error", err)
		return out, ErrLog
	}
	if s.prov.Enabled() {
		for _, purge := range []bool{false, true} {
			if err := s.prov.StopLocked(ctx, tenantID, purge); err != nil {
				s.log.Warn("connect: could not stop the bot for a deletion", "tenant", tenantID, "purge", purge, "error", err)
				return out, ErrStopFailed
			}
		}
	}
	confirmID, err := s.deleteRows(ctx, tenantID, a, notify, now)
	if err != nil {
		return out, err
	}
	unlock()
	unlock = func() {}
	s.log.Info("connect: account deleted", "tenant", tenantID)

	wantSecret, wantVolume := s.sec.Enabled(), s.prov.Enabled()
	if wantVolume {
		out.VolumePending = !s.cleanup(ctx, tenantID, KindVolume)
	}
	if wantSecret {
		out.SecretPending = !s.cleanup(ctx, tenantID, KindSecret)
	}
	out.Check = s.verifyDeletion(ctx, tenantID, &deletedIdentity{email: a.email, emailHash: a.emailHash, confirmMailID: confirmID, cutoff: now})
	return out, nil
}

// markDeleting is step 2. A repeat (the tenant is already 'deleting') changes
// nothing and writes no second audit row.
func (s *Service) markDeleting(ctx context.Context, tenantID, accountID, actor string, now time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("connect: delete: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var status string
	switch err := tx.QueryRow(ctx, `SELECT status FROM tenants WHERE id = $1 FOR UPDATE`, tenantID).Scan(&status); {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrNoAccount
	case err != nil:
		return fmt.Errorf("connect: delete: %w", err)
	}
	var one int
	switch err := tx.QueryRow(ctx, `SELECT 1 FROM accounts WHERE id = $1 AND tenant_id = $2`, accountID, tenantID).Scan(&one); {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrNoAccount // deleted meanwhile
	case err != nil:
		return fmt.Errorf("connect: delete: %w", err)
	}
	if status != "deleting" {
		if _, err := tx.Exec(ctx,
			`UPDATE tenants SET status = 'deleting', status_before_suspend = NULL, is_canary = false WHERE id = $1`, tenantID); err != nil {
			return fmt.Errorf("connect: delete: %w", err)
		}
		if err := audit.Write(ctx, tx, now, actor, "account.delete", "tenant:"+tenantID, nil); err != nil {
			return fmt.Errorf("connect: delete: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO tenant_deletions (tenant_key, deleted_account, requested_at) VALUES ($1, $2, $3)
		ON CONFLICT (tenant_key) DO UPDATE SET deleted_account = EXCLUDED.deleted_account
		WHERE tenant_deletions.completed_at IS NULL`, tenantID, accountID, now); err != nil {
		return fmt.Errorf("connect: delete: %w", err)
	}
	return tx.Commit(ctx)
}

// deleteRows is step 5. It returns the outbox id of the confirmation (0 when
// none is queued).
func (s *Service) deleteRows(ctx context.Context, tenantID string, a accountRef, notify bool, now time.Time) (int64, error) {
	// The tables with a tenant_id column come from the schema, so a table
	// added later is deleted (and verified) without touching this function.
	tables, err := db.TenantTables(ctx, s.pool)
	if err != nil {
		return 0, err
	}
	owned, err := db.AccountTables(ctx, s.pool)
	if err != nil {
		return 0, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("connect: delete: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM tenants WHERE id = $1 FOR UPDATE`, tenantID).Scan(&status); err != nil {
		return 0, fmt.Errorf("connect: delete: %w", err)
	}
	var still bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM accounts WHERE id = $1)`, a.id).Scan(&still); err != nil {
		return 0, fmt.Errorf("connect: delete: %w", err)
	}
	if status != "deleting" || !still {
		return 0, ErrNoAccount // a concurrent deletion finished first
	}
	for _, t := range tables {
		if _, err := tx.Exec(ctx, `DELETE FROM `+pgx.Identifier{t}.Sanitize()+` WHERE tenant_id = $1`, tenantID); err != nil {
			return 0, fmt.Errorf("connect: delete %s: %w", t, err)
		}
	}
	for _, t := range owned {
		if _, err := tx.Exec(ctx, `DELETE FROM `+pgx.Identifier{t}.Sanitize()+` WHERE account_id = $1`, a.id); err != nil {
			return 0, fmt.Errorf("connect: delete %s: %w", t, err)
		}
	}
	for _, q := range []struct{ sql, arg string }{
		{`DELETE FROM waitlist WHERE lower(email) = lower($1)`, a.email},
		{`DELETE FROM invites WHERE lower(email) = lower($1)`, a.email},
		{`DELETE FROM mail_outbox WHERE lower(to_email) = lower($1)`, a.email},
		{`DELETE FROM signin_requests WHERE email_hash = $1`, a.emailHash},
		{`DELETE FROM rate_counters WHERE key = 'signin:email:' || $1`, a.emailHash},
		{`DELETE FROM rate_counters WHERE right(key, 36) = $1`, a.id},
	} {
		if _, err := tx.Exec(ctx, q.sql, q.arg); err != nil {
			return 0, fmt.Errorf("connect: delete: %w", err)
		}
	}
	for kind, want := range map[string]bool{KindSecret: s.sec.Enabled(), KindVolume: s.prov.Enabled()} {
		if !want {
			continue
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO pending_cleanups (tenant_key, kind, requested_at) VALUES ($1, $2, $3)
			ON CONFLICT (tenant_key, kind) DO UPDATE SET requested_at = EXCLUDED.requested_at, attempts = 0, last_attempt_at = NULL, last_error = ''`,
			tenantID, kind, now); err != nil {
			return 0, fmt.Errorf("connect: delete: %w", err)
		}
	}
	var mailID int64
	if notify {
		// Queued after the address's other mail was deleted above. The row
		// holds the address, so it expires after a day (the outbox then fails
		// it and erases its text) and the outbox retention deletes the row
		// itself: auth.Cleanup removes every sent or failed row 7 days after
		// it was created.
		if mailID, err = mail.EnqueueID(ctx, tx, DeletionMessage(a.email), now.Add(confirmationTTL)); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("connect: delete: %w", err)
	}
	return mailID, nil
}

// DeletionMessage is the confirmation email, in the company register.
func DeletionMessage(to string) mail.Message {
	return mail.Message{
		To:      to,
		Subject: "Your SanHolo account is deleted",
		Body: "TydeWhatMay confirms that your SanHolo account is deleted.\n\n" +
			"SanHolo stopped your bot and deleted your account, sessions, passkeys, notification subscriptions and export links from its database.\n" +
			"SanHolo deletes your game token from the secrets store and your bot's data volume. A deletion that cannot finish at once is recorded and retried until it succeeds.\n" +
			"Your Replicant Space account is not deleted. Delete it in the game if you want it gone.\n" +
			"Encrypted backups made before today may still hold your data. They expire within 30 days. A deleted account is not restored from a backup.\n",
	}
}

// resumeDeletions finishes the deletions that stopped half way: a tenant
// that is 'deleting' and still has its account, because the bot could not be
// stopped or the deletion log could not be written. The customer confirmed
// the deletion already.
func (s *Service) resumeDeletions(ctx context.Context) {
	rows, err := s.pool.Query(ctx, `
		SELECT d.tenant_key FROM tenant_deletions d JOIN tenants t ON t.id = d.tenant_key
		WHERE d.completed_at IS NULL AND t.status = 'deleting' AND d.requested_at <= $1
		  AND EXISTS (SELECT 1 FROM accounts a WHERE a.tenant_id = d.tenant_key)
		ORDER BY d.requested_at, d.tenant_key LIMIT 20`, s.now().Add(-resumeAfter))
	if err != nil {
		s.log.Error("connect: pending deletions", "error", err)
		return
	}
	var tenants []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err == nil {
			tenants = append(tenants, t)
		}
	}
	rows.Close()
	for _, t := range tenants {
		if ctx.Err() != nil {
			return
		}
		if _, err := s.deleteTenant(ctx, t, ActorSystem, true); err != nil && !errors.Is(err, ErrNoAccount) {
			s.log.Warn("connect: could not finish a deletion", "tenant", t, "error", err)
		}
	}
}
