package updater

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/lhaig/sanholo-cloud/internal/provision"
)

// stepLockKey is the advisory lock that lets one control-plane process at a
// time drive the rollout. It is a session lock: a crashed holder releases it
// with its connection.
const stepLockKey int64 = 0x53414e55504454 // "SANUPDT"

// Run steps the active rollout each interval until ctx ends.
func (s *Service) Run(ctx context.Context, interval time.Duration) {
	if !s.prov.Enabled() {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Step(ctx); err != nil && ctx.Err() == nil {
				s.log.Warn("updater: step failed", "error", err)
			}
		}
	}
}

// Step advances the active rollout by one pass and returns. A pass starts the
// pending tenants of the current batch, judges the ones that are updating,
// and moves on to the next batch only when the current one is complete. With
// no rollout, or provisioning disabled, it does nothing. Another process
// holding the step lock makes it return at once.
func (s *Service) Step(ctx context.Context) error {
	if !s.prov.Enabled() {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("updater: step: %w", err)
	}
	defer conn.Release()
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, stepLockKey).Scan(&got); err != nil {
		return fmt.Errorf("updater: step: lock: %w", err)
	}
	if !got {
		return nil
	}
	defer func() { _, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, stepLockKey) }()

	rel, err := scanRelease(s.pool.QueryRow(ctx, `SELECT `+releaseCols+` FROM releases WHERE state IN ('rolling', 'halted')`))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("updater: step: %w", err)
	}
	rows, err := s.rows(ctx, rel.Version)
	if err != nil {
		return err
	}
	if rel.State == StateHalted {
		return s.advanceReverts(ctx, rel, rows)
	}
	cur, found := -1, false
	for _, r := range rows {
		if active(r.State) && (!found || r.Batch < cur) {
			cur, found = r.Batch, true
		}
	}
	if !found {
		return s.finish(ctx, rel)
	}
	for _, r := range rows {
		if r.Batch != cur {
			continue
		}
		var halted bool
		switch r.State {
		case RowPending:
			halted, err = s.start(ctx, rel, r)
		case RowUpdating:
			halted, err = s.poll(ctx, rel, r)
		}
		if err != nil {
			return err
		}
		if halted {
			return nil
		}
	}
	return nil
}

func active(state string) bool {
	switch state {
	case RowPending, RowUpdating, RowRevertPending, RowReverting:
		return true
	}
	return false
}

func (s *Service) rows(ctx context.Context, version string) ([]Row, error) {
	return s.Rollout(ctx, version)
}

// setRow moves one row from a state to another; it reports whether the row
// was in from (a concurrent abandon or resume makes it false).
func (s *Service) setRow(ctx context.Context, rel Release, r Row, from, to, reason string, started bool) (bool, error) {
	now := s.now()
	var startedAt any
	if started {
		startedAt = now
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE rollout_tenants SET state = $4, reason = $5, started_at = coalesce($6, started_at), updated_at = $7
		WHERE release_version = $1 AND tenant_id = $2 AND state = $3`,
		rel.Version, r.TenantID, from, to, reason, startedAt, now)
	if err != nil {
		return false, fmt.Errorf("updater: record progress: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// start registers the new version for a pending tenant. A tenant that was
// suspended or stopped since the plan is skipped; a registration Nomad
// refuses fails the tenant.
func (s *Service) start(ctx context.Context, rel Release, r Row) (halted bool, err error) {
	err = s.prov.Update(ctx, r.TenantID, rel.Version, rel.Digest)
	switch {
	case err == nil:
		_, err = s.setRow(ctx, rel, r, RowPending, RowUpdating, "", true)
		return false, err
	case errors.Is(err, provision.ErrDisabled):
		return false, err
	case errors.Is(err, provision.ErrNotAllowed), errors.Is(err, provision.ErrNotFound):
		_, err = s.setRow(ctx, rel, r, RowPending, RowSkipped, SkipChanged, false)
		return false, err
	case ctx.Err() != nil:
		return false, ctx.Err()
	}
	s.log.Warn("updater: could not register the release", "tenant", r.TenantID, "release", rel.Version, "error", err)
	return true, s.halt(ctx, rel, r, provision.ReasonFor(err))
}

// poll judges an updating tenant against Nomad.
func (s *Service) poll(ctx context.Context, rel Release, r Row) (halted bool, err error) {
	state, cerr := s.prov.CheckUpdate(ctx, r.TenantID, rel.Version, rel.Digest)
	expired := r.StartedAt != nil && s.now().Sub(*r.StartedAt) > s.timeout
	reason := ""
	switch {
	case cerr != nil && ctx.Err() != nil:
		return false, ctx.Err()
	case cerr != nil, state == provision.UpdatePending:
		// Nomad unreadable counts as "not yet": only the timeout fails it.
		if !expired {
			return false, nil
		}
		reason = provision.ReasonTimeout
	case state == provision.UpdateHealthy:
		_, err = s.setRow(ctx, rel, r, RowUpdating, RowDone, "", false)
		return false, err
	case state == provision.UpdateFailed:
		reason = provision.ReasonDeployFailed
	case state == provision.UpdateReverted:
		reason = ReasonNomadReverted
	case state == provision.UpdateStopped:
		// Stopped on purpose (disconnect, delete) is not the release's fault.
		if inst, gerr := s.prov.Get(ctx, r.TenantID); gerr == nil && (inst == nil || inst.State == provision.StateStopped) {
			_, err = s.setRow(ctx, rel, r, RowUpdating, RowSkipped, SkipChanged, false)
			return false, err
		}
		reason = provision.ReasonStopped
	}
	return true, s.halt(ctx, rel, r, reason)
}

// halt records a failure and stops the rollout. In one transaction: the
// failed tenant and the tenants of the batch that are still updating become
// revert_pending, the release becomes halted, and the audit row is written.
// Tenants that are pending are not touched. Then the reverts start.
func (s *Service) halt(ctx context.Context, rel Release, r Row, reason string) error {
	ctx = context.WithoutCancel(ctx)
	now := s.now()
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var state string
		if err := tx.QueryRow(ctx, `SELECT state FROM releases WHERE version = $1 FOR UPDATE`, rel.Version).Scan(&state); err != nil {
			return fmt.Errorf("updater: halt: %w", err)
		}
		if state != StateRolling {
			return nil // abandoned or already halted meanwhile
		}
		tag, err := tx.Exec(ctx, `
			UPDATE rollout_tenants SET state = 'revert_pending', reason = $3, started_at = $4, updated_at = $4
			WHERE release_version = $1 AND tenant_id = $2 AND state IN ('pending', 'updating')`, rel.Version, r.TenantID, reason, now)
		if err != nil {
			return fmt.Errorf("updater: halt: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return nil
		}
		if _, err := tx.Exec(ctx, `
			UPDATE rollout_tenants SET state = 'revert_pending', reason = $3, started_at = $4, updated_at = $4
			WHERE release_version = $1 AND tenant_id <> $2 AND state = 'updating'`, rel.Version, r.TenantID, ReasonSibling, now); err != nil {
			return fmt.Errorf("updater: halt: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE releases SET state = 'halted', halted_tenant = $2, halt_reason = $3, updated_at = $4 WHERE version = $1`,
			rel.Version, r.TenantID, reason, now); err != nil {
			return fmt.Errorf("updater: halt: %w", err)
		}
		return audited(ctx, tx, now, "rollout.halt", rel.Version, map[string]any{"tenant": r.TenantID, "reason": reason, "batch": r.Batch})
	})
	if err != nil {
		return err
	}
	s.log.Warn("updater: rollout halted", "release", rel.Version, "tenant", r.TenantID, "reason", reason)
	rel.State = StateHalted
	rows, err := s.rows(ctx, rel.Version)
	if err != nil {
		return err
	}
	return s.advanceReverts(ctx, rel, rows)
}

// advanceReverts drives the tenants of a halted rollout that are going back
// to their previous version. The revert is explicit and does not depend on
// Nomad's auto_revert: the previous spec is registered again, which Nomad
// skips when it already holds exactly that spec (auto_revert got there
// first), and the tenant counts as reverted only once Nomad reports the
// previous version healthy. A revert that cannot be registered is retried
// each step; one that is not healthy within the timeout marks the tenant
// failed, for the operator.
func (s *Service) advanceReverts(ctx context.Context, rel Release, rows []Row) error {
	for _, r := range rows {
		expired := r.StartedAt != nil && s.now().Sub(*r.StartedAt) > s.timeout
		switch r.State {
		case RowRevertPending:
			err := s.prov.Update(ctx, r.TenantID, r.PreviousVersion, r.PreviousDigest)
			switch {
			case err == nil:
				if _, err := s.setRow(ctx, rel, r, RowRevertPending, RowReverting, r.Reason, true); err != nil {
					return err
				}
			case errors.Is(err, provision.ErrDisabled), ctx.Err() != nil:
				return err
			case errors.Is(err, provision.ErrNotAllowed), errors.Is(err, provision.ErrNotFound), expired:
				if _, err := s.setRow(ctx, rel, r, RowRevertPending, RowFailed, r.Reason, false); err != nil {
					return err
				}
			}
		case RowReverting:
			state, err := s.prov.CheckUpdate(ctx, r.TenantID, r.PreviousVersion, r.PreviousDigest)
			switch {
			case err != nil && ctx.Err() != nil:
				return ctx.Err()
			case err == nil && state == provision.UpdateHealthy:
				if _, err := s.setRow(ctx, rel, r, RowReverting, RowReverted, r.Reason, false); err != nil {
					return err
				}
			case err == nil && (state == provision.UpdateFailed || state == provision.UpdateStopped), expired:
				if _, err := s.setRow(ctx, rel, r, RowReverting, RowFailed, r.Reason, false); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// finish runs when no tenant is left to move. First a sweep: an instance
// that was created or started while the rollout ran, and so has no row, gets
// one; if any of them has to move the rollout goes on. Otherwise the release
// becomes done and the fleet target.
func (s *Service) finish(ctx context.Context, rel Release) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		var state string
		if err := tx.QueryRow(ctx, `SELECT state FROM releases WHERE version = $1 FOR UPDATE`, rel.Version).Scan(&state); err != nil {
			return fmt.Errorf("updater: finish: %w", err)
		}
		if state != StateRolling {
			return nil
		}
		cands, err := candidates(ctx, tx, rel.Version)
		if err != nil {
			return err
		}
		if len(cands) > 0 {
			var next int
			if err := tx.QueryRow(ctx, `SELECT coalesce(max(batch), -1) + 1 FROM rollout_tenants WHERE release_version = $1`, rel.Version).Scan(&next); err != nil {
				return fmt.Errorf("updater: finish: %w", err)
			}
			sum, err := s.insertRows(ctx, tx, rel, cands, next, false)
			if err != nil {
				return err
			}
			if sum.Pending > 0 {
				return nil
			}
		}
		now := s.now()
		if _, err := tx.Exec(ctx, `UPDATE releases SET is_target = false, updated_at = $1 WHERE is_target AND version <> $2`, now, rel.Version); err != nil {
			return fmt.Errorf("updater: finish: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE releases SET state = 'done', is_target = true, finished_at = $2, updated_at = $2 WHERE version = $1`, rel.Version, now); err != nil {
			return fmt.Errorf("updater: finish: %w", err)
		}
		var done, skipped int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FILTER (WHERE state = 'done'), count(*) FILTER (WHERE state = 'skipped')
			FROM rollout_tenants WHERE release_version = $1`, rel.Version).Scan(&done, &skipped); err != nil {
			return fmt.Errorf("updater: finish: %w", err)
		}
		return audited(ctx, tx, now, "rollout.complete", rel.Version, map[string]any{"done": done, "skipped": skipped})
	})
}
