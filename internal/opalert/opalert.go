// Package opalert turns Alertmanager webhook notifications into pushes to
// the operator (US-023), through push.Service.NotifyOperator.
//
// Alertmanager groups, waits and repeats; this package only makes the result
// safe to push: one push per alert per state change, a reminder for an alert
// that stays firing no more often than Reminder, a "Resolved" push only for
// an alert whose firing push went out, and a bound on the alerts read per
// request. NotifyOperator's own budget (push.OperatorHourlyCap, critical
// alerts exempt) bounds the rest; an alert refused over it is not remembered,
// so Alertmanager's next send of it pushes it.
package opalert

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lhaig/sanholo-cloud/internal/metrics"
	"github.com/lhaig/sanholo-cloud/internal/push"
)

const (
	// MaxBody is the largest webhook body read.
	MaxBody = 256 << 10
	// MaxAlerts is the most alerts read from one notification. Alertmanager
	// is configured with max_alerts below this (deploy/alertmanager.yml).
	MaxAlerts = 50
	// Reminder is how long a firing alert stays quiet after its push.
	Reminder = 3 * time.Hour
	// forget drops what is remembered about an alert after this long.
	forget = 24 * time.Hour
	// maxRemembered bounds the memory of alerts seen.
	maxRemembered = 1024
	// fallbackKind is used when an alert has no usable kind label.
	fallbackKind = "platform"
)

// ErrBadNotification is a body that is not an Alertmanager v4 notification.
var ErrBadNotification = errors.New("opalert: not an Alertmanager notification")

// Notifier is push.Service. severity is the alert's "severity" label.
type Notifier interface {
	NotifyOperator(kind, severity, text string) error
}

// Notification is the part of Alertmanager's webhook body that is read.
type Notification struct {
	Version string  `json:"version"`
	Alerts  []Alert `json:"alerts"`
}

// Alert is one alert of a notification.
type Alert struct {
	Status      string            `json:"status"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	Fingerprint string            `json:"fingerprint"`
}

// Intake remembers which alerts it pushed. It is safe for concurrent use.
type Intake struct {
	n   Notifier
	now func() time.Time

	mu   sync.Mutex
	seen map[string]seen

	pushed, deduped, failed, truncated, capped atomic.Uint64
}

type seen struct {
	firing bool
	at     time.Time
}

// New returns an Intake pushing through n. now may be nil.
func New(n Notifier, now func() time.Time) *Intake {
	if now == nil {
		now = time.Now
	}
	return &Intake{n: n, now: now, seen: map[string]seen{}}
}

// Stats returns the counters for the metrics endpoint.
func (in *Intake) Stats() metrics.AlertStats {
	return metrics.AlertStats{
		Pushed: in.pushed.Load(), Deduped: in.deduped.Load(),
		Failed: in.failed.Load(), Truncated: in.truncated.Load(), Capped: in.capped.Load(),
	}
}

// Parse reads a webhook body.
func Parse(body []byte) (Notification, error) {
	var n Notification
	if len(body) == 0 || len(body) > MaxBody || json.Unmarshal(body, &n) != nil || n.Version != "4" {
		return Notification{}, ErrBadNotification
	}
	return n, nil
}

// Handle pushes what n calls for. It returns an error when a push could not
// be queued; the alerts already pushed are remembered, so Alertmanager's
// retry of the same notification pushes only what was missed.
func (in *Intake) Handle(n Notification) error {
	alerts := n.Alerts
	if len(alerts) > MaxAlerts {
		for range alerts[MaxAlerts:] {
			in.truncated.Add(1)
		}
		alerts = alerts[:MaxAlerts]
	}
	var errs []error
	for _, a := range alerts {
		if err := in.one(a); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (in *Intake) one(a Alert) error {
	firing := a.Status != "resolved"
	key := fingerprint(a)
	now := in.now()

	in.mu.Lock()
	prev, known := in.seen[key]
	switch {
	case firing && known && prev.firing && now.Sub(prev.at) < Reminder,
		!firing && (!known || !prev.firing):
		// Already pushed and not due a reminder; or resolved without a
		// firing push to resolve (nothing to tell the operator).
		in.mu.Unlock()
		in.deduped.Add(1)
		return nil
	}
	// Claim the push before sending, so a concurrent copy of the same
	// notification (an Alertmanager HA pair) is deduplicated; undo on failure.
	in.seen[key] = seen{firing: firing, at: now}
	in.prune(now)
	in.mu.Unlock()

	text := summary(a)
	if !firing {
		text = "Resolved: " + text
	}
	if err := in.n.NotifyOperator(kind(a), a.Labels["severity"], text); err != nil {
		in.mu.Lock()
		if known {
			in.seen[key] = prev
		} else {
			delete(in.seen, key)
		}
		in.mu.Unlock()
		if errors.Is(err, push.ErrCapped) {
			// Over the operator's hourly budget: not pushed and not
			// remembered, so Alertmanager's next send pushes it. No error:
			// an immediate retry would meet the same cap.
			in.capped.Add(1)
			return nil
		}
		in.failed.Add(1)
		return err
	}
	in.pushed.Add(1)
	return nil
}

// prune bounds the memory: over maxRemembered, resolved and day-old entries
// go. Called with mu held.
func (in *Intake) prune(now time.Time) {
	if len(in.seen) <= maxRemembered {
		return
	}
	for k, s := range in.seen {
		if now.Sub(s.at) >= forget || !s.firing {
			delete(in.seen, k)
		}
	}
}

var kindRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

// kind is the alert's "kind" label (the rules set one per alert) when it is
// a valid operator kind, else fallbackKind.
func kind(a Alert) string {
	if k := a.Labels["kind"]; kindRE.MatchString(k) {
		return k
	}
	return fallbackKind
}

// summary is the summary annotation, else the alert name, else a fixed
// phrase. NotifyOperator cleans and bounds it.
func summary(a Alert) string {
	for _, s := range []string{a.Annotations["summary"], a.Labels["alertname"]} {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	return "Platform alert"
}

// fingerprint is Alertmanager's fingerprint, or a hash of the labels when
// it is missing (older Alertmanager).
func fingerprint(a Alert) string {
	if a.Fingerprint != "" {
		return "f:" + a.Fingerprint
	}
	h := sha256.New()
	for _, k := range slices.Sorted(maps.Keys(a.Labels)) {
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write([]byte(a.Labels[k]))
		h.Write([]byte{0})
	}
	return "l:" + hex.EncodeToString(h.Sum(nil))
}
