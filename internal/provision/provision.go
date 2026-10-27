// Package provision starts, watches and stops a tenant's Nomad job (US-012).
//
// After a tenant has connected a game account (internal/connect), Trigger
// registers the job through the Nomad HTTP API, records the instance in
// tenant_instances and follows it until Nomad calls it healthy, called it
// failed, or the timeout passes. Registering is idempotent: a job whose spec
// hash is already in Nomad is left alone, and the registration carries the
// job's modify index so two writers cannot overwrite each other.
//
// A job carries no secret, and neither does the table: the game token
// reaches the bot from qm, through the identity Nomad gives the task.
//
// Without a Nomad address (development) the service is disabled: Trigger
// does nothing, Get reports nothing, and tenants stay "not provisioned".
package provision

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lhaig/sanholo-cloud/internal/nomad"
	"github.com/lhaig/sanholo-cloud/internal/tenant"
)

// Instance states (tenant_instances.state).
const (
	StatePending = "pending"
	StateRunning = "running"
	StateFailed  = "failed"
	StateStopped = "stopped"
)

// Health values (tenant_instances.last_health).
const (
	HealthUnknown   = "unknown"
	HealthStarting  = "starting"
	HealthHealthy   = "healthy"
	HealthUnhealthy = "unhealthy"
)

// Failure reasons. Fixed phrases: Nomad's own text is never stored.
const (
	ReasonRegister     = "Nomad refused the job"
	ReasonUnreachable  = "Nomad could not be reached"
	ReasonDenied       = "Nomad refused the control plane's token"
	ReasonTimeout      = "the bot did not become healthy in time"
	ReasonDeployFailed = "Nomad reported that the deployment failed"
	ReasonMissing      = "the job is missing from Nomad"
	ReasonVolume       = "Nomad could not create the bot's data volume"
	ReasonStopFailed   = "Nomad did not stop the bot"
	ReasonSoakCheck    = "a health check failed after the update"
	ReasonSoakRestart  = "the bot restarted after the update"
	ReasonSoakReplaced = "Nomad replaced the bot's allocation after the update"
)

var (
	// ErrDisabled: Nomad is not configured, so nothing can be provisioned.
	ErrDisabled = errors.New("provisioning is not configured")
	// ErrNotFound: the tenant, or its instance, does not exist.
	ErrNotFound = errors.New("not found")
	// ErrNoGameAccount: the tenant has not connected a game account.
	ErrNoGameAccount = errors.New("the tenant has no game account")
	// ErrNotAllowed: the tenant is suspended or being deleted, or the
	// instance is stopped on purpose.
	ErrNotAllowed = errors.New("not allowed in the tenant's current state")
	// ErrNotRunning means the tenant has no instance that should be running
	// (never started, stopped, failed, or provisioning is off).
	ErrNotRunning = errors.New("the tenant's bot is not running")
	// ErrStarting means the instance exists and should run but has no
	// reachable allocation yet.
	ErrStarting = errors.New("the tenant's bot is starting")
)

// Options configure the service.
type Options struct {
	// Settings are the deployment-wide job settings; Version is the image
	// version a new instance starts at (an existing instance keeps its own).
	Settings nomad.JobSettings
	Version  string
	// Timeout is how long a new or restarted instance may take to become
	// healthy. Default 15 minutes.
	Timeout time.Duration
	// Poll is the interval between status reads. Default 5 seconds.
	Poll time.Duration
	// Volume are the settings of each tenant's dynamic host volume.
	Volume nomad.VolumeSettings
	// DriftPerPass caps how many instances one reconcile pass re-registers
	// because their job differs from the expected spec (a rotated proxy key
	// touches every tenant; this spreads it out). Default 3.
	DriftPerPass int
	// LockSlots caps the tenants holding a lock at once. Default: the pool's
	// connections minus two.
	LockSlots int
	Logger    *slog.Logger
}

// Service provisions tenant jobs. A nil *Service is disabled.
type Service struct {
	pool    *pgxpool.Pool
	nomad   *nomad.Client
	set     nomad.JobSettings
	vol     nomad.VolumeSettings
	drift   int
	version string
	timeout time.Duration
	poll    time.Duration
	log     *slog.Logger
	now     func() time.Time

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	inflight map[string]bool
	// queued holds the tenants whose restart was asked for while a start was
	// in flight; the start's goroutine runs it when it is done.
	queued map[string]bool
	closed bool

	// lockSem bounds the tenants holding a lock connection at once, so the
	// queries made under the lock always find a free connection.
	lockSem chan struct{}
}

// New returns a Service. With a disabled Nomad client the service is
// disabled and the options are not checked; otherwise the job settings and
// version must be valid, so a mistake stops the process at start.
func New(pool *pgxpool.Pool, nc *nomad.Client, opt Options) (*Service, error) {
	s := &Service{pool: pool, nomad: nc, log: opt.Logger, now: time.Now, inflight: map[string]bool{}, queued: map[string]bool{}}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	// Each holder keeps a connection and its queries need another, so at
	// most MaxConns-2 holders leave room for them (and for requests); the
	// size can be set explicitly.
	n := opt.LockSlots
	if n <= 0 {
		n = 1
		if pool != nil {
			n = max(1, int(pool.Config().MaxConns)-2)
		}
	}
	s.lockSem = make(chan struct{}, n)
	if !nc.Enabled() {
		return s, nil
	}
	v, err := nomad.NormalizeVersion(opt.Version)
	if err != nil {
		return nil, fmt.Errorf("provision: %w", err)
	}
	if nc.Namespace() != opt.Settings.Namespace {
		return nil, errors.New("provision: the job namespace is not the Nomad client's namespace")
	}
	if _, err := nomad.BuildJob(opt.Settings, "check", v); err != nil {
		return nil, fmt.Errorf("provision: %w", err)
	}
	if err := opt.Volume.Validate(); err != nil {
		return nil, fmt.Errorf("provision: %w", err)
	}
	s.set, s.version, s.vol = opt.Settings, v, opt.Volume
	s.drift = opt.DriftPerPass
	if s.drift <= 0 {
		s.drift = 3
	}
	s.timeout, s.poll = opt.Timeout, opt.Poll
	if s.timeout <= 0 {
		s.timeout = 15 * time.Minute
	}
	if s.poll <= 0 {
		s.poll = 5 * time.Second
	}
	return s, nil
}

// SetClock replaces the time source (tests).
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// Enabled reports whether Nomad is configured.
func (s *Service) Enabled() bool { return s != nil && s.nomad.Enabled() }

// Close stops background work and waits for it to end.
func (s *Service) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.cancel()
	s.wg.Wait()
}

// Wait blocks until every background start has finished (tests).
func (s *Service) Wait() {
	if s != nil {
		s.wg.Wait()
	}
}

// Instance is the non-secret record of a tenant's job.
type Instance struct {
	TenantID     string
	JobID        string
	ImageVersion string
	// ImageDigest is the digest the job is pinned to, empty for the tag.
	ImageDigest string
	State       string
	Health      string
	HealthAt    *time.Time
	// UnhealthyAt is the last time the instance was seen unhealthy.
	UnhealthyAt   *time.Time
	FailureReason string
	RequestedAt   time.Time
	UpdatedAt     time.Time
	// RestartSeq counts restarts that registered the job again because no
	// allocation was running; it is the job meta sanholo.restart.
	RestartSeq int
	// RestartRequestedAt is when the control plane last restarted the bot
	// (a rotated token, a retry, an admin restart). Nomad counts such a
	// restart like a crash, so a soak must not read it as one.
	RestartRequestedAt *time.Time
}

const instanceCols = `tenant_id, job_id, image_version, image_digest, state, last_health, last_health_at, last_unhealthy_at, failure_reason, requested_at, updated_at, restart_seq, restart_requested_at`

func scanInstance(row pgx.Row) (*Instance, error) {
	var i Instance
	err := row.Scan(&i.TenantID, &i.JobID, &i.ImageVersion, &i.ImageDigest, &i.State, &i.Health, &i.HealthAt, &i.UnhealthyAt, &i.FailureReason, &i.RequestedAt, &i.UpdatedAt, &i.RestartSeq, &i.RestartRequestedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("provision: read instance: %w", err)
	}
	return &i, nil
}

// Get returns the tenant's instance, nil when there is none (not
// provisioned). It reads the database only.
func (s *Service) Get(ctx context.Context, tenantID string) (*Instance, error) {
	if s == nil {
		return nil, nil
	}
	return scanInstance(s.pool.QueryRow(ctx, `SELECT `+instanceCols+` FROM tenant_instances WHERE tenant_id = $1`, tenantID))
}

// Start registers the tenant's job (if Nomad does not already have exactly
// this one) and records the instance as pending. It is safe to repeat: an
// instance that is already running stays running, and an unchanged job is
// not registered again. A new instance starts at the fleet target (the
// release the updater last finished rolling out; the configured version
// before any release exists), and so does an instance that was stopped. A
// running or failed one keeps the version it has, which belongs to the
// updater.
func (s *Service) Start(ctx context.Context, tenantID string) error {
	return s.start(ctx, tenantID, false)
}

func (s *Service) start(ctx context.Context, tenantID string, force bool) error {
	if !s.Enabled() {
		return ErrDisabled
	}
	if !tenant.Valid(tenantID) {
		return ErrNotFound
	}
	unlock, err := s.lockTenant(ctx, tenantID)
	if err != nil {
		return err
	}
	defer unlock()
	return s.startLocked(ctx, tenantID, force)
}

// startLocked is start for a caller that holds the tenant's lock.
func (s *Service) startLocked(ctx context.Context, tenantID string, force bool) error {
	var status string
	var hasGame bool
	err := s.pool.QueryRow(ctx, `
		SELECT t.status, EXISTS (SELECT 1 FROM game_accounts g WHERE g.tenant_id = t.id)
		FROM tenants t WHERE t.id = $1`, tenantID).Scan(&status, &hasGame)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrNotFound
	case err != nil:
		return fmt.Errorf("provision: tenant: %w", err)
	case status != "pending" && status != "active":
		return ErrNotAllowed
	case !hasGame:
		return ErrNoGameAccount
	}
	version, digest := s.version, ""
	cur, err := s.Get(ctx, tenantID)
	if err != nil {
		return err
	}
	tv, td, hasTarget, err := s.target(ctx)
	if err != nil {
		return err
	}
	switch {
	case cur != nil && cur.State != StateStopped:
		version, digest = cur.ImageVersion, cur.ImageDigest
	case hasTarget:
		version, digest = tv, td
	case cur != nil:
		version, digest = cur.ImageVersion, cur.ImageDigest
	}
	restartSeq := 0
	if cur != nil {
		restartSeq = cur.RestartSeq
	}
	job, err := nomad.BuildJobRestart(s.set, tenantID, version, digest, restartSeq)
	if err != nil {
		return fmt.Errorf("provision: %w", err)
	}
	now := s.now()
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO tenant_instances (tenant_id, job_id, image_version, image_digest, state, requested_at, created_at, updated_at)
		VALUES ($1, $2, $3, $5, 'pending', $4, $4, $4)
		ON CONFLICT (tenant_id) DO UPDATE SET
		    job_id = EXCLUDED.job_id, image_version = EXCLUDED.image_version, image_digest = EXCLUDED.image_digest,
		    state          = CASE WHEN tenant_instances.state = 'running' THEN 'running' ELSE 'pending' END,
		    last_health    = CASE WHEN tenant_instances.state = 'running' THEN tenant_instances.last_health ELSE 'unknown' END,
		    failure_reason = CASE WHEN tenant_instances.state = 'running' THEN tenant_instances.failure_reason ELSE '' END,
		    requested_at   = CASE WHEN tenant_instances.state = 'running' THEN tenant_instances.requested_at ELSE EXCLUDED.requested_at END,
		    updated_at     = EXCLUDED.updated_at`,
		tenantID, job.ID, version, now, digest); err != nil {
		return fmt.Errorf("provision: record instance: %w", err)
	}
	return s.register(ctx, tenantID, job, force, "could not register the job")
}

// errVolume marks a failure to create the tenant's volume.
var errVolume = errors.New("data volume")

// register creates the tenant's volume if it is missing, then registers the
// job. The volume comes first: a job whose volume does not exist is accepted
// by Nomad but never placed. Any failure is recorded on the instance.
func (s *Service) register(ctx context.Context, tenantID string, job nomad.Job, force bool, what string) error {
	if err := s.finishVolumeDeletion(ctx, tenantID); err != nil {
		err = fmt.Errorf("%w: %w", errVolume, err)
		s.fail(ctx, tenantID, reasonFor(err))
		s.log.Warn("provision: could not delete the previous data volume", "tenant", tenantID, "error", err)
		return fmt.Errorf("provision: volume: %w", err)
	}
	if _, err := s.nomad.EnsureHostVolume(ctx, tenantID, s.vol); err != nil {
		err = fmt.Errorf("%w: %w", errVolume, err)
		s.fail(ctx, tenantID, reasonFor(err))
		s.log.Warn("provision: could not create the data volume", "tenant", tenantID, "error", err)
		return fmt.Errorf("provision: volume: %w", err)
	}
	if err := s.ensureJob(ctx, job, force); err != nil {
		s.fail(ctx, tenantID, reasonFor(err))
		s.log.Warn("provision: "+what, "tenant", tenantID, "error", err)
		return fmt.Errorf("provision: register: %w", err)
	}
	return nil
}

// lockRetry is how long a caller waits before trying a tenant's lock again.
const lockRetry = 20 * time.Millisecond

// lockTenant serialises the operations that register or change a tenant's
// job (start, update, restart, stop), across processes: a Start that read the
// instance before an Update must not register its old version after the
// Update's. It is a PostgreSQL advisory lock on a connection of its own,
// released by unlock or, if anything goes wrong, by closing the session.
//
// Lock order, for anything that takes more than one: this advisory lock
// first, then the tenants row (SELECT ... FOR UPDATE). Nothing takes the row
// and then this lock.
//
// A waiter holds nothing: it asks for a free slot and tries the lock without
// blocking, and on either failure gives the connection and the slot back and
// tries again shortly. A tenant whose holder is slow therefore costs the
// others nothing but that one slot.
func (s *Service) lockTenant(ctx context.Context, tenantID string) (unlock func(), err error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		unlock, err := s.tryLockTenant(ctx, tenantID)
		if err != nil || unlock != nil {
			return unlock, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(lockRetry):
		}
	}
}

// tryLockTenant returns (nil, nil) when no slot is free or another session
// holds the lock.
func (s *Service) tryLockTenant(ctx context.Context, tenantID string) (func(), error) {
	select {
	case s.lockSem <- struct{}{}:
	default:
		return nil, nil
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		<-s.lockSem
		return nil, fmt.Errorf("provision: lock: %w", err)
	}
	const key = `hashtextextended($1, 5381)`
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(`+key+`)`, tenantID).Scan(&got); err != nil {
		// The lock may have been granted as the context ended; closing the
		// session releases it for sure.
		_ = conn.Hijack().Close(context.WithoutCancel(ctx))
		<-s.lockSem
		return nil, fmt.Errorf("provision: lock: %w", err)
	}
	if !got {
		conn.Release()
		<-s.lockSem
		return nil, nil
	}
	return func() {
		if _, err := conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(`+key+`)`, tenantID); err != nil {
			_ = conn.Hijack().Close(context.WithoutCancel(ctx))
		} else {
			conn.Release()
		}
		<-s.lockSem
	}, nil
}

// LockTenant is lockTenant for a caller that must hold the tenant's lock
// across several steps (disconnect: stop the job, then delete the rows) and
// uses StopLocked meanwhile.
func (s *Service) LockTenant(ctx context.Context, tenantID string) (unlock func(), err error) {
	if !s.Enabled() {
		return nil, ErrDisabled
	}
	if !tenant.Valid(tenantID) {
		return nil, ErrNotFound
	}
	return s.lockTenant(ctx, tenantID)
}

// ensureJob registers job unless Nomad already has exactly that spec and it
// is not stopped (or force). The registration names the modify index it was
// based on; one conflict is retried against a fresh read.
func (s *Service) ensureJob(ctx context.Context, job nomad.Job, force bool) error {
	for range 2 {
		var index uint64
		info, err := s.nomad.Job(ctx, job.ID)
		switch {
		case errors.Is(err, nomad.ErrNotFound):
		case err != nil:
			return err
		case !force && !info.Stop && info.Meta[nomad.MetaSpec] == job.Meta[nomad.MetaSpec]:
			return nil
		default:
			index = info.ModifyIndex
		}
		err = s.nomad.Register(ctx, job, nomad.RegisterOptions{ModifyIndex: index})
		if !errors.Is(err, nomad.ErrConflict) {
			return err
		}
	}
	return nomad.ErrConflict
}

// unreachable: no response arrived at all.
func unreachable(err error) bool {
	var ne *nomad.Error
	return errors.As(err, &ne) && ne.Status == 0 && errors.Is(err, nomad.ErrUnavailable)
}

func reasonFor(err error) string {
	switch {
	case errors.Is(err, nomad.ErrDenied):
		return ReasonDenied
	case errors.Is(err, errVolume) && !unreachable(err):
		// Nomad answered but would not make the volume (a 500 with no node
		// that fits, a rejected request).
		return ReasonVolume
	case errors.Is(err, nomad.ErrUnavailable), errors.Is(err, nomad.ErrDisabled):
		return ReasonUnreachable
	}
	return ReasonRegister
}

// fail records a failure, best effort.
func (s *Service) fail(ctx context.Context, tenantID, reason string) {
	ctx = context.WithoutCancel(ctx)
	if _, err := s.pool.Exec(ctx, `
		UPDATE tenant_instances SET state = 'failed', last_health = 'unknown', last_health_at = $2, failure_reason = $3, updated_at = $2
		WHERE tenant_id = $1 AND state <> 'stopped'`, tenantID, s.now(), reason); err != nil {
		s.log.Error("provision: could not record a failure", "tenant", tenantID, "error", err)
	}
}

// Trigger starts the tenant's instance in the background and follows it to
// healthy, failed or the timeout. It returns at once; a second call while
// one is running does nothing. The customer sees the state on the account
// page.
func (s *Service) Trigger(tenantID string) { s.trigger(tenantID, false) }

// TriggerRestart is Trigger for a rotated token, or a retry of a failed
// instance: a running instance is restarted so it reads the new token from
// qm, and a failed one is registered again even if Nomad holds the same job.
// Asked for while a start is in flight, the restart is queued and runs when
// that start is done, so a token rotated during a start is never missed.
func (s *Service) TriggerRestart(tenantID string) { s.trigger(tenantID, true) }

func (s *Service) trigger(tenantID string, restart bool) {
	if !s.Enabled() || !tenant.Valid(tenantID) {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if s.inflight[tenantID] {
		if restart {
			s.queued[tenantID] = true
		}
		s.mu.Unlock()
		return
	}
	s.inflight[tenantID] = true
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		for {
			if err := s.runTrigger(tenantID, restart); err != nil {
				if !errors.Is(err, ErrNoGameAccount) && !errors.Is(err, ErrNotAllowed) {
					s.log.Warn("provision: start failed", "tenant", tenantID, "error", err)
				}
			} else {
				s.supervise(s.ctx, tenantID)
			}
			s.mu.Lock()
			if s.queued[tenantID] && !s.closed && s.ctx.Err() == nil {
				delete(s.queued, tenantID)
				s.mu.Unlock()
				restart = true
				continue
			}
			delete(s.queued, tenantID)
			delete(s.inflight, tenantID)
			s.mu.Unlock()
			return
		}
	}()
}

func (s *Service) runTrigger(tenantID string, restart bool) error {
	if inst, err := s.Get(s.ctx, tenantID); restart && err == nil && inst != nil && inst.State != StateStopped {
		return s.restart(s.ctx, tenantID, true)
	}
	return s.Start(s.ctx, tenantID)
}

func (s *Service) hasQueued(tenantID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.queued[tenantID]
}

// supervise follows a pending instance until it is no longer pending, or
// until a restart is queued behind it.
//
// It also ends when the instance is gone (the game account was disconnected
// meanwhile) and after the provisioning timeout: a supervisor that never ended
// would keep the tenant "in flight" and nothing could start it again.
func (s *Service) supervise(ctx context.Context, tenantID string) {
	deadline := time.Now().Add(s.timeout + 10*s.poll)
	for {
		inst, err := s.Refresh(ctx, tenantID)
		if err != nil && ctx.Err() != nil {
			return
		}
		if errors.Is(err, ErrNotFound) || (err == nil && inst == nil) {
			return
		}
		if inst != nil && inst.State != StatePending {
			return
		}
		if time.Now().After(deadline) {
			return
		}
		if s.hasQueued(tenantID) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.poll):
		}
	}
}

// Next is the state machine: given the instance and what Nomad reports, the
// state, health and failure reason to record. Exported for the table test.
func Next(inst Instance, st nomad.Status, now time.Time, timeout time.Duration) (state, health, reason string) {
	expired := now.Sub(inst.RequestedAt) > timeout
	switch st.Phase {
	case nomad.PhaseStopped:
		return StateStopped, HealthUnknown, ""
	case nomad.PhaseAbsent:
		return StateFailed, HealthUnknown, ReasonMissing
	case nomad.PhaseHealthy:
		return StateRunning, HealthHealthy, ""
	case nomad.PhaseFailed:
		return StateFailed, HealthUnhealthy, ReasonDeployFailed
	case nomad.PhaseUnhealthy:
		switch {
		case inst.State == StateFailed:
			return inst.State, inst.Health, inst.FailureReason
		case inst.State == StatePending && expired:
			return StateFailed, HealthUnhealthy, ReasonTimeout
		}
		return inst.State, HealthUnhealthy, ""
	}
	// Pending: registered, no healthy allocation yet.
	switch {
	case inst.State == StateFailed:
		return inst.State, inst.Health, inst.FailureReason
	case inst.State == StatePending && expired:
		return StateFailed, HealthUnknown, ReasonTimeout
	}
	return inst.State, HealthStarting, ""
}

// Refresh reads Nomad's view of the instance and records it. A stopped
// instance is left alone. When Nomad cannot be read the record is unchanged,
// except that a pending instance past its timeout is failed.
func (s *Service) Refresh(ctx context.Context, tenantID string) (*Instance, error) {
	if !s.Enabled() {
		return nil, ErrDisabled
	}
	inst, err := s.Get(ctx, tenantID)
	if err != nil || inst == nil {
		if err == nil {
			err = ErrNotFound
		}
		return nil, err
	}
	if inst.State == StateStopped {
		return inst, nil
	}
	now := s.now()
	st, err := s.nomad.StatusLive(ctx, inst.JobID)
	if err != nil {
		if inst.State == StatePending && now.Sub(inst.RequestedAt) > s.timeout {
			s.fail(ctx, tenantID, reasonFor(err))
			return s.Get(ctx, tenantID)
		}
		return inst, fmt.Errorf("provision: status: %w", err)
	}
	state, health, reason := Next(*inst, st, now, s.timeout)
	if state == inst.State && health == inst.Health && reason == inst.FailureReason && inst.HealthAt != nil {
		// Nothing changed; keep updated_at meaningful and skip the write
		// except for the health timestamp, which the reconciler refreshes.
		if now.Sub(*inst.HealthAt) < time.Minute {
			return inst, nil
		}
	}
	// Compare-and-set on what was read, so a stop or restart that happened
	// meanwhile is not overwritten.
	if _, err := s.pool.Exec(ctx, `
		UPDATE tenant_instances SET state = $2, last_health = $3, last_health_at = $4, failure_reason = $5, updated_at = $4,
		    last_unhealthy_at = CASE WHEN $3 = 'unhealthy' THEN $4 ELSE last_unhealthy_at END
		WHERE tenant_id = $1 AND state = $6 AND requested_at = $7`,
		tenantID, state, health, now, reason, inst.State, inst.RequestedAt); err != nil {
		return nil, fmt.Errorf("provision: record status: %w", err)
	}
	return s.Get(ctx, tenantID)
}

// Run refreshes every pending, running and failed instance each interval
// until ctx ends: it finishes what a restarted control plane left pending,
// notices an instance that stopped being healthy, and lets a failed one
// recover when Nomad does.
func (s *Service) Run(ctx context.Context, interval time.Duration) {
	if !s.Enabled() {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Reconcile(ctx)
		}
	}
}

// Reconcile refreshes all non-stopped instances once.
func (s *Service) Reconcile(ctx context.Context) {
	rows, err := s.pool.Query(ctx, `SELECT tenant_id FROM tenant_instances WHERE state <> 'stopped' ORDER BY tenant_id`)
	if err != nil {
		s.log.Error("provision: reconcile", "error", err)
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	fixed := 0
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		s.mu.Lock()
		busy := s.inflight[id]
		s.mu.Unlock()
		if busy {
			continue
		}
		if _, err := s.Refresh(ctx, id); err != nil && !errors.Is(err, ErrNotFound) {
			s.log.Warn("provision: refresh failed", "tenant", id, "error", err)
			continue
		}
		if fixed < s.drift {
			if s.converge(ctx, id) {
				fixed++
			}
		}
	}
}

// Stop stops the tenant's job (purge also forgets it) and records the
// instance as stopped. Safe to repeat. It is the door disconnect and delete
// go through.
func (s *Service) Stop(ctx context.Context, tenantID string, purge bool) error {
	if !s.Enabled() {
		return ErrDisabled
	}
	if !tenant.Valid(tenantID) {
		return ErrNotFound
	}
	unlock, err := s.lockTenant(ctx, tenantID)
	if err != nil {
		return err
	}
	defer unlock()
	return s.StopLocked(ctx, tenantID, purge)
}

// StopLocked is Stop for a caller that holds the tenant's lock (LockTenant).
func (s *Service) StopLocked(ctx context.Context, tenantID string, purge bool) error {
	if !s.Enabled() {
		return ErrDisabled
	}
	jobID, err := nomad.JobID(tenantID)
	if err != nil {
		return ErrNotFound
	}
	if err := s.nomad.Stop(ctx, jobID, purge); err != nil {
		return fmt.Errorf("provision: stop: %w", err)
	}
	if _, err := s.pool.Exec(ctx, `
		UPDATE tenant_instances SET state = 'stopped', last_health = 'unknown', failure_reason = '', updated_at = $2
		WHERE tenant_id = $1`, tenantID, s.now()); err != nil {
		return fmt.Errorf("provision: record stop: %w", err)
	}
	return nil
}

// querier is what both a pool and a transaction offer.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// markPending resets the instance to pending with a fresh timeout window.
// ErrNotFound when the tenant has no instance, ErrNotAllowed when it is
// stopped on purpose.
func (s *Service) markPending(ctx context.Context, q querier, tenantID string) error {
	tag, err := q.Exec(ctx, `
		UPDATE tenant_instances SET state = 'pending', last_health = 'unknown', failure_reason = '', requested_at = $2, updated_at = $2
		WHERE tenant_id = $1 AND state <> 'stopped'`, tenantID, s.now())
	if err != nil {
		return fmt.Errorf("provision: mark pending: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var state string
	switch err := q.QueryRow(ctx, `SELECT state FROM tenant_instances WHERE tenant_id = $1`, tenantID).Scan(&state); {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrNotFound
	case err != nil:
		return fmt.Errorf("provision: mark pending: %w", err)
	}
	return ErrNotAllowed
}

// PrepareRestart is the database half of an admin restart, run inside the
// admin action's transaction so that the audit row and the new state commit
// together. Restart is the Nomad half, run after the commit.
func (s *Service) PrepareRestart(ctx context.Context, tx pgx.Tx, tenantID string) error {
	if !s.Enabled() {
		return ErrDisabled
	}
	return s.markPending(ctx, tx, tenantID)
}

// Restart restarts the tenant's bot: the running allocation in place, or,
// when there is none, a fresh registration of the job. Call it after
// PrepareRestart has committed. A failure leaves the instance failed.
func (s *Service) Restart(ctx context.Context, tenantID string) error {
	err := s.restart(ctx, tenantID, false)
	if err == nil {
		s.Trigger(tenantID) // follow it to healthy
	}
	return err
}

func (s *Service) restart(ctx context.Context, tenantID string, mark bool) error {
	if !s.Enabled() {
		return ErrDisabled
	}
	jobID, err := nomad.JobID(tenantID)
	if err != nil {
		return ErrNotFound
	}
	unlock, err := s.lockTenant(ctx, tenantID)
	if err != nil {
		return err
	}
	defer unlock()
	if mark {
		if err := s.markPending(ctx, s.pool, tenantID); err != nil {
			return err
		}
	}
	// Recorded before Nomad is asked, so that an updater that sees the restart
	// in the allocation's counter can already see why.
	if _, err := s.pool.Exec(ctx, `UPDATE tenant_instances SET restart_requested_at = $2 WHERE tenant_id = $1`, tenantID, s.now()); err != nil {
		return fmt.Errorf("provision: restart: %w", err)
	}
	st, err := s.nomad.Status(ctx, jobID)
	if err != nil {
		s.fail(ctx, tenantID, reasonFor(err))
		return fmt.Errorf("provision: restart: %w", err)
	}
	if st.AllocID != "" {
		if err := s.nomad.RestartAllocation(ctx, st.AllocID); err != nil {
			s.fail(ctx, tenantID, reasonFor(err))
			return fmt.Errorf("provision: restart: %w", err)
		}
		return nil
	}
	// No allocation to restart (a failed or dead job). Registering the same
	// spec again is only a new evaluation on real Nomad, which does not
	// revive it, so the restart counter in the job meta is bumped: a new job
	// version, a new deployment, a replaced allocation. The counter is stored
	// and carried by every later render, and is outside the spec hash.
	if _, err := s.pool.Exec(ctx, `UPDATE tenant_instances SET restart_seq = restart_seq + 1 WHERE tenant_id = $1`, tenantID); err != nil {
		return fmt.Errorf("provision: restart: %w", err)
	}
	return s.startLocked(ctx, tenantID, true)
}

// Upstream is host:port of the tenant's running allocation, where the proxy
// connects. ErrNotRunning when the tenant has no instance that should be
// running; ErrStarting when it should be but Nomad shows no running
// allocation of the current version; any other error is Nomad being
// unreadable. It reads Nomad on every call; the caller caches briefly.
func (s *Service) Upstream(ctx context.Context, tenantID string) (string, error) {
	if !s.Enabled() {
		return "", ErrNotRunning
	}
	inst, err := s.Get(ctx, tenantID)
	if err != nil {
		return "", err
	}
	if inst == nil || (inst.State != StatePending && inst.State != StateRunning) {
		return "", ErrNotRunning
	}
	st, err := s.nomad.Status(ctx, inst.JobID)
	if err != nil {
		return "", fmt.Errorf("provision: upstream: %w", err)
	}
	switch st.Phase {
	case nomad.PhaseAbsent, nomad.PhaseStopped, nomad.PhaseFailed:
		return "", ErrNotRunning
	}
	if st.Address == "" {
		return "", ErrStarting
	}
	return st.Address, nil
}
