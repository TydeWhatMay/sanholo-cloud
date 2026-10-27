package push

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
)

// HourlyCap is the most alerts pushed to one tenant per window. The window is
// fixed: it opens at the first counted alert and closes one hour later, when
// the count starts again. Over the cap the tenant gets one CapNotice push and
// nothing else until the window closes.
const (
	HourlyCap = 30
	Window    = time.Hour
	// CapNotice is the single push sent when the cap is hit.
	CapNotice = "More alerts are waiting in your Bridge."
)

// Bounds on what is accepted and pushed.
const (
	MaxTextRunes = 280 // the bot cuts to this
	maxKeyRunes  = 120
	maxLinkLen   = 2048
	bodyRunes    = 240 // what the service worker shows
	keyRunes     = 64  // the worker's notification tag
	// MaxBody is the largest alert request body read.
	MaxBody = 8 << 10
)

// Kinds are the alert kinds the bot sends (internal/alert in the client).
var Kinds = map[string]bool{
	"asteroid": true, "location-event": true, "device-paused": true, "needs-help": true,
	"game-event": true, "decision": true, "job-blocked": true, "job-failed": true,
	"bot-stuck": true, "decision-expiring": true, "alert-cap": true, "test": true,
}

var severities = map[string]bool{"info": true, "warning": true, "critical": true}

// ErrBadAlert is an alert request body that is not acceptable.
var ErrBadAlert = errors.New("push: bad alert")

// ErrQueueFull means the delivery queue has no room; the alert is dropped.
var ErrQueueFull = errors.New("push: delivery queue full")

// Alert is the bot's alert as it posts it.
type Alert struct {
	Kind     string `json:"kind"`
	Severity string `json:"severity"`
	Text     string `json:"text"`
	Key      string `json:"key"`
	At       string `json:"at"`
	Link     string `json:"link"`
}

// ParseAlert reads and checks an alert body: a known kind and severity, text
// within the bot's own bound, a parseable time. Unknown fields are ignored.
func ParseAlert(body []byte) (Alert, error) {
	var a Alert
	if len(body) == 0 || len(body) > MaxBody || json.Unmarshal(body, &a) != nil {
		return Alert{}, ErrBadAlert
	}
	if !Kinds[a.Kind] || !severities[a.Severity] || a.Text == "" ||
		len([]rune(a.Text)) > MaxTextRunes || len([]rune(a.Key)) > maxKeyRunes || len(a.Link) > maxLinkLen {
		return Alert{}, ErrBadAlert
	}
	if a.At != "" {
		if _, err := time.Parse(time.RFC3339, a.At); err != nil {
			return Alert{}, ErrBadAlert
		}
	}
	return a, nil
}

// Payload is what the service worker receives: {title, body, link, key, kind}
// and nothing else.
type Payload struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Link  string `json:"link"`
	Key   string `json:"key"`
	Kind  string `json:"kind"`
}

// linkParams are the Bridge deep-link parameters a push may keep.
var linkParams = []string{"alert", "job", "sys"}

// BuildPayload makes the push for a. The link is rebuilt on the tenant's own
// origin from the bot's link: the host the bot names is never used, so a push
// can only open the customer's Bridge.
func BuildPayload(a Alert, origin string) Payload {
	return Payload{
		Title: "SanHolo: " + strings.ReplaceAll(a.Kind, "-", " "),
		Body:  clean(a.Text, bodyRunes),
		Link:  deepLink(origin, a.Link),
		Key:   clean(a.Key, keyRunes),
		Kind:  a.Kind,
	}
}

func deepLink(origin, raw string) string {
	base := strings.TrimRight(origin, "/") + "/"
	u, err := url.Parse(raw)
	if raw == "" || err != nil {
		return base
	}
	q := url.Values{}
	for _, k := range linkParams {
		if v := u.Query().Get(k); v != "" && len(v) <= 200 {
			q.Set(k, v)
		}
	}
	if len(q) == 0 {
		return base
	}
	return base + "?" + q.Encode()
}

// clean drops control characters (a newline becomes a space) and cuts to n
// runes.
func clean(s string, n int) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteRune(' ')
		case unicode.IsControl(r):
		default:
			b.WriteRune(r)
		}
	}
	out := []rune(strings.TrimSpace(b.String()))
	if len(out) > n {
		out = append(out[:n-1], '…')
	}
	return string(out)
}

// Config configures a Service.
type Config struct {
	Store  *Store
	Sender Sender
	// TenantOrigin is the Bridge origin of a tenant (https://id.domain).
	TenantOrigin func(tenantID string) string
	Logger       *slog.Logger
	Now          func() time.Time
	// After schedules f after d (time.AfterFunc); tests replace it.
	After     func(d time.Duration, f func())
	Workers   int
	QueueSize int
	// MaxAttempts is deliveries tried per subscription on 429/5xx. Default 3.
	MaxAttempts int
}

// Service delivers alerts. Submit never waits for a push service.
type Service struct {
	cfg   Config
	queue chan job
	wg    sync.WaitGroup // jobs queued, running or waiting to retry

	mu      sync.Mutex
	windows map[string]*window

	stats counters
}

// Stats are the delivery counters since start, for the metrics endpoint.
// Every field only grows.
type Stats struct {
	// Delivered: messages a push service accepted (one per subscription).
	Delivered uint64
	// Gone: subscriptions the push service reported gone (removed).
	Gone uint64
	// Refused: messages refused for good (4xx other than 404/410/429).
	Refused uint64
	// GaveUp: messages dropped after MaxAttempts retries.
	GaveUp uint64
	// CapNotices: times a bucket hit HourlyCap and got the single notice.
	CapNotices uint64
	// CapDropped: alerts dropped because their bucket was over the cap.
	CapDropped uint64
	// QueueFull: alerts refused because the delivery queue had no room.
	QueueFull uint64
}

type counters struct {
	delivered, gone, refused, gaveUp, capNotices, capDropped, queueFull atomic.Uint64
}

// Stats returns the delivery counters.
func (s *Service) Stats() Stats {
	c := &s.stats
	return Stats{
		Delivered: c.delivered.Load(), Gone: c.gone.Load(), Refused: c.refused.Load(), GaveUp: c.gaveUp.Load(),
		CapNotices: c.capNotices.Load(), CapDropped: c.capDropped.Load(), QueueFull: c.queueFull.Load(),
	}
}

type window struct {
	start   time.Time
	n       int
	noticed bool
}

type job struct {
	tenantID string // "" for an operator alert
	alert    Alert
}

// New returns a Service; call Start to run its workers.
func New(cfg Config) *Service {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.After == nil {
		cfg.After = func(d time.Duration, f func()) { time.AfterFunc(d, f) }
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 4
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 256
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 3
	}
	return &Service{cfg: cfg, queue: make(chan job, cfg.QueueSize), windows: map[string]*window{}}
}

// Start runs the workers until ctx ends.
func (s *Service) Start(ctx context.Context) {
	for range s.cfg.Workers {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case j := <-s.queue:
					s.process(ctx, j)
					s.wg.Done()
				}
			}
		}()
	}
}

// Wait blocks until every queued alert, and every retry waiting on a timer,
// has finished. Tests use it; so can a shutdown.
func (s *Service) Wait() { s.wg.Wait() }

// Submit queues an alert for tenantID's subscriptions. It does not block.
func (s *Service) Submit(tenantID string, a Alert) error {
	return s.enqueue(job{tenantID: tenantID, alert: a})
}

var operatorKind = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

// NotifyOperator pushes text to every subscription of a tenant that has an
// operator account (US-023). Operator pushes have a cap bucket of their own.
func (s *Service) NotifyOperator(kind, text string) error {
	if !operatorKind.MatchString(kind) || strings.TrimSpace(text) == "" {
		return ErrBadAlert
	}
	return s.enqueue(job{alert: Alert{Kind: kind, Severity: "critical", Text: clean(text, MaxTextRunes)}})
}

func (s *Service) enqueue(j job) error {
	s.wg.Add(1)
	select {
	case s.queue <- j:
		return nil
	default:
		s.wg.Done()
		s.stats.queueFull.Add(1)
		return ErrQueueFull
	}
}

type verdict int

const (
	send verdict = iota
	notice
	drop
)

// admit counts one alert against bucket's window.
func (s *Service) admit(bucket string) verdict {
	now := s.cfg.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.windows) > 1024 {
		for k, w := range s.windows {
			if now.Sub(w.start) >= Window {
				delete(s.windows, k)
			}
		}
	}
	w := s.windows[bucket]
	if w == nil || now.Sub(w.start) >= Window {
		w = &window{start: now}
		s.windows[bucket] = w
	}
	switch {
	case w.n < HourlyCap:
		w.n++
		return send
	case !w.noticed:
		w.noticed = true
		return notice
	}
	return drop
}

func (s *Service) process(ctx context.Context, j job) {
	var subs []stored
	var err error
	bucket := j.tenantID
	if j.tenantID == "" {
		bucket = "\x00operator"
		subs, err = s.cfg.Store.forOperators(ctx)
	} else {
		subs, err = s.cfg.Store.forTenant(ctx, j.tenantID)
	}
	if err != nil {
		s.cfg.Logger.Error("push: list subscriptions", "err", err)
		return
	}
	if len(subs) == 0 {
		return // nothing to deliver to; the cap is not spent
	}
	a := j.alert
	switch s.admit(bucket) {
	case drop:
		s.stats.capDropped.Add(1)
		return
	case notice:
		s.stats.capNotices.Add(1)
		a = Alert{Kind: "alert-cap", Severity: "warning", Text: CapNotice, Key: "alert-cap"}
	}
	for _, sub := range subs {
		p := BuildPayload(a, s.cfg.TenantOrigin(sub.TenantID))
		if a.Kind == "alert-cap" {
			p.Title = "SanHolo"
		}
		if j.tenantID == "" {
			p.Title = "SanHolo operator: " + strings.ReplaceAll(a.Kind, "-", " ")
		}
		body, _ := json.Marshal(p) // strings only
		s.deliver(ctx, sub, body, 1)
	}
}

// deliver sends one message to one subscription and handles the outcome.
func (s *Service) deliver(ctx context.Context, sub stored, body []byte, attempt int) {
	err := s.cfg.Sender.Send(ctx, sub.Sub, body)
	var re *RetryError
	switch {
	case err == nil:
		s.stats.delivered.Add(1)
	case errors.Is(err, ErrGone):
		s.stats.gone.Add(1)
		if rerr := s.cfg.Store.removeID(ctx, sub.ID); rerr != nil {
			s.cfg.Logger.Error("push: remove dead subscription", "err", rerr)
		}
	case errors.As(err, &re):
		if attempt >= s.cfg.MaxAttempts || ctx.Err() != nil {
			s.stats.gaveUp.Add(1)
			s.cfg.Logger.Warn("push: gave up after retries", "tenant", sub.TenantID, "attempts", attempt)
			return
		}
		delay := backoff(attempt, re.After)
		s.wg.Add(1)
		s.cfg.After(delay, func() {
			defer s.wg.Done()
			s.deliver(ctx, sub, body, attempt+1)
		})
	default:
		s.stats.refused.Add(1)
		s.cfg.Logger.Warn("push: delivery refused", "tenant", sub.TenantID)
	}
}

// backoff doubles from 5 seconds, honours Retry-After, and stops at 5 minutes.
func backoff(attempt int, retryAfter time.Duration) time.Duration {
	d := 5 * time.Second << (attempt - 1)
	if retryAfter > d {
		d = retryAfter
	}
	return min(d, 5*time.Minute)
}
