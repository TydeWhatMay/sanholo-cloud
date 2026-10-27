package provision

import (
	"context"
	"errors"

	"github.com/lhaig/sanholo-cloud/internal/audit"
	"github.com/lhaig/sanholo-cloud/internal/nomad"
)

// converge brings one running instance's Nomad job in line with what the
// control plane expects, and reports whether it re-registered the job. Two
// cases, both found by the reconcile worker:
//
//   - drift: the job's spec hash differs from the one the current settings
//     render for the instance's version (a rotated proxy key, a changed
//     memory limit, a job someone edited). The job is registered again;
//   - lag: the instance runs an older version than the fleet target, which
//     happens when it was not running (and so was skipped) while the target's
//     rollout planned its tenants. It moves to the target.
//
// It never acts for a tenant with an unfinished row in a rollout, and the
// lag case waits for every rollout to end: the updater owns those versions.
// The check runs under the tenant's lock, where the updater's registration
// also runs, and the updater's row stays pending until that registration is
// done, so the two cannot interleave.
func (s *Service) converge(ctx context.Context, tenantID string) bool {
	unlock, err := s.lockTenant(ctx, tenantID)
	if err != nil {
		return false
	}
	defer unlock()
	inst, err := s.Get(ctx, tenantID)
	if err != nil || inst == nil || inst.State != StateRunning {
		return false
	}
	var status string
	var active, anyRollout bool
	err = s.pool.QueryRow(ctx, `
		SELECT t.status,
		       EXISTS (SELECT 1 FROM rollout_tenants r JOIN releases l ON l.version = r.release_version
		               WHERE r.tenant_id = t.id AND l.state IN ('rolling', 'halted')
		                 AND r.state IN ('pending', 'updating', 'revert_pending', 'reverting')),
		       EXISTS (SELECT 1 FROM releases WHERE state IN ('rolling', 'halted'))
		FROM tenants t WHERE t.id = $1`, tenantID).Scan(&status, &active, &anyRollout)
	if err != nil || active || (status != "pending" && status != "active") {
		return false
	}
	version, digest, moved := inst.ImageVersion, inst.ImageDigest, false
	if !anyRollout {
		if tv, td, ok, err := s.target(ctx); err != nil {
			return false
		} else if ok && nomad.CompareVersions(tv, version) > 0 {
			// Forward only. An abandoned rollout leaves tenants on a version
			// that is newer than the target; moving them back could put an
			// older client on a volume a newer one has migrated.
			version, digest, moved = tv, td, true
		}
	}
	want, err := nomad.BuildJobPinned(s.set, tenantID, version, digest)
	if err != nil {
		return false
	}
	if !moved {
		info, err := s.nomad.Job(ctx, inst.JobID)
		if err != nil || info.Stop || info.Meta[nomad.MetaSpec] == want.Meta[nomad.MetaSpec] {
			return false
		}
	}
	if err := s.updateLocked(ctx, tenantID, version, digest); err != nil {
		s.log.Warn("provision: could not converge the job", "tenant", tenantID, "error", err)
		return false
	}
	s.log.Info("provision: job brought in line", "tenant", tenantID, "version", version, "moved", moved)
	s.auditConverge(ctx, tenantID, inst.ImageVersion, version, moved)
	return true
}

// ActorReconcile is the audit actor of what the reconcile worker changes.
const ActorReconcile = "reconcile"

// auditConverge records a re-registration made by the reconcile worker: a
// move to a newer version, or the same version registered again after drift.
// Best effort: the change is made, and a failed write is logged.
func (s *Service) auditConverge(ctx context.Context, tenantID, from, to string, moved bool) {
	ctx = context.WithoutCancel(ctx)
	action := "instance.reregister"
	if moved {
		action = "instance.converge"
	}
	err := func() error {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := audit.Write(ctx, tx, s.now(), ActorReconcile, action, "tenant:"+tenantID, map[string]any{"from": from, "to": to}); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}()
	if err != nil {
		s.log.Error("provision: could not audit a converge", "tenant", tenantID, "error", err)
	}
}

// SoakBase is the allocation and restart count an update's soak starts from.
type SoakBase struct {
	AllocID  string
	Restarts int
}

// Soak verdicts.
type SoakVerdict string

const (
	// SoakOK: a live check passes (or has not run yet), the same allocation
	// runs, and it has not restarted.
	SoakOK SoakVerdict = "ok"
	// SoakFailing: a live check fails, or the version has no running
	// allocation.
	SoakFailing SoakVerdict = "failing"
	// SoakRestarted: the task restart count rose.
	SoakRestarted SoakVerdict = "restarted"
	// SoakReplaced: another allocation runs the version now.
	SoakReplaced SoakVerdict = "replaced"
)

// SoakReason is the fixed phrase for a failed verdict.
func SoakReason(v SoakVerdict) string {
	switch v {
	case SoakRestarted:
		return ReasonSoakRestart
	case SoakReplaced:
		return ReasonSoakReplaced
	}
	return ReasonSoakCheck
}

// SoakBaseline reads the allocation and restart count to compare against.
func (s *Service) SoakBaseline(ctx context.Context, tenantID string) (SoakBase, error) {
	if !s.Enabled() {
		return SoakBase{}, ErrDisabled
	}
	jobID, err := nomad.JobID(tenantID)
	if err != nil {
		return SoakBase{}, ErrNotFound
	}
	// Under the tenant's lock, so a restart the control plane is carrying out
	// is finished (and recorded) before the baseline is read.
	unlock, err := s.lockTenant(ctx, tenantID)
	if err != nil {
		return SoakBase{}, err
	}
	defer unlock()
	st, err := s.nomad.Status(ctx, jobID)
	if err != nil {
		return SoakBase{}, err
	}
	return SoakBase{AllocID: st.AllocID, Restarts: st.Restarts}, nil
}

// CheckSoak judges the live state of the tenant's job against the baseline
// taken when the update's deployment turned healthy. Nomad's deployment
// verdict is decided once, 30 seconds in, so it cannot say anything about the
// minutes after; the live check results, the restart count and the identity
// of the allocation can. An error means Nomad could not be read.
func (s *Service) CheckSoak(ctx context.Context, tenantID string, base SoakBase) (SoakVerdict, error) {
	if !s.Enabled() {
		return "", ErrDisabled
	}
	jobID, err := nomad.JobID(tenantID)
	if err != nil {
		return "", ErrNotFound
	}
	st, err := s.nomad.StatusLive(ctx, jobID)
	if err != nil {
		return "", err
	}
	switch {
	case st.AllocID == "":
		return SoakFailing, nil
	case base.AllocID != "" && st.AllocID != base.AllocID:
		return SoakReplaced, nil
	case st.Restarts > base.Restarts:
		return SoakRestarted, nil
	case st.Phase == nomad.PhaseUnhealthy:
		return SoakFailing, nil
	}
	return SoakOK, nil
}

// DeleteVolume deletes the tenant's host volume. A volume that is gone is
// not an error. The job must be stopped first (Nomad refuses while it holds
// the volume).
func (s *Service) DeleteVolume(ctx context.Context, tenantID string) error {
	if !s.Enabled() {
		return ErrDisabled
	}
	return s.nomad.DeleteHostVolume(ctx, tenantID)
}

// RecordStopFailure marks the instance failed because Nomad refused to stop
// it (suspension).
func (s *Service) RecordStopFailure(ctx context.Context, tenantID string) {
	s.fail(ctx, tenantID, ReasonStopFailed)
}

// finishVolumeDeletion carries out a volume deletion that a disconnect left
// pending (Nomad refused it then) before the tenant gets a volume again: a
// different game account connected later must not inherit the old account's
// data. Nothing is pending in the normal case, which costs one query.
func (s *Service) finishVolumeDeletion(ctx context.Context, tenantID string) error {
	var pending bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pending_cleanups WHERE tenant_key = $1 AND kind = 'volume')`, tenantID).Scan(&pending); err != nil {
		return err
	}
	if !pending {
		return nil
	}
	if err := s.nomad.DeleteHostVolume(ctx, tenantID); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM pending_cleanups WHERE tenant_key = $1 AND kind = 'volume'`, tenantID)
	return err
}

// Leftovers reports whether Nomad still holds the tenant's job (stopped or
// not) or its host volume. Account deletion's check reads it; both are false
// once the deletion is complete.
func (s *Service) Leftovers(ctx context.Context, tenantID string) (job, volume bool, err error) {
	if !s.Enabled() {
		return false, false, ErrDisabled
	}
	jobID, err := nomad.JobID(tenantID)
	if err != nil {
		return false, false, ErrNotFound
	}
	switch _, err := s.nomad.Job(ctx, jobID); {
	case err == nil:
		job = true
	case !errors.Is(err, nomad.ErrNotFound):
		return false, false, err
	}
	volume, err = s.nomad.HostVolumeExists(ctx, tenantID)
	return job, volume, err
}
