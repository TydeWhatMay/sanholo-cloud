package updater

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/lhaig/sanholo-cloud/internal/provision"
)

// Summary describes a freshly planned rollout, for the audit row.
type Summary struct {
	Pending, Skipped, Done int
	Batches                int
	Canary                 string
}

// candidate is a tenant with an instance, as the planner and the final sweep
// see it.
type candidate struct {
	tenantID, version, digest, instState, tenantStatus string
	canary                                             bool
}

// classify decides what a rollout of rel does with the tenant: skip it (and
// why), count it done (it already runs the release) or move it.
func (c candidate) classify(rel Release) (state, reason string) {
	switch {
	case c.tenantStatus == "deleting":
		return RowSkipped, SkipDeleting
	case c.tenantStatus != "pending" && c.tenantStatus != "active":
		return RowSkipped, SkipSuspended
	case c.instState == provision.StateStopped:
		return RowSkipped, SkipStopped
	case c.instState != provision.StateRunning:
		return RowSkipped, SkipNotRunning
	case c.version == rel.Version && c.digest == rel.Digest:
		return RowDone, ""
	}
	return RowPending, ""
}

// candidates lists the tenants with an instance, oldest first. When
// missingFrom is a release version only those without a row for it are
// listed.
func candidates(ctx context.Context, tx pgx.Tx, missingFrom string) ([]candidate, error) {
	rows, err := tx.Query(ctx, `
		SELECT i.tenant_id, i.image_version, i.image_digest, i.state, t.status, t.is_canary
		FROM tenant_instances i JOIN tenants t ON t.id = i.tenant_id
		WHERE $1 = '' OR NOT EXISTS (SELECT 1 FROM rollout_tenants r WHERE r.release_version = $1 AND r.tenant_id = i.tenant_id)
		ORDER BY t.created_at, t.id`, missingFrom)
	if err != nil {
		return nil, fmt.Errorf("updater: candidates: %w", err)
	}
	defer rows.Close()
	var out []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.tenantID, &c.version, &c.digest, &c.instState, &c.tenantStatus, &c.canary); err != nil {
			return nil, fmt.Errorf("updater: candidates: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// StartTx plans the rollout of a release inside the caller's transaction
// and marks the release rolling. The canary is the tenant marked as such, or
// the oldest tenant that has to move; it is batch 0 on its own. The rest
// follow in creation order in batches of the configured size. Tenants that
// cannot be moved (suspended, stopped, not running) are recorded as skipped,
// tenants already on the release as done.
//
// ErrNotFound: no such release. ErrState: it is rolling or halted already.
// ErrRolloutActive: a different release is rolling or halted (the unique
// index refuses a second rollout, whichever process asks).
func (s *Service) StartTx(ctx context.Context, tx pgx.Tx, version string) (Summary, error) {
	var sum Summary
	rel, err := scanRelease(tx.QueryRow(ctx, `SELECT `+releaseCols+` FROM releases WHERE version = $1 FOR UPDATE`, version))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return sum, ErrNotFound
	case err != nil:
		return sum, fmt.Errorf("updater: start: %w", err)
	case rel.State == StateRolling || rel.State == StateHalted:
		return sum, ErrState
	}
	// Taking the rollout slot comes first: it is where a concurrent start
	// loses.
	now := s.now()
	if _, err := tx.Exec(ctx, `
		UPDATE releases SET state = 'rolling', batch_size = $2, halted_tenant = '', halt_reason = '',
		    started_at = $3, finished_at = NULL, updated_at = $3
		WHERE version = $1`, rel.Version, s.batch, now); err != nil {
		if isUnique(err, "releases_one_rollout") {
			return sum, ErrRolloutActive
		}
		return sum, fmt.Errorf("updater: start: %w", err)
	}
	rel.BatchSize = s.batch
	if _, err := tx.Exec(ctx, `DELETE FROM rollout_tenants WHERE release_version = $1`, rel.Version); err != nil {
		return sum, fmt.Errorf("updater: start: %w", err)
	}
	cands, err := candidates(ctx, tx, "")
	if err != nil {
		return sum, err
	}
	sum, err = s.insertRows(ctx, tx, rel, cands, 0, true)
	if err != nil {
		return sum, err
	}
	// The caller rolls the transaction back on an error, so nothing above
	// stays: a canary that failed health lately holds the rollout back.
	var recent bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM tenant_instances i
		    WHERE i.last_unhealthy_at > $2
		      AND (i.tenant_id IN (SELECT id FROM tenants WHERE is_canary) OR i.tenant_id = $1))`,
		sum.Canary, now.Add(-CanaryGate)).Scan(&recent); err != nil {
		return sum, fmt.Errorf("updater: start: canary health: %w", err)
	}
	if recent && !rel.CanaryOverride {
		return sum, ErrCanaryUnhealthy
	}
	return sum, nil
}

// insertRows writes a row per candidate. Pending ones are batched: with
// canary, one canary first (batch first), then batches of the release's
// batch size; without, batches only, from first.
func (s *Service) insertRows(ctx context.Context, tx pgx.Tx, rel Release, cands []candidate, first int, withCanary bool) (Summary, error) {
	var sum Summary
	var pending []candidate
	for _, c := range cands {
		state, reason := c.classify(rel)
		switch state {
		case RowPending:
			pending = append(pending, c)
			continue
		case RowSkipped:
			sum.Skipped++
		case RowDone:
			sum.Done++
		}
		if err := insertRow(ctx, tx, rel.Version, c, first, state, reason); err != nil {
			return sum, err
		}
	}
	batch := first
	if withCanary && len(pending) > 0 {
		pick := 0
		for i, c := range pending {
			if c.canary {
				pick = i
				break
			}
		}
		sum.Canary = pending[pick].tenantID
		if err := insertRow(ctx, tx, rel.Version, pending[pick], batch, RowPending, ""); err != nil {
			return sum, err
		}
		pending = append(pending[:pick:pick], pending[pick+1:]...)
		batch++
		sum.Pending++
		sum.Batches++
	}
	for i, c := range pending {
		b := batch + i/rel.BatchSize
		if err := insertRow(ctx, tx, rel.Version, c, b, RowPending, ""); err != nil {
			return sum, err
		}
		sum.Pending++
		if i%rel.BatchSize == 0 {
			sum.Batches++
		}
	}
	return sum, nil
}

func insertRow(ctx context.Context, tx pgx.Tx, version string, c candidate, batch int, state, reason string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO rollout_tenants (release_version, tenant_id, batch, state, reason, previous_version, previous_digest)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, version, c.tenantID, batch, state, reason, c.version, c.digest)
	if err != nil {
		return fmt.Errorf("updater: plan: %w", err)
	}
	return nil
}

// ResumeTx puts a halted rollout back to rolling. The tenants that failed
// or were reverted are tried again first; the ones not yet reached are
// still pending. ErrState when the release is not halted or a revert is
// still unfinished.
func (s *Service) ResumeTx(ctx context.Context, tx pgx.Tx) (Release, error) {
	rel, err := s.lockHalted(ctx, tx)
	if err != nil {
		return rel, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE rollout_tenants SET state = 'pending', reason = '', started_at = NULL, updated_at = $2
		WHERE release_version = $1 AND state IN ('reverted', 'failed')`, rel.Version, s.now()); err != nil {
		return rel, fmt.Errorf("updater: resume: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE releases SET state = 'rolling', halted_tenant = '', halt_reason = '', updated_at = $2 WHERE version = $1`,
		rel.Version, s.now()); err != nil {
		return rel, fmt.Errorf("updater: resume: %w", err)
	}
	return rel, nil
}

// AbandonTx gives up a halted rollout: its rows are dropped and the release
// goes back to available (done, for a release that is still the fleet
// target). Tenants already moved stay where they are. ErrState when the
// release is not halted or a revert is still unfinished.
func (s *Service) AbandonTx(ctx context.Context, tx pgx.Tx) (Release, error) {
	rel, err := s.lockHalted(ctx, tx)
	if err != nil {
		return rel, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM rollout_tenants WHERE release_version = $1`, rel.Version); err != nil {
		return rel, fmt.Errorf("updater: abandon: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE releases SET state = CASE WHEN is_target THEN 'done' ELSE 'available' END,
		    halted_tenant = '', halt_reason = '', canary_override = false, updated_at = $2 WHERE version = $1`, rel.Version, s.now()); err != nil {
		return rel, fmt.Errorf("updater: abandon: %w", err)
	}
	return rel, nil
}

// lockHalted locks the halted release and checks that no revert is under
// way.
func (s *Service) lockHalted(ctx context.Context, tx pgx.Tx) (Release, error) {
	rel, err := scanRelease(tx.QueryRow(ctx, `SELECT `+releaseCols+` FROM releases WHERE state = 'halted' FOR UPDATE`))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return rel, ErrState
	case err != nil:
		return rel, fmt.Errorf("updater: halted release: %w", err)
	}
	var reverting bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM rollout_tenants WHERE release_version = $1 AND state IN ('revert_pending', 'reverting'))`,
		rel.Version).Scan(&reverting); err != nil {
		return rel, fmt.Errorf("updater: halted release: %w", err)
	}
	if reverting {
		return rel, ErrState
	}
	return rel, nil
}

// OverrideCanaryTx lets the release proceed although the canary tenant was
// unhealthy in the last hour: it is started, or completed, regardless of the
// gate. The caller writes the audit row in the same transaction. It is
// cleared when a rollout is abandoned. ErrNotFound: no such release;
// ErrState: it is done.
func (s *Service) OverrideCanaryTx(ctx context.Context, tx pgx.Tx, version string) error {
	tag, err := tx.Exec(ctx, `UPDATE releases SET canary_override = true, updated_at = $2 WHERE version = $1 AND state <> 'done'`, version, s.now())
	if err != nil {
		return fmt.Errorf("updater: override: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM releases WHERE version = $1)`, version).Scan(&exists); err != nil {
		return fmt.Errorf("updater: override: %w", err)
	}
	if !exists {
		return ErrNotFound
	}
	return ErrState
}

// Hold says why a release cannot start or complete yet.
type Hold struct {
	// Tenant is the canary tenant that was unhealthy, Until when the gate
	// opens by itself.
	Tenant string
	Until  time.Time
}

// CanaryHold reports the canary gate for a release that is not done, nil when
// it is open or overridden.
func (s *Service) CanaryHold(ctx context.Context, rel Release) (*Hold, error) {
	if rel.CanaryOverride || rel.State == StateDone {
		return nil, nil
	}
	var h Hold
	var at time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT i.tenant_id, i.last_unhealthy_at FROM tenant_instances i
		WHERE i.last_unhealthy_at > $2
		  AND (i.tenant_id IN (SELECT id FROM tenants WHERE is_canary)
		       OR i.tenant_id IN (SELECT tenant_id FROM rollout_tenants WHERE release_version = $1 AND batch = 0))
		ORDER BY i.last_unhealthy_at DESC LIMIT 1`, rel.Version, s.now().Add(-CanaryGate)).Scan(&h.Tenant, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("updater: canary hold: %w", err)
	}
	h.Until = at.Add(CanaryGate)
	return &h, nil
}
