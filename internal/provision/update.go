package provision

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/lhaig/sanholo-cloud/internal/nomad"
	"github.com/lhaig/sanholo-cloud/internal/tenant"
)

// ReasonStopped is recorded when a tenant's job was stopped while the updater
// was waiting for it.
const ReasonStopped = "the instance was stopped during the update"

// target is the fleet target release: the version and digest a new instance
// starts at. ok is false before any rollout has finished.
func (s *Service) target(ctx context.Context) (version, digest string, ok bool, err error) {
	err = s.pool.QueryRow(ctx, `SELECT version, coalesce(digest, '') FROM releases WHERE is_target`).Scan(&version, &digest)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", "", false, nil
	case err != nil:
		return "", "", false, fmt.Errorf("provision: fleet target: %w", err)
	}
	return version, digest, true, nil
}

// Update moves the tenant's instance to an image version, pinned to digest
// when one is given. It is the updater's door: the instance record takes the
// new version and goes back to pending, then the job is registered unless
// Nomad already holds exactly that spec (the spec hash makes a repeat, and a
// revert that Nomad's auto_revert already did, a no-op). Moving an instance
// to the version and digest it already has changes nothing in the database
// unless it had failed.
//
// ErrNotFound: the tenant has no instance. ErrNotAllowed: the tenant is not
// pending or active, or the instance is stopped; those are left alone.
func (s *Service) Update(ctx context.Context, tenantID, version, digest string) error {
	if !s.Enabled() {
		return ErrDisabled
	}
	if !tenant.Valid(tenantID) {
		return ErrNotFound
	}
	v, err := nomad.NormalizeVersion(version)
	if err != nil {
		return fmt.Errorf("provision: %w", err)
	}
	if _, err := nomad.BuildJobPinned(s.set, tenantID, v, digest); err != nil {
		return fmt.Errorf("provision: %w", err)
	}
	unlock, err := s.lockTenant(ctx, tenantID)
	if err != nil {
		return err
	}
	defer unlock()
	return s.updateLocked(ctx, tenantID, v, digest)
}

// updateLocked is Update for a caller that holds the tenant's lock. It renders
// the job itself, with the instance's stored restart counter, so an update or
// a revert never registers the old counter over a bump.
func (s *Service) updateLocked(ctx context.Context, tenantID, v, digest string) error {
	var status string
	var err error
	err = s.pool.QueryRow(ctx, `SELECT status FROM tenants WHERE id = $1`, tenantID).Scan(&status)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrNotFound
	case err != nil:
		return fmt.Errorf("provision: tenant: %w", err)
	case status != "pending" && status != "active":
		return ErrNotAllowed
	}
	cur, err := s.Get(ctx, tenantID)
	switch {
	case err != nil:
		return err
	case cur == nil:
		return ErrNotFound
	case cur.State == StateStopped:
		return ErrNotAllowed
	}
	if cur.ImageVersion != v || cur.ImageDigest != digest || cur.State == StateFailed {
		now := s.now()
		tag, err := s.pool.Exec(ctx, `
			UPDATE tenant_instances SET image_version = $2, image_digest = $3, state = 'pending', last_health = 'unknown',
			    failure_reason = '', requested_at = $4, updated_at = $4
			WHERE tenant_id = $1 AND state <> 'stopped'`, tenantID, v, digest, now)
		if err != nil {
			return fmt.Errorf("provision: record update: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return ErrNotAllowed
		}
	}
	job, err := nomad.BuildJobRestart(s.set, tenantID, v, digest, cur.RestartSeq)
	if err != nil {
		return fmt.Errorf("provision: %w", err)
	}
	return s.register(ctx, tenantID, job, false, "could not register the update")
}

// UpdateState is the updater's reading of a tenant's job against the spec it
// expects.
type UpdateState string

const (
	// UpdatePending: registered, not (yet) healthy. Includes an allocation
	// Nomad judged unhealthy but has not yet given up on.
	UpdatePending UpdateState = "pending"
	// UpdateHealthy: the expected spec is the job's current version and its
	// allocation is healthy.
	UpdateHealthy UpdateState = "healthy"
	// UpdateFailed: Nomad gave up on the deployment (or the job is dead).
	UpdateFailed UpdateState = "failed"
	// UpdateReverted: the job now holds a different spec than the expected
	// one: Nomad's auto_revert put the previous version back (or someone
	// else registered the job).
	UpdateReverted UpdateState = "reverted"
	// UpdateStopped: the job is stopped or gone.
	UpdateStopped UpdateState = "stopped"
)

// CheckUpdate reads Nomad's view of the tenant's job and judges it against
// the job that (version, digest) would render. Only allocations of the job's
// current version count (nomad.Status), so the allocation of a version that
// was rolled back says nothing. Any error means Nomad could not be read; the
// caller treats it as "not yet" until its own timeout.
func (s *Service) CheckUpdate(ctx context.Context, tenantID, version, digest string) (UpdateState, error) {
	if !s.Enabled() {
		return "", ErrDisabled
	}
	want, err := nomad.BuildJobPinned(s.set, tenantID, version, digest)
	if err != nil {
		return "", fmt.Errorf("provision: %w", err)
	}
	st, err := s.nomad.Status(ctx, want.ID)
	if err != nil {
		return "", fmt.Errorf("provision: check update: %w", err)
	}
	switch st.Phase {
	case nomad.PhaseAbsent, nomad.PhaseStopped:
		return UpdateStopped, nil
	}
	if st.Job == nil || st.Job.Meta[nomad.MetaSpec] != want.Meta[nomad.MetaSpec] {
		return UpdateReverted, nil
	}
	switch st.Phase {
	case nomad.PhaseHealthy:
		// Let the instance record catch up now rather than at the next
		// reconcile; best effort.
		_, _ = s.Refresh(ctx, tenantID)
		return UpdateHealthy, nil
	case nomad.PhaseFailed:
		return UpdateFailed, nil
	}
	return UpdatePending, nil
}

// ReasonFor is the fixed failure phrase for an error from Update.
func ReasonFor(err error) string { return reasonFor(err) }
