package metrics

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/lhaig/sanholo-cloud/internal/secrets"
)

// QMProbe checks qm's health on an interval and keeps the last result, so a
// scrape never waits on qm. Sealed and unreachable are told apart: a sealed
// qm needs the operator's unseal shares, an unreachable one a network or
// process fix.
type QMProbe struct {
	check func(context.Context) error
	now   func() time.Time

	mu      sync.Mutex
	checked bool
	up      bool
	sealed  bool
	at      time.Time
}

// NewQMProbe returns a probe around check (secrets.Client.Ready in
// production). now may be nil.
func NewQMProbe(check func(context.Context) error, now func() time.Time) *QMProbe {
	if now == nil {
		now = time.Now
	}
	return &QMProbe{check: check, now: now}
}

// Probe runs one check, bounded to 10 seconds.
func (p *QMProbe) Probe(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err := p.check(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.checked, p.up, p.sealed, p.at = true, err == nil, errors.Is(err, secrets.ErrSealed), p.now()
}

// Run probes now and then every interval until ctx ends.
func (p *QMProbe) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		p.Probe(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// write emits nothing before the first probe: a missing series is what
// the "qm probe stale" alert looks for, not a made-up value.
func (p *QMProbe) write(e *enc) {
	p.mu.Lock()
	checked, up, sealed, at := p.checked, p.up, p.sealed, p.at
	p.mu.Unlock()
	if !checked {
		return
	}
	e.family("sanholo_cloud_qm_up", "gauge", "1 when the last qm health check found qm initialized and unsealed.")
	e.sample("sanholo_cloud_qm_up", b2f(up))
	e.family("sanholo_cloud_qm_sealed", "gauge", "1 when the last qm health check found qm sealed.")
	e.sample("sanholo_cloud_qm_sealed", b2f(sealed))
	e.family("sanholo_cloud_qm_last_probe_timestamp_seconds", "gauge", "Unix time of the last qm health check.")
	e.sample("sanholo_cloud_qm_last_probe_timestamp_seconds", float64(at.Unix()))
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
