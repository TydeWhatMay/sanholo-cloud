package connect

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/lhaig/sanholo-cloud/internal/db"
)

// Verification states (tenant_deletions.check_state).
const (
	CheckPassed  = "passed"
	CheckPending = "pending"
	CheckFailed  = "failed"
)

// Fixed phrases of a verification. They are stored in
// tenant_deletions.check_detail and shown to the operator; none carries the
// remote system's own text.
const (
	detailQMPending     = "qm purge pending"
	detailVolumePending = "volume deletion pending"
	detailQMUnreadable  = "qm could not be read"
	detailNomadRead     = "Nomad could not be read"
	detailQMPresent     = "the token is still in qm"
	detailJobPresent    = "the Nomad job still exists"
	detailVolumePresent = "the Nomad volume still exists"
)

// Check is the result of one verification.
type Check struct {
	State string
	// Detail lists what was found, in fixed phrases and table names.
	Detail []string
}

// deletedIdentity is what only the request that deleted the account knows:
// the address and its hash (not kept anywhere afterwards), the outbox row of
// the confirmation, and the moment of the deletion.
type deletedIdentity struct {
	email         string
	emailHash     string
	confirmMailID int64
	cutoff        time.Time
}

// verifyDeletion is the check job (US-018). It looks for anything of the
// tenant that should be gone, records the result in tenant_deletions and,
// when everything is gone, removes the tombstone. It runs once when a
// deletion ends and again on every cleanup pass until the tombstone is
// removed.
//
//   - every table the schema gives a tenant_id column (db.TenantTables) has
//     no row for the tenant;
//   - no table with an account_id column (db.AccountTables) has a row for the
//     deleted account id, and no rate-limit row is keyed by it or by the
//     tenant id (the push and alert-intake limits);
//   - right after the deletion, when the address is still known: nothing in
//     the waiting list, the invites, the outbox (the confirmation excepted),
//     the sign-in requests or the accounts is keyed by the address or its
//     hash. Later passes cannot look for it: the address is not kept;
//   - qm holds no version of the tenant's game secret. While a purge is
//     recorded as pending (the purge contract is final, ADR 0024, but until
//     the route is deployed on the production qm it is refused with 403)
//     the check reports it as pending instead, and qm is not asked;
//   - Nomad has neither the job nor the volume.
//
// A leftover that a cleanup can remove is queued again (the secret, the
// volume) or stopped again (the job), so the check also repairs.
func (s *Service) verifyDeletion(ctx context.Context, tenantID string, who *deletedIdentity) Check {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	var failed, pending []string
	read := func(what string, err error) {
		s.log.Error("connect: verification could not read", "tenant", tenantID, "what", what, "error", err)
		pending = append(pending, "could not read "+what)
	}

	tables, err := db.TenantTables(ctx, s.pool)
	if err != nil {
		read("the schema", err)
	}
	for _, t := range tables {
		n, err := s.count(ctx, `SELECT count(*) FROM `+pgx.Identifier{t}.Sanitize()+` WHERE tenant_id = $1`, tenantID)
		switch {
		case err != nil:
			read(t, err)
		case n > 0:
			failed = append(failed, "rows remain in "+t)
		}
	}

	var accountID *string
	switch err := s.pool.QueryRow(ctx, `SELECT deleted_account::text FROM tenant_deletions WHERE tenant_key = $1`, tenantID).Scan(&accountID); {
	case errors.Is(err, pgx.ErrNoRows):
		return Check{State: CheckFailed, Detail: []string{"the deletion is not recorded"}}
	case err != nil:
		read("the deletion record", err)
	}
	if accountID != nil {
		owned, err := db.AccountTables(ctx, s.pool)
		if err != nil {
			read("the schema", err)
		}
		for _, t := range owned {
			n, err := s.count(ctx, `SELECT count(*) FROM `+pgx.Identifier{t}.Sanitize()+` WHERE account_id = $1`, *accountID)
			switch {
			case err != nil:
				read(t, err)
			case n > 0:
				failed = append(failed, "rows remain in "+t)
			}
		}
		if n, err := s.count(ctx, `SELECT count(*) FROM rate_counters WHERE right(key, 36) = $1`, *accountID); err != nil {
			read("rate_counters", err)
		} else if n > 0 {
			failed = append(failed, "rows remain in rate_counters")
		}
	}
	if n, err := s.count(ctx, `SELECT count(*) FROM rate_counters WHERE key IN ('push:tenant:' || $1, 'alerts:tenant:' || $1)`, tenantID); err != nil {
		read("rate_counters", err)
	} else if n > 0 {
		failed = append(failed, "rows remain in rate_counters")
	}
	if who != nil {
		failed = append(failed, s.addressLeftovers(ctx, who)...)
	}

	pendingSecret, pendingVolume, err := s.pendingKinds(ctx, tenantID)
	if err != nil {
		read("pending deletions", err)
	}
	if s.sec.Enabled() {
		if pendingSecret {
			pending = append(pending, detailQMPending)
		} else if v, err := s.sec.Version(ctx, tenantID); err != nil {
			s.log.Warn("connect: verification could not read qm", "tenant", tenantID, "error", err)
			pending = append(pending, detailQMUnreadable)
		} else if v > 0 {
			failed = append(failed, detailQMPresent)
			s.requeue(ctx, tenantID, KindSecret)
		}
	}
	if s.prov.Enabled() {
		job, volume, err := s.prov.Leftovers(ctx, tenantID)
		switch {
		case err != nil:
			s.log.Warn("connect: verification could not read Nomad", "tenant", tenantID, "error", err)
			pending = append(pending, detailNomadRead)
		default:
			if job {
				failed = append(failed, detailJobPresent)
				if err := s.prov.Stop(ctx, tenantID, true); err != nil {
					s.log.Warn("connect: could not stop a leftover job", "tenant", tenantID, "error", err)
				}
			}
			if volume && pendingVolume {
				pending = append(pending, detailVolumePending)
			} else if volume {
				failed = append(failed, detailVolumePresent)
				s.requeue(ctx, tenantID, KindVolume)
			}
		}
	}

	res := Check{State: CheckPassed}
	switch {
	case len(failed) > 0:
		res = Check{State: CheckFailed, Detail: append(failed, pending...)}
	case len(pending) > 0:
		res = Check{State: CheckPending, Detail: pending}
	}
	if _, err := s.pool.Exec(ctx, `
		UPDATE tenant_deletions SET checked_at = $2, check_state = $3, check_detail = $4 WHERE tenant_key = $1`,
		tenantID, s.now(), res.State, strings.Join(res.Detail, "; ")); err != nil {
		s.log.Error("connect: could not record a verification", "tenant", tenantID, "error", err)
	}
	if res.State == CheckPassed {
		s.removeTombstone(ctx, tenantID)
	}
	s.log.Info("connect: deletion verified", "tenant", tenantID, "state", res.State, "detail", strings.Join(res.Detail, "; "))
	return res
}

func (s *Service) count(ctx context.Context, q string, args ...any) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, q, args...).Scan(&n)
	return n, err
}

// addressLeftovers looks for rows keyed by the address or its hash. The
// sign-in request and rate-limit checks only count rows from before the
// deletion: a stranger asking for a link for the same address afterwards is
// not the deleted account's data.
func (s *Service) addressLeftovers(ctx context.Context, who *deletedIdentity) []string {
	var failed []string
	for _, c := range []struct {
		table, sql string
		args       []any
	}{
		{"waitlist", `SELECT count(*) FROM waitlist WHERE lower(email) = lower($1)`, []any{who.email}},
		{"invites", `SELECT count(*) FROM invites WHERE lower(email) = lower($1)`, []any{who.email}},
		{"mail_outbox", `SELECT count(*) FROM mail_outbox WHERE lower(to_email) = lower($1) AND id <> $2`, []any{who.email, who.confirmMailID}},
		{"accounts", `SELECT count(*) FROM accounts WHERE email_hash = $1`, []any{who.emailHash}},
		{"signin_requests", `SELECT count(*) FROM signin_requests WHERE email_hash = $1 AND created_at <= $2`, []any{who.emailHash, who.cutoff}},
		{"rate_counters", `SELECT count(*) FROM rate_counters WHERE key = 'signin:email:' || $1 AND window_start <= $2`, []any{who.emailHash, who.cutoff}},
	} {
		n, err := s.count(ctx, c.sql, c.args...)
		if err != nil {
			failed = append(failed, "could not read "+c.table)
		} else if n > 0 {
			failed = append(failed, "rows remain in "+c.table)
		}
	}
	return failed
}

func (s *Service) pendingKinds(ctx context.Context, tenantID string) (secret, volume bool, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM pending_cleanups WHERE tenant_key = $1 AND kind = 'secret'),
		       EXISTS (SELECT 1 FROM pending_cleanups WHERE tenant_key = $1 AND kind = 'volume')`, tenantID).Scan(&secret, &volume)
	return secret, volume, err
}

// requeue records a deletion again that a verification found undone.
func (s *Service) requeue(ctx context.Context, tenantID, kind string) {
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO pending_cleanups (tenant_key, kind, requested_at) VALUES ($1, $2, $3)
		ON CONFLICT (tenant_key, kind) DO NOTHING`, tenantID, kind, s.now()); err != nil {
		s.log.Error("connect: could not queue a deletion", "tenant", tenantID, "kind", kind, "error", err)
	}
}

// removeTombstone deletes the tenants row of a verified deletion, under the
// row lock and after counting again: removing the row would cascade to
// anything still referencing it, so it goes only when nothing does.
func (s *Service) removeTombstone(ctx context.Context, tenantID string) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.log.Error("connect: tombstone", "tenant", tenantID, "error", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var status string
	switch err := tx.QueryRow(ctx, `SELECT status FROM tenants WHERE id = $1 FOR UPDATE`, tenantID).Scan(&status); {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		s.log.Error("connect: tombstone", "tenant", tenantID, "error", err)
		return
	case status != "deleting":
		return
	}
	var pending bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pending_cleanups WHERE tenant_key = $1)`, tenantID).Scan(&pending); err != nil || pending {
		return
	}
	tables, err := db.TenantTables(ctx, s.pool)
	if err != nil {
		return
	}
	for _, t := range tables {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{t}.Sanitize()+` WHERE tenant_id = $1`, tenantID).Scan(&n); err != nil || n > 0 {
			return
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM tenants WHERE id = $1 AND status = 'deleting'`, tenantID); err != nil {
		s.log.Error("connect: tombstone", "tenant", tenantID, "error", err)
		return
	}
	if _, err := tx.Exec(ctx, `UPDATE tenant_deletions SET completed_at = $2, deleted_account = NULL WHERE tenant_key = $1`, tenantID, s.now()); err != nil {
		s.log.Error("connect: tombstone", "tenant", tenantID, "error", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.log.Error("connect: tombstone", "tenant", tenantID, "error", err)
	}
}

// verifyDeletions runs the check job again for every deletion whose account
// is gone and whose tombstone is still there.
func (s *Service) verifyDeletions(ctx context.Context) {
	rows, err := s.pool.Query(ctx, `
		SELECT d.tenant_key FROM tenant_deletions d
		WHERE d.completed_at IS NULL AND NOT EXISTS (SELECT 1 FROM accounts a WHERE a.tenant_id = d.tenant_key)
		ORDER BY d.requested_at, d.tenant_key LIMIT 50`)
	if err != nil {
		s.log.Error("connect: pending verifications", "error", err)
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
		s.verifyDeletion(ctx, t, nil)
	}
}
