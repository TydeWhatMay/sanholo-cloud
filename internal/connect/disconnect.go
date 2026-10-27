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
	if s.prov.Enabled() {
		for _, purge := range []bool{false, true} {
			if err := s.prov.Stop(ctx, tenantID, purge); err != nil {
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
				ON CONFLICT (tenant_key, kind) DO UPDATE SET requested_at = EXCLUDED.requested_at, attempts = 0, last_error = ''`,
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
	s.log.Info("connect: game account disconnected", "tenant", tenantID)
	if wantVolume {
		out.VolumePending = !s.cleanup(ctx, tenantID, KindVolume)
	}
	if wantSecret {
		out.SecretPending = !s.cleanup(ctx, tenantID, KindSecret)
	}
	return out, nil
}

// cleanup attempts one pending deletion and reports whether it is done. A
// failure is logged (the errors carry no secret) and recorded with a fixed
// phrase.
func (s *Service) cleanup(ctx context.Context, tenantID, kind string) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	// A tenant that connected again no longer wants its old data removed.
	var connected bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM game_accounts WHERE tenant_id = $1)`, tenantID).Scan(&connected); err != nil {
		s.log.Error("connect: cleanup", "tenant", tenantID, "kind", kind, "error", err)
		return false
	}
	var err error
	phrase := cleanupUnavailable
	switch {
	case connected:
		_, _ = s.pool.Exec(ctx, `DELETE FROM pending_cleanups WHERE tenant_key = $1 AND kind = $2`, tenantID, kind)
		return true
	case kind == KindSecret:
		err = s.sec.PurgeTenant(ctx, tenantID)
		if isDenied(err) {
			phrase = cleanupDenied
		}
	case kind == KindVolume:
		err = s.prov.DeleteVolume(ctx, tenantID)
		phrase = cleanupVolume
	}
	if err != nil {
		s.log.Warn("connect: a deletion is still pending", "tenant", tenantID, "kind", kind, "error", err)
		if _, uerr := s.pool.Exec(ctx, `
			UPDATE pending_cleanups SET attempts = attempts + 1, last_attempt_at = $3, last_error = $4
			WHERE tenant_key = $1 AND kind = $2`, tenantID, kind, s.now(), phrase); uerr != nil {
			s.log.Error("connect: could not record a failed deletion", "tenant", tenantID, "kind", kind, "error", uerr)
		}
		return false
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM pending_cleanups WHERE tenant_key = $1 AND kind = $2`, tenantID, kind); err != nil {
		s.log.Error("connect: could not clear a finished deletion", "tenant", tenantID, "kind", kind, "error", err)
		return false
	}
	s.log.Info("connect: deletion finished", "tenant", tenantID, "kind", kind)
	return true
}

// RetryCleanups attempts every pending deletion once and returns how many
// finished.
func (s *Service) RetryCleanups(ctx context.Context) int {
	rows, err := s.pool.Query(ctx, `SELECT tenant_key, kind FROM pending_cleanups ORDER BY requested_at, tenant_key, kind LIMIT 100`)
	if err != nil {
		s.log.Error("connect: pending deletions", "error", err)
		return 0
	}
	type item struct{ tenant, kind string }
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.tenant, &it.kind); err == nil {
			items = append(items, it)
		}
	}
	rows.Close()
	done := 0
	for _, it := range items {
		if ctx.Err() != nil {
			break
		}
		if (it.kind == KindSecret && !s.sec.Enabled()) || (it.kind == KindVolume && !s.prov.Enabled()) {
			continue
		}
		if s.cleanup(ctx, it.tenant, it.kind) {
			done++
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
