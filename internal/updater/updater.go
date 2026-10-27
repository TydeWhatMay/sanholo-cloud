// Package updater rolls a client release out to the fleet (US-021).
//
// The operator adds a release (version, optional image digest) and starts its
// rollout. The updater moves one canary tenant first, then batches of tenants
// (default 5), and does not start a batch before every tenant of the one
// before is healthy on the new version. Any failure (a deployment that
// failed, one that Nomad reverted, a timeout, a refused registration) halts
// the rollout: the tenant that failed is put back on its previous version, the
// tenants of the batch that were still updating are put back too, the tenants
// not yet reached are never touched, and the rollout waits for the operator to
// resume or abandon it.
//
// All state is in PostgreSQL (releases, rollout_tenants). A step reads it,
// does one thing per tenant and writes the result, so a restarted control
// plane resumes where the last one stopped; repeating a step is harmless
// because registering a spec Nomad already holds is skipped (the spec hash).
// A release becomes the fleet target, the version new instances start at,
// only when its rollout is done.
package updater

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lhaig/sanholo-cloud/internal/audit"
	"github.com/lhaig/sanholo-cloud/internal/nomad"
	"github.com/lhaig/sanholo-cloud/internal/provision"
)

// Release states (releases.state).
const (
	StateAvailable = "available"
	StateRolling   = "rolling"
	StateHalted    = "halted"
	StateDone      = "done"
)

// Row states (rollout_tenants.state).
const (
	RowPending       = "pending"
	RowUpdating      = "updating"
	RowRevertPending = "revert_pending"
	RowReverting     = "reverting"
	RowDone          = "done"
	RowSkipped       = "skipped"
	RowReverted      = "reverted"
	RowFailed        = "failed"
)

// ActorUpdater is the audit actor of changes the updater makes itself.
const ActorUpdater = "updater"

// Fixed phrases. Nomad's own text is never stored; the failure phrases of
// the provisioner (provision.Reason*) are used as they are.
const (
	ReasonNomadReverted = "Nomad reverted the deployment"
	ReasonSibling       = "another tenant in the batch failed"
	SkipSuspended       = "the tenant is suspended"
	SkipDeleting        = "the tenant is being deleted"
	SkipStopped         = "the instance is stopped"
	SkipNotRunning      = "the instance is not running"
	SkipChanged         = "the tenant was suspended or stopped during the rollout"
)

var (
	// ErrNotFound: no such release or tenant.
	ErrNotFound = errors.New("not found")
	// ErrExists: the release version is already known.
	ErrExists = errors.New("the release already exists")
	// ErrInvalid: the version, digest, notes or URL is not acceptable.
	ErrInvalid = errors.New("invalid release")
	// ErrRolloutActive: another rollout is rolling or halted.
	ErrRolloutActive = errors.New("another rollout is under way")
	// ErrState: the action does not apply to the release's current state.
	ErrState = errors.New("not applicable in the current state")
	// ErrCanaryUnhealthy: the canary tenant failed its health check in the
	// last hour, so a rollout is not started (and not completed).
	ErrCanaryUnhealthy = errors.New("the canary tenant was unhealthy in the last hour")
)

// Options configure the service.
type Options struct {
	// BatchSize is how many tenants move together after the canary.
	// Default 5.
	BatchSize int
	// HealthTimeout is how long a tenant may take to become healthy on the
	// new version (and, after a failure, on the old one). Default 15
	// minutes.
	HealthTimeout time.Duration
	// CanarySoak and BatchSoak are how long a tenant is watched after its
	// deployment turned healthy before it counts as done: the canary (batch
	// 0) and every other batch. Nomad's own verdict comes 30 seconds in, and
	// the client answers 200 "starting" for up to ten minutes, so the soak is
	// what notices a bot that never really came up. Zero skips the soak (the
	// configuration default is 1h and 10m).
	CanarySoak time.Duration
	BatchSoak  time.Duration
	Logger     *slog.Logger
}

// CanaryGate is how long after a health failure of the canary tenant a
// rollout is refused.
const CanaryGate = time.Hour

// Service plans and drives rollouts.
type Service struct {
	pool    *pgxpool.Pool
	prov    *provision.Service
	batch   int
	timeout time.Duration
	canary  time.Duration
	soak    time.Duration
	log     *slog.Logger
	now     func() time.Time

	mu sync.Mutex // one Step at a time in this process
}

// New returns a Service. prov may be nil or disabled for the commands that
// only touch the database (add, plan, resume, abandon); Step then does
// nothing.
func New(pool *pgxpool.Pool, prov *provision.Service, opt Options) *Service {
	s := &Service{pool: pool, prov: prov, batch: opt.BatchSize, timeout: opt.HealthTimeout, canary: opt.CanarySoak, soak: opt.BatchSoak, log: opt.Logger, now: time.Now}
	if s.batch <= 0 {
		s.batch = 5
	}
	if s.timeout <= 0 {
		s.timeout = 15 * time.Minute
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	return s
}

// SetClock replaces the time source (tests).
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// Release is one row of the releases table.
type Release struct {
	Version      string
	Digest       string
	Notes        string
	URL          string
	State        string
	IsTarget     bool
	BatchSize    int
	HaltedTenant string
	HaltReason   string
	CreatedAt    time.Time
	StartedAt    *time.Time
	FinishedAt   *time.Time
}

// Row is a tenant's progress in a release's rollout.
type Row struct {
	Version         string
	TenantID        string
	Batch           int
	State           string
	Reason          string
	PreviousVersion string
	PreviousDigest  string
	StartedAt       *time.Time
	UpdatedAt       time.Time
	// SoakStartedAt is when the deployment turned healthy and the soak
	// began; SoakAlloc and SoakRestarts are what the soak compares against.
	SoakStartedAt *time.Time
	SoakAlloc     string
	SoakRestarts  int
}

const releaseCols = `version, coalesce(digest, ''), notes, url, state, is_target, batch_size, halted_tenant, halt_reason, created_at, started_at, finished_at`

func scanRelease(row pgx.Row) (Release, error) {
	var r Release
	err := row.Scan(&r.Version, &r.Digest, &r.Notes, &r.URL, &r.State, &r.IsTarget, &r.BatchSize, &r.HaltedTenant, &r.HaltReason, &r.CreatedAt, &r.StartedAt, &r.FinishedAt)
	return r, err
}

const rowCols = `release_version, tenant_id, batch, state, reason, previous_version, previous_digest, started_at, updated_at, soak_started_at, soak_alloc, soak_restarts`

func scanRow(row pgx.Row) (Row, error) {
	var r Row
	err := row.Scan(&r.Version, &r.TenantID, &r.Batch, &r.State, &r.Reason, &r.PreviousVersion, &r.PreviousDigest, &r.StartedAt, &r.UpdatedAt, &r.SoakStartedAt, &r.SoakAlloc, &r.SoakRestarts)
	return r, err
}

// Releases lists every release, newest first.
func (s *Service) Releases(ctx context.Context) ([]Release, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+releaseCols+` FROM releases ORDER BY created_at DESC, version DESC`)
	if err != nil {
		return nil, fmt.Errorf("updater: releases: %w", err)
	}
	defer rows.Close()
	var out []Release
	for rows.Next() {
		r, err := scanRelease(rows)
		if err != nil {
			return nil, fmt.Errorf("updater: releases: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Target is the fleet target, nil before any rollout has finished.
func (s *Service) Target(ctx context.Context) (*Release, error) {
	r, err := scanRelease(s.pool.QueryRow(ctx, `SELECT `+releaseCols+` FROM releases WHERE is_target`))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("updater: target: %w", err)
	}
	return &r, nil
}

// Rollout returns the rollout progress of a release: its rows by batch, then
// creation order of the tenant.
func (s *Service) Rollout(ctx context.Context, version string) ([]Row, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+rowCols+` FROM rollout_tenants r JOIN tenants t ON t.id = r.tenant_id
		WHERE release_version = $1 ORDER BY batch, t.created_at, tenant_id`, version)
	if err != nil {
		return nil, fmt.Errorf("updater: rollout: %w", err)
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, fmt.Errorf("updater: rollout: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func isUnique(err error, constraint string) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505" && (constraint == "" || pg.ConstraintName == constraint)
}

// inTx runs fn and commits, or rolls everything back.
func (s *Service) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("updater: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("updater: commit: %w", err)
	}
	return nil
}

// AddReleaseTx records a release inside the caller's transaction (the admin
// action writes its audit row there). The version may be written with or
// without the leading v; a digest, when given, must be sha256:<64 hex>.
func (s *Service) AddReleaseTx(ctx context.Context, tx pgx.Tx, version, digest, notes, url string) (Release, error) {
	v, err := nomad.NormalizeVersion(version)
	if err != nil {
		return Release{}, fmt.Errorf("%w: version", ErrInvalid)
	}
	digest = strings.TrimSpace(digest)
	if digest != "" && !nomad.ValidDigest(digest) {
		return Release{}, fmt.Errorf("%w: digest", ErrInvalid)
	}
	notes, url = strings.TrimSpace(notes), strings.TrimSpace(url)
	if len([]rune(notes)) > 2000 || strings.ContainsFunc(notes, func(r rune) bool { return r < ' ' && r != '\n' && r != '\t' && r != '\r' || r == 0x7f }) {
		return Release{}, fmt.Errorf("%w: notes", ErrInvalid)
	}
	if url != "" && !validURL(url) {
		return Release{}, fmt.Errorf("%w: url", ErrInvalid)
	}
	var d any
	if digest != "" {
		d = digest
	}
	r, err := scanRelease(tx.QueryRow(ctx, `
		INSERT INTO releases (version, digest, notes, url, batch_size, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $6) RETURNING `+releaseCols, v, d, notes, url, s.batch, s.now()))
	switch {
	case isUnique(err, ""):
		return Release{}, ErrExists
	case err != nil:
		return Release{}, fmt.Errorf("updater: add release: %w", err)
	}
	return r, nil
}

func validURL(u string) bool {
	if len(u) > 300 || !strings.HasPrefix(u, "https://") {
		return false
	}
	return !strings.ContainsFunc(u, func(r rune) bool { return r <= ' ' || r == 0x7f || r == '<' || r == '>' || r == '"' })
}

// SetCanaryTx marks the tenant as the canary of every rollout; an empty id
// clears the mark. ErrNotFound for an unknown tenant.
func (s *Service) SetCanaryTx(ctx context.Context, tx pgx.Tx, tenantID string) error {
	if _, err := tx.Exec(ctx, `UPDATE tenants SET is_canary = false WHERE is_canary AND id <> $1`, tenantID); err != nil {
		return fmt.Errorf("updater: canary: %w", err)
	}
	if tenantID == "" {
		return nil
	}
	tag, err := tx.Exec(ctx, `UPDATE tenants SET is_canary = true WHERE id = $1`, tenantID)
	if err != nil {
		return fmt.Errorf("updater: canary: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}

func audited(ctx context.Context, tx pgx.Tx, at time.Time, action, version string, details map[string]any) error {
	return audit.Write(ctx, tx, at, ActorUpdater, action, "release:"+version, details)
}
