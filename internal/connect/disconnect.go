package connect

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/lhaig/sanholo-cloud/internal/audit"
	"github.com/lhaig/sanholo-cloud/internal/provision"
	"github.com/lhaig/sanholo-cloud/internal/secrets"
)

var (
	// ErrNotConnected: the tenant has no game account to disconnect.
	ErrNotConnected = errors.New("the tenant has no game account")
	// ErrStopFailed: Nomad did not stop the bot. Nothing was changed; the
	// customer can try again.
	ErrStopFailed = errors.New("the bot could not be stopped")
	// ErrLockTimeout: the bot is being started or changed right now and the
	// disconnect could not get the tenant's lock in time. Nothing was
	// changed; try again.
	ErrLockTimeout = errors.New("the bot is busy")
)

// Cleanup kinds (pending_cleanups.kind).
const (
	KindSecret = "secret"
	KindVolume = "volume"
)

// Fixed phrases for pending_cleanups.last_error. The remote system's own
// text is never stored.
const (
	cleanupDenied      = "the secrets store refused the deletion"
	cleanupUnavailable = "the system could not be reached"
	cleanupVolume      = "Nomad did not delete the volume"
)

// SetProvisioner gives the service the provisioner that Disconnect stops the
// bot through. Without one (or with provisioning disabled) there is no bot
// or volume to remove.
func (s *Service) SetProvisioner(p *provision.Service) { s.prov = p }

// Disconnected reports what a disconnect left to finish.
type Disconnected struct {
	// SecretPending: the token could not be deleted from the secrets store
	// yet (until qm ships its purge endpoint every deletion is refused). The
	// deletion is recorded and retried by RunCleanups.
	SecretPending bool
	// VolumePending: the same for the bot's data volume.
	VolumePending bool
}

// Disconnect removes the tenant's game account (US-012): the bot is stopped
// (without purge first, then purged), the game account record and the
// instance record are deleted, and the deletion of the token in qm and of the
// host volume is attempted. The tenant and the account stay.
//
// Order: the bot is stopped first, so that a failure there changes nothing
// and the customer can repeat the request. Then one transaction deletes the
// rows, writes the audit row and records the two remaining deletions as
// pending; only then are they attempted. A deletion that fails stays pending
// (a worker retries it), so a crash or a refusal never loses it. The qm purge
// would also remove a token written meanwhile, so a connect that happens
// while a deletion is pending cancels it (store).
func (s *Service) Disconnect(ctx context.Context, accountID, tenantID string) (Disconnected, error) {
	var out Disconnected
	var one int
	switch err := s.pool.QueryRow(ctx, `SELECT 1 FROM game_accounts WHERE tenant_id = $1`, tenantID).Scan(&one); {
	case errors.Is(err, pgx.ErrNoRows):
		return out, ErrNotConnected
	case err != nil:
		return out, fmt.Errorf("connect: disconnect: %w", err)
	}
	// The tenant's lock is held from the stop to the commit of the rows being
	// deleted: a start that passed its checks before this disconnect must not
	// register the job after the stop, nor recreate the instance row after it
	// is deleted. Lock order: this lock, then the tenants row.
	unlock := func() {}
	if s.prov.Enabled() {
		lctx, cancel := context.WithTimeout(ctx, s.lockWait)
		u, err := s.prov.LockTenant(lctx, tenantID)
		cancel()
		if errors.Is(err, context.DeadlineExceeded) {
			return out, ErrLockTimeout
		}
		if err != nil {
			s.log.Warn("connect: could not lock the tenant", "tenant", tenantID, "error", err)
			return out, ErrStopFailed
		}
		unlock = u
		defer func() { unlock() }()
		for _, purge := range []bool{false, true} {
			if err := s.prov.StopLocked(ctx, tenantID, purge); err != nil {
				s.log.Warn("connect: could not stop the bot", "tenant", tenantID, "purge", purge, "error", err)
				return out, ErrStopFailed
			}
		}
	}
	wantSecret, wantVolume := s.sec.Enabled(), s.prov.Enabled()
	now := s.now()
	err := func() error {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("connect: disconnect: %w", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := tx.QueryRow(ctx, `SELECT 1 FROM tenants WHERE id = $1 FOR UPDATE`, tenantID).Scan(&one); err != nil {
			return fmt.Errorf("connect: disconnect: %w", err)
		}
		tag, err := tx.Exec(ctx, `DELETE FROM game_accounts WHERE tenant_id = $1`, tenantID)
		if err != nil {
			return fmt.Errorf("connect: disconnect: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return ErrNotConnected // a concurrent disconnect won
		}
		if _, err := tx.Exec(ctx, `DELETE FROM tenant_instances WHERE tenant_id = $1`, tenantID); err != nil {
			return fmt.Errorf("connect: disconnect: %w", err)
		}
		for kind, want := range map[string]bool{KindSecret: wantSecret, KindVolume: wantVolume} {
			if !want {
				continue
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO pending_cleanups (tenant_key, kind, requested_at) VALUES ($1, $2, $3)
				ON CONFLICT (tenant_key, kind) DO UPDATE SET requested_at = EXCLUDED.requested_at, attempts = 0, last_attempt_at = NULL, last_error = ''`,
				tenantID, kind, now); err != nil {
				return fmt.Errorf("connect: disconnect: %w", err)
			}
		}
		if err := audit.Write(ctx, tx, now, accountID, "game.disconnect", "tenant:"+tenantID, nil); err != nil {
			return fmt.Errorf("connect: disconnect: %w", err)
		}
		return tx.Commit(ctx)
	}()
	if err != nil {
		return out, err
	}
	unlock()
	unlock = func() {}
	s.log.Info("connect: game account disconnected", "tenant", tenantID)
	if wantVolume {
		out.VolumePending = !s.cleanup(ctx, tenantID, KindVolume)
	}
	if wantSecret {
		out.SecretPending = !s.cleanup(ctx, tenantID, KindSecret)
	}
	return out, nil
}

// cleanupDue is the SQL condition (with the clock as $2) for a pending
// deletion to be attempted now: a deletion that keeps failing backs off, one
// minute after the first failure, doubling, never more than six hours between
// attempts.
const cleanupDue = `(last_attempt_at IS NULL OR last_attempt_at + LEAST(interval '6 hours', interval '1 minute' * power(2, LEAST(attempts - 1, 10))) <= $2)`

// stuckThresholds are the attempt counts at which a deletion that keeps
// failing is logged at error level once, so an operator sees it.
var stuckThresholds = map[int]bool{5: true, 10: true, 25: true, 50: true, 100: true}

// cleanup attempts one pending deletion and reports whether it is done. A
// failure is logged (the errors carry no secret) and recorded with a fixed
// phrase.
//
// It runs in a transaction that holds the tenant's row lock from the check
// to the end of the remote call. A connect for the tenant takes the same row
// lock first, so a late purge cannot delete the token a connect is about to
// write (or has just written): either it purges first and the connect writes
// afterwards, or the connect commits first and the check below sees the game
// account. Lock order everywhere: the tenant's provisioning lock (if any),
// then this row; connect.store, Disconnect and this function all take the row
// before anything else in the database, so none can wait on another in a cycle.
func (s *Service) cleanup(ctx context.Context, tenantID, kind string) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.log.Error("connect: cleanup", "tenant", tenantID, "kind", kind, "error", err)
		return false
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// No tenant row (the tenant is gone) is fine: nothing can connect to it.
	var one int
	if err := tx.QueryRow(ctx, `SELECT 1 FROM tenants WHERE id = $1 FOR UPDATE`, tenantID).Scan(&one); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		s.log.Error("connect: cleanup", "tenant", tenantID, "kind", kind, "error", err)
		return false
	}
	var pending, connected bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM pending_cleanups WHERE tenant_key = $1 AND kind = $2),
		       EXISTS (SELECT 1 FROM game_accounts WHERE tenant_id = $1)`, tenantID, kind).Scan(&pending, &connected); err != nil {
		s.log.Error("connect: cleanup", "tenant", tenantID, "kind", kind, "error", err)
		return false
	}
	switch {
	case !pending:
		return true // someone finished it
	case connected && kind == KindSecret:
		// Connected again: the old token is replaced and a later disconnect
		// purges everything.
		return s.finishCleanup(ctx, tx, tenantID, kind)
	case connected:
		// The new connection's start deletes the old volume before it makes
		// a new one (provision.register); leave it to that.
		return false
	}
	phrase := cleanupUnavailable
	switch kind {
	case KindSecret:
		err = s.sec.PurgeTenant(ctx, tenantID)
		if isDenied(err) {
			phrase = cleanupDenied
		}
	case KindVolume:
		err = s.prov.DeleteVolume(ctx, tenantID)
		phrase = cleanupVolume
	}
	if err != nil {
		var attempts int
		if uerr := tx.QueryRow(ctx, `
			UPDATE pending_cleanups SET attempts = attempts + 1, last_attempt_at = $3, last_error = $4
			WHERE tenant_key = $1 AND kind = $2 RETURNING attempts`, tenantID, kind, s.now(), phrase).Scan(&attempts); uerr != nil {
			s.log.Error("connect: could not record a failed deletion", "tenant", tenantID, "kind", kind, "error", uerr)
			return false
		}
		if cerr := tx.Commit(ctx); cerr != nil {
			s.log.Error("connect: could not record a failed deletion", "tenant", tenantID, "kind", kind, "error", cerr)
			return false
		}
		if stuckThresholds[attempts] {
			s.log.Error("connect: a deletion is stuck", "tenant", tenantID, "kind", kind, "attempts", attempts, "reason", phrase, "error", err)
		} else {
			s.log.Warn("connect: a deletion is still pending", "tenant", tenantID, "kind", kind, "attempts", attempts, "error", err)
		}
		return false
	}
	return s.finishCleanup(ctx, tx, tenantID, kind)
}

func (s *Service) finishCleanup(ctx context.Context, tx pgx.Tx, tenantID, kind string) bool {
	if _, err := tx.Exec(ctx, `DELETE FROM pending_cleanups WHERE tenant_key = $1 AND kind = $2`, tenantID, kind); err != nil {
		s.log.Error("connect: could not clear a finished deletion", "tenant", tenantID, "kind", kind, "error", err)
		return false
	}
	if err := tx.Commit(ctx); err != nil {
		s.log.Error("connect: could not clear a finished deletion", "tenant", tenantID, "kind", kind, "error", err)
		return false
	}
	s.log.Info("connect: deletion finished", "tenant", tenantID, "kind", kind)
	return true
}

// RetryCleanups attempts the pending deletions that are due, each kind
// independently (a pile of stuck secret purges must not starve the volumes),
// and returns how many finished. A failing deletion backs off, see
// cleanupDue.
func (s *Service) RetryCleanups(ctx context.Context) int {
	done := 0
	for _, kind := range []string{KindSecret, KindVolume} {
		if (kind == KindSecret && !s.sec.Enabled()) || (kind == KindVolume && !s.prov.Enabled()) {
			continue
		}
		rows, err := s.pool.Query(ctx, `
			SELECT tenant_key FROM pending_cleanups WHERE kind = $1 AND `+cleanupDue+`
			  AND NOT (kind = 'volume' AND EXISTS (SELECT 1 FROM game_accounts g WHERE g.tenant_id = tenant_key))
			ORDER BY last_attempt_at NULLS FIRST, requested_at, tenant_key LIMIT 100`, kind, s.now())
		if err != nil {
			s.log.Error("connect: pending deletions", "kind", kind, "error", err)
			continue
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
				return done
			}
			if s.cleanup(ctx, t, kind) {
				done++
			}
		}
	}
	return done
}

// RunCleanups retries the pending deletions each interval until ctx ends.
func (s *Service) RunCleanups(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.RetryCleanups(ctx)
		}
	}
}

// Pending lists the deletions still to finish for a tenant.
func (s *Service) Pending(ctx context.Context, tenantID string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT kind FROM pending_cleanups WHERE tenant_key = $1 ORDER BY kind`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("connect: pending: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, fmt.Errorf("connect: pending: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func isDenied(err error) bool { return errors.Is(err, secrets.ErrDenied) }
