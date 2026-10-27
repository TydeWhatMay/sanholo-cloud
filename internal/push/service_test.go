package push_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lhaig/sanholo-cloud/internal/dbtest"
	"github.com/lhaig/sanholo-cloud/internal/push"
)

type seeded struct {
	tenant, account, session string
}

func seed(t *testing.T, pool *pgxpool.Pool, tenant string, operator bool) seeded {
	t.Helper()
	ctx := context.Background()
	s := seeded{tenant: tenant}
	if _, err := pool.Exec(ctx, `INSERT INTO tenants (id) VALUES ($1)`, tenant); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO accounts (tenant_id, email, email_hash, is_operator) VALUES ($1, $2, $2, $3) RETURNING id::text`,
		tenant, tenant+"@example.org", operator).Scan(&s.account); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO account_sessions (secret_hash, account_id, expires_at) VALUES ($1, $2, now() + interval '1 day') RETURNING id::text`,
		"h-"+tenant, s.account).Scan(&s.session); err != nil {
		t.Fatal(err)
	}
	return s
}

func (s seeded) add(t *testing.T, st *push.Store, sub push.Subscription) {
	t.Helper()
	if _, err := st.Add(context.Background(), s.tenant, s.account, s.session, sub); err != nil {
		t.Fatalf("add %s: %v", sub.Endpoint, err)
	}
}

func endpoint(n int) string { return fmt.Sprintf("https://fcm.googleapis.com/fcm/send/e%03d", n) }

func TestStoreAddCapAndOwnership(t *testing.T) {
	pool := dbtest.Migrated(t)
	st := push.NewStore(pool)
	ctx := context.Background()
	a := seed(t, pool, "acme", false)
	b := seed(t, pool, "bolt", false)
	k := newKeys(t)

	for i := range push.MaxPerTenant {
		created, err := st.Add(ctx, a.tenant, a.account, a.session, k.sub(endpoint(i)))
		if err != nil || !created {
			t.Fatalf("subscription %d: %v %v", i+1, created, err)
		}
	}
	// The 11th is refused; nothing was stored.
	if _, err := st.Add(ctx, a.tenant, a.account, a.session, k.sub(endpoint(99))); !errors.Is(err, push.ErrCapReached) {
		t.Fatalf("11th subscription: %v", err)
	}
	if n, _ := st.Count(ctx, a.tenant); n != push.MaxPerTenant {
		t.Fatalf("count = %d", n)
	}
	// At the cap, the same browser re-subscribing (new keys) is an update, not a new slot.
	k2 := newKeys(t)
	created, err := st.Add(ctx, a.tenant, a.account, a.session, k2.sub(endpoint(3)))
	if err != nil || created {
		t.Fatalf("re-subscribe at the cap: %v %v", created, err)
	}
	var p256 string
	_ = pool.QueryRow(ctx, `SELECT p256dh FROM push_subscriptions WHERE endpoint = $1`, endpoint(3)).Scan(&p256)
	if p256 != k2.sub("").Keys.P256dh {
		t.Fatal("re-subscribing did not replace the keys")
	}
	// The cap is per tenant.
	if _, err := st.Add(ctx, b.tenant, b.account, b.session, k.sub(endpoint(200))); err != nil {
		t.Fatalf("other tenant: %v", err)
	}
	// Another tenant cannot take over an endpoint.
	if _, err := st.Add(ctx, b.tenant, b.account, b.session, k.sub(endpoint(1))); !errors.Is(err, push.ErrEndpointTaken) {
		t.Fatalf("endpoint of another tenant: %v", err)
	}
	var owner string
	_ = pool.QueryRow(ctx, `SELECT tenant_id FROM push_subscriptions WHERE endpoint = $1`, endpoint(1)).Scan(&owner)
	if owner != "acme" {
		t.Fatalf("endpoint owner = %s", owner)
	}
	// Removing is scoped to the tenant.
	_ = st.Remove(ctx, b.tenant, endpoint(1))
	if n, _ := st.Count(ctx, a.tenant); n != push.MaxPerTenant {
		t.Fatal("another tenant removed a subscription")
	}
	_ = st.Remove(ctx, a.tenant, endpoint(1))
	if n, _ := st.Count(ctx, a.tenant); n != push.MaxPerTenant-1 {
		t.Fatal("own removal did not remove")
	}
	// The tenant column lets deletion find the rows, and removes them with the tenant.
	if _, err := pool.Exec(ctx, `DELETE FROM tenants WHERE id = 'acme'`); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.Count(ctx, a.tenant); n != 0 {
		t.Fatalf("%d subscriptions left after the tenant was deleted", n)
	}
}

func TestStoreConcurrentAddsRespectTheCap(t *testing.T) {
	pool := dbtest.Migrated(t)
	st := push.NewStore(pool)
	a := seed(t, pool, "acme", false)
	k := newKeys(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := range 25 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := st.Add(context.Background(), a.tenant, a.account, a.session, k.sub(endpoint(i))); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if n, _ := st.Count(context.Background(), a.tenant); n != push.MaxPerTenant || ok != push.MaxPerTenant {
		t.Fatalf("stored %d, succeeded %d, want %d", n, ok, push.MaxPerTenant)
	}
}

func TestSubscriptionSurvivesItsSession(t *testing.T) {
	pool := dbtest.Migrated(t)
	st := push.NewStore(pool)
	a := seed(t, pool, "acme", false)
	a.add(t, st, newKeys(t).sub(endpoint(1)))
	if _, err := pool.Exec(context.Background(), `DELETE FROM account_sessions WHERE id = $1`, a.session); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.Count(context.Background(), a.tenant); n != 1 {
		t.Fatalf("a signed-out session took the subscription with it: %d", n)
	}
}

// fakeSender records what it is given and answers per endpoint.
type fakeSender struct {
	mu     sync.Mutex
	sent   []sentMsg
	answer map[string][]error // endpoint -> errors for successive calls
	calls  map[string]int
}

type sentMsg struct {
	endpoint string
	p        push.Payload
	raw      []byte
}

func (f *fakeSender) Send(_ context.Context, sub push.Subscription, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	n := f.calls[sub.Endpoint]
	f.calls[sub.Endpoint]++
	var p push.Payload
	_ = json.Unmarshal(payload, &p)
	f.sent = append(f.sent, sentMsg{sub.Endpoint, p, append([]byte(nil), payload...)})
	if errs := f.answer[sub.Endpoint]; n < len(errs) {
		return errs[n]
	}
	return nil
}

func (f *fakeSender) to(endpoint string) []sentMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []sentMsg
	for _, m := range f.sent {
		if m.endpoint == endpoint {
			out = append(out, m)
		}
	}
	return out
}

type env struct {
	pool   *pgxpool.Pool
	st     *push.Store
	svc    *push.Service
	sender *fakeSender
	now    time.Time
	nowMu  sync.Mutex
	timers []func()
	delays []time.Duration
}

func (e *env) advance(d time.Duration) { e.nowMu.Lock(); e.now = e.now.Add(d); e.nowMu.Unlock() }

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{pool: dbtest.Migrated(t), sender: &fakeSender{}, now: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	e.st = push.NewStore(e.pool)
	e.svc = push.New(push.Config{
		Store: e.st, Sender: e.sender, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		TenantOrigin: func(id string) string { return "https://" + id + ".sanholo.test" },
		Now:          func() time.Time { e.nowMu.Lock(); defer e.nowMu.Unlock(); return e.now },
		After: func(d time.Duration, f func()) {
			e.nowMu.Lock()
			e.timers, e.delays = append(e.timers, f), append(e.delays, d)
			e.nowMu.Unlock()
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	e.svc.Start(ctx)
	t.Cleanup(func() { e.svc.Wait(); cancel() })
	return e
}

// fire runs the retry timers that are waiting.
func (e *env) fire() {
	e.nowMu.Lock()
	ts := e.timers
	e.timers = nil
	e.nowMu.Unlock()
	for _, f := range ts {
		f()
	}
}

func alert(kind, text string) push.Alert {
	return push.Alert{Kind: kind, Severity: "warning", Text: text, Key: "k1", At: "2026-10-27T12:00:00Z"}
}

func TestPayloadIsExactlyWhatTheWorkerExpects(t *testing.T) {
	e := newEnv(t)
	a := seed(t, e.pool, "acme", false)
	ep := endpoint(1)
	a.add(t, e.st, newKeys(t).sub(ep))

	in := alert("needs-help", "Line one\nline two\x07 with control")
	in.Link = "https://evil.example/phish?alert=decision%3Ad1&job=j9&sys=SOL&token=SECRET&screen=fleet#frag"
	if err := e.svc.Submit("acme", in); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	msgs := e.sender.to(ep)
	if len(msgs) != 1 {
		t.Fatalf("sent %d", len(msgs))
	}
	var generic map[string]any
	_ = json.Unmarshal(msgs[0].raw, &generic)
	if len(generic) != 5 {
		t.Fatalf("payload fields = %v, want exactly title, body, link, key, kind", generic)
	}
	p := msgs[0].p
	if p.Title != "SanHolo: needs help" || p.Kind != "needs-help" || p.Key != "k1" {
		t.Fatalf("payload = %+v", p)
	}
	if p.Body != "Line one line two with control" {
		t.Fatalf("body = %q", p.Body)
	}
	if want := "https://acme.sanholo.test/?alert=decision%3Ad1&job=j9&sys=SOL"; p.Link != want {
		t.Fatalf("link = %q, want %q (tenant origin, bridge parameters only)", p.Link, want)
	}
	for _, leak := range []string{"evil.example", "SECRET", "token", "severity", "2026-10"} {
		if strings.Contains(string(msgs[0].raw), leak) {
			t.Errorf("payload leaks %q: %s", leak, msgs[0].raw)
		}
	}
	// No link at all points at the Bridge.
	in2 := alert("test", "hello")
	_ = e.svc.Submit("acme", in2)
	e.svc.Wait()
	if got := e.sender.to(ep)[1].p.Link; got != "https://acme.sanholo.test/" {
		t.Fatalf("link without params = %q", got)
	}
	// The body is cut at what the worker shows.
	long := alert("test", strings.Repeat("é", push.MaxTextRunes))
	_ = e.svc.Submit("acme", long)
	e.svc.Wait()
	if n := len([]rune(e.sender.to(ep)[2].p.Body)); n != 240 {
		t.Fatalf("body runes = %d", n)
	}
}

func TestOnlyTheTenantsOwnSubscriptionsGetThePush(t *testing.T) {
	e := newEnv(t)
	a := seed(t, e.pool, "acme", false)
	b := seed(t, e.pool, "bolt", false)
	a.add(t, e.st, newKeys(t).sub(endpoint(1)))
	b.add(t, e.st, newKeys(t).sub(endpoint(2)))
	_ = e.svc.Submit("acme", alert("test", "hi"))
	e.svc.Wait()
	if len(e.sender.to(endpoint(1))) != 1 || len(e.sender.to(endpoint(2))) != 0 {
		t.Fatalf("sent: %+v", e.sender.sent)
	}
}

func TestHourlyCapBoundary(t *testing.T) {
	e := newEnv(t)
	a := seed(t, e.pool, "acme", false)
	ep := endpoint(1)
	a.add(t, e.st, newKeys(t).sub(ep))
	submit := func(n int) {
		for i := range n {
			if err := e.svc.Submit("acme", alert("test", fmt.Sprintf("m%d", i))); err != nil {
				t.Fatal(err)
			}
			e.svc.Wait()
		}
	}
	submit(push.HourlyCap)
	if got := len(e.sender.to(ep)); got != 30 {
		t.Fatalf("first window sent %d, want 30", got)
	}
	// The 31st: exactly one notice.
	submit(1)
	msgs := e.sender.to(ep)
	if len(msgs) != 31 || msgs[30].p.Body != "More alerts are waiting in your Bridge." || msgs[30].p.Kind != "alert-cap" {
		t.Fatalf("31st push = %+v (total %d)", msgs[len(msgs)-1].p, len(msgs))
	}
	// Then silence, including a critical one, until the window closes.
	submit(5)
	crit := alert("asteroid", "rock")
	crit.Severity = "critical"
	_ = e.svc.Submit("acme", crit)
	e.svc.Wait()
	if got := len(e.sender.to(ep)); got != 31 {
		t.Fatalf("pushes while capped = %d, want 31", got)
	}
	// One second before the hour: still silent.
	e.advance(time.Hour - time.Second)
	submit(1)
	if got := len(e.sender.to(ep)); got != 31 {
		t.Fatalf("pushes 1s before the window rolls = %d", got)
	}
	// At the hour the count starts again.
	e.advance(time.Second)
	submit(1)
	if got := len(e.sender.to(ep)); got != 32 {
		t.Fatalf("pushes at the window boundary = %d, want 32", got)
	}
	// And the notice is available again in the new window.
	submit(push.HourlyCap - 1)
	submit(1)
	if last := e.sender.to(ep); last[len(last)-1].p.Kind != "alert-cap" {
		t.Fatal("no notice in the second window")
	}
}

func TestCapIsPerTenantAndNotSpentWithoutSubscribers(t *testing.T) {
	e := newEnv(t)
	a := seed(t, e.pool, "acme", false)
	b := seed(t, e.pool, "bolt", false)
	// acme has nobody subscribed: 40 alerts spend nothing.
	for range 40 {
		_ = e.svc.Submit("acme", alert("test", "x"))
		e.svc.Wait()
	}
	a.add(t, e.st, newKeys(t).sub(endpoint(1)))
	b.add(t, e.st, newKeys(t).sub(endpoint(2)))
	_ = e.svc.Submit("acme", alert("test", "first"))
	e.svc.Wait()
	if len(e.sender.to(endpoint(1))) != 1 {
		t.Fatal("alerts before anyone subscribed used up the cap")
	}
	for range 35 {
		_ = e.svc.Submit("bolt", alert("test", "x"))
		e.svc.Wait()
	}
	_ = e.svc.Submit("acme", alert("test", "second"))
	e.svc.Wait()
	if len(e.sender.to(endpoint(1))) != 2 {
		t.Fatal("another tenant's flood capped this tenant")
	}
}

func TestGoneSubscriptionIsDeleted(t *testing.T) {
	e := newEnv(t)
	a := seed(t, e.pool, "acme", false)
	dead, live := endpoint(1), endpoint(2)
	a.add(t, e.st, newKeys(t).sub(dead))
	a.add(t, e.st, newKeys(t).sub(live))
	e.sender.answer = map[string][]error{dead: {push.ErrGone}}
	_ = e.svc.Submit("acme", alert("test", "x"))
	e.svc.Wait()
	if n, _ := e.st.Count(context.Background(), "acme"); n != 1 {
		t.Fatalf("count after 410 = %d, want 1", n)
	}
	var left string
	_ = e.pool.QueryRow(context.Background(), `SELECT endpoint FROM push_subscriptions`).Scan(&left)
	if left != live {
		t.Fatalf("remaining = %s", left)
	}
	// A refusal that is not gone/retryable keeps the row.
	e.sender.answer = map[string][]error{live: {fmt.Errorf("%w: HTTP 403", push.ErrPermanent)}}
	_ = e.svc.Submit("acme", alert("test", "y"))
	e.svc.Wait()
	if n, _ := e.st.Count(context.Background(), "acme"); n != 1 {
		t.Fatal("a 403 deleted the subscription")
	}
}

func TestRetriesBackOffAndStop(t *testing.T) {
	e := newEnv(t)
	a := seed(t, e.pool, "acme", false)
	ep := endpoint(1)
	a.add(t, e.st, newKeys(t).sub(ep))
	e.sender.answer = map[string][]error{ep: {&push.RetryError{After: 40 * time.Second}, &push.RetryError{}, &push.RetryError{}, &push.RetryError{}}}
	_ = e.svc.Submit("acme", alert("test", "x"))
	// The submit returned without waiting on the push service; the first
	// attempt happens, then a timer waits.
	deadline := time.Now().Add(5 * time.Second)
	for len(e.sender.to(ep)) < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	for {
		e.nowMu.Lock()
		n := len(e.timers)
		e.nowMu.Unlock()
		if n == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.fire() // attempt 2
	for {
		e.nowMu.Lock()
		n := len(e.timers)
		e.nowMu.Unlock()
		if n == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.fire() // attempt 3, the last
	e.svc.Wait()
	if got := len(e.sender.to(ep)); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
	// Retry-After 40s beats the 5s base; the second wait doubles to 10s.
	if len(e.delays) != 2 || e.delays[0] != 40*time.Second || e.delays[1] != 10*time.Second {
		t.Fatalf("delays = %v", e.delays)
	}
	if n, _ := e.st.Count(context.Background(), "acme"); n != 1 {
		t.Fatal("a 429/5xx deleted the subscription")
	}
}

func TestNotifyOperatorReachesOnlyOperatorTenants(t *testing.T) {
	e := newEnv(t)
	op := seed(t, e.pool, "ops", true)
	cust := seed(t, e.pool, "acme", false)
	op.add(t, e.st, newKeys(t).sub(endpoint(1)))
	cust.add(t, e.st, newKeys(t).sub(endpoint(2)))
	if err := e.svc.NotifyOperator("qm-sealed", "qm is sealed"); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	got := e.sender.to(endpoint(1))
	if len(got) != 1 || got[0].p.Body != "qm is sealed" || got[0].p.Link != "https://ops.sanholo.test/" || got[0].p.Title != "SanHolo operator: qm sealed" {
		t.Fatalf("operator push = %+v", got)
	}
	if len(e.sender.to(endpoint(2))) != 0 {
		t.Fatal("a customer received an operator alert")
	}
	for _, kind := range []string{"", "Bad Kind", "x/y", strings.Repeat("a", 41)} {
		if err := e.svc.NotifyOperator(kind, "t"); err == nil {
			t.Errorf("kind %q accepted", kind)
		}
	}
	if err := e.svc.NotifyOperator("ok", "  "); err == nil {
		t.Error("blank text accepted")
	}
}

func TestQueueIsBoundedAndSubmitNeverBlocks(t *testing.T) {
	pool := dbtest.Migrated(t)
	svc := push.New(push.Config{Store: push.NewStore(pool), Sender: &fakeSender{}, QueueSize: 2,
		TenantOrigin: func(string) string { return "https://x" }})
	// Not started: nothing drains the queue.
	if err := svc.Submit("a", alert("test", "1")); err != nil {
		t.Fatal(err)
	}
	if err := svc.Submit("a", alert("test", "2")); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- svc.Submit("a", alert("test", "3")) }()
	select {
	case err := <-done:
		if !errors.Is(err, push.ErrQueueFull) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Submit blocked on a full queue")
	}
}

func TestParseAlert(t *testing.T) {
	good := `{"kind":"asteroid","severity":"critical","text":"Rock","key":"k","at":"2026-10-27T12:00:00Z","link":"https://a.test/?alert=x"}`
	if _, err := push.ParseAlert([]byte(good)); err != nil {
		t.Fatal(err)
	}
	edge := fmt.Sprintf(`{"kind":"test","severity":"info","text":%q}`, strings.Repeat("é", push.MaxTextRunes))
	if _, err := push.ParseAlert([]byte(edge)); err != nil {
		t.Fatalf("280 runes refused: %v", err)
	}
	bad := map[string]string{
		"unknown kind":  `{"kind":"drop-table","severity":"info","text":"x"}`,
		"empty kind":    `{"severity":"info","text":"x"}`,
		"bad severity":  `{"kind":"test","severity":"urgent","text":"x"}`,
		"no text":       `{"kind":"test","severity":"info"}`,
		"281 runes":     fmt.Sprintf(`{"kind":"test","severity":"info","text":%q}`, strings.Repeat("é", push.MaxTextRunes+1)),
		"long key":      fmt.Sprintf(`{"kind":"test","severity":"info","text":"x","key":%q}`, strings.Repeat("k", 121)),
		"bad time":      `{"kind":"test","severity":"info","text":"x","at":"yesterday"}`,
		"long link":     fmt.Sprintf(`{"kind":"test","severity":"info","text":"x","link":%q}`, "https://a.test/"+strings.Repeat("l", 2048)),
		"not json":      `kind=test`,
		"empty":         ``,
		"oversize body": `{"kind":"test","severity":"info","text":"x","pad":"` + strings.Repeat("p", push.MaxBody) + `"}`,
	}
	for name, body := range bad {
		if _, err := push.ParseAlert([]byte(body)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
