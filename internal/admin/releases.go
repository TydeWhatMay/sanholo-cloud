package admin

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/lhaig/sanholo-cloud/internal/updater"
)

var (
	// ErrReleaseExists means the release version is already known.
	ErrReleaseExists = updater.ErrExists
	// ErrReleaseInvalid means the version, digest, notes or URL was refused.
	ErrReleaseInvalid = updater.ErrInvalid
	// ErrRolloutActive means another rollout is rolling or halted.
	ErrRolloutActive = updater.ErrRolloutActive
)

// SetUpdater gives the service the updater that the release actions drive.
func (s *Service) SetUpdater(u *updater.Service) { s.upd = u }

// mapUpdater turns the updater's sentinels into this package's.
func mapUpdater(err error) error {
	switch {
	case errors.Is(err, updater.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, updater.ErrState):
		return ErrState
	}
	return err
}

// AddRelease records a client release (US-021). It is operator-only: the
// command line and the admin page are the only ways in.
func (s *Service) AddRelease(ctx context.Context, actor, version, digest, notes, url string) error {
	if s.upd == nil {
		return ErrState
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		r, err := s.upd.AddReleaseTx(ctx, tx, version, digest, notes, url)
		if err != nil {
			return mapUpdater(err)
		}
		return s.audit(ctx, tx, actor, "release.add", "release:"+r.Version, map[string]any{"pinned": r.Digest != "", "digest": r.Digest})
	})
}

// StartRollout plans and starts the rollout of a release. The updater
// drives it from there.
func (s *Service) StartRollout(ctx context.Context, actor, version string) error {
	if s.upd == nil {
		return ErrState
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		sum, err := s.upd.StartTx(ctx, tx, version)
		if err != nil {
			return mapUpdater(err)
		}
		return s.audit(ctx, tx, actor, "rollout.start", "release:"+version, map[string]any{
			"tenants": sum.Pending, "skipped": sum.Skipped, "already_current": sum.Done, "batches": sum.Batches, "canary": sum.Canary,
		})
	})
}

// ResumeRollout puts the halted rollout back to rolling.
func (s *Service) ResumeRollout(ctx context.Context, actor string) error {
	if s.upd == nil {
		return ErrState
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		r, err := s.upd.ResumeTx(ctx, tx)
		if err != nil {
			return mapUpdater(err)
		}
		return s.audit(ctx, tx, actor, "rollout.resume", "release:"+r.Version, map[string]any{"halted_tenant": r.HaltedTenant, "reason": r.HaltReason})
	})
}

// AbandonRollout gives up the halted rollout.
func (s *Service) AbandonRollout(ctx context.Context, actor string) error {
	if s.upd == nil {
		return ErrState
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		r, err := s.upd.AbandonTx(ctx, tx)
		if err != nil {
			return mapUpdater(err)
		}
		return s.audit(ctx, tx, actor, "rollout.abandon", "release:"+r.Version, map[string]any{"halted_tenant": r.HaltedTenant, "reason": r.HaltReason})
	})
}

// SetCanary marks a tenant as the canary of every rollout; an empty id
// clears the mark.
func (s *Service) SetCanary(ctx context.Context, actor, tenantID string) error {
	if s.upd == nil {
		return ErrState
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if err := s.upd.SetCanaryTx(ctx, tx, tenantID); err != nil {
			return mapUpdater(err)
		}
		target := "tenant:" + tenantID
		action := "tenant.canary"
		if tenantID == "" {
			target, action = "", "tenant.canary_clear"
		}
		return s.audit(ctx, tx, actor, action, target, nil)
	})
}

// Releases lists the releases, newest first.
func (s *Service) Releases(ctx context.Context) ([]updater.Release, error) {
	if s.upd == nil {
		return nil, nil
	}
	return s.upd.Releases(ctx)
}

// RolloutRows is the per-tenant progress of a release's rollout.
func (s *Service) RolloutRows(ctx context.Context, version string) ([]updater.Row, error) {
	if s.upd == nil {
		return nil, nil
	}
	return s.upd.Rollout(ctx, version)
}
