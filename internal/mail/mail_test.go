package mail

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lhaig/sanholo-cloud/internal/dbtest"
)

func enqueue(t *testing.T, pool *pgxpool.Pool, m Message, expires time.Time) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := Enqueue(ctx, tx, m, expires); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerSendsAndErasesBody(t *testing.T) {
	pool := dbtest.Migrated(t)
	f := &Fake{}
	w := &Worker{Pool: pool, Mailer: f}
	enqueue(t, pool, Message{To: "a@example.org", Subject: "s", Body: "secret link"}, time.Time{})
	n, err := w.RunOnce(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("RunOnce = %d, %v", n, err)
	}
	if got := f.Sent(); len(got) != 1 || got[0].Body != "secret link" || got[0].To != "a@example.org" {
		t.Fatalf("sent = %+v", got)
	}
	var body string
	var sentAt *time.Time
	if err := pool.QueryRow(context.Background(), `SELECT body, sent_at FROM mail_outbox`).Scan(&body, &sentAt); err != nil {
		t.Fatal(err)
	}
	if body != "" || sentAt == nil {
		t.Fatalf("body %q sent_at %v: body must be erased once sent", body, sentAt)
	}
	if n, _ := w.RunOnce(context.Background()); n != 0 || len(f.Sent()) != 1 {
		t.Fatal("sent twice")
	}
}

func TestWorkerRetriesWithBackoffThenGivesUp(t *testing.T) {
	pool := dbtest.Migrated(t)
	f := &Fake{}
	f.FailNext(100)
	now := time.Now().Add(time.Second) // after the row's DB-clock next_attempt_at
	w := &Worker{Pool: pool, Mailer: f, Now: func() time.Time { return now }}
	enqueue(t, pool, Message{To: "a@example.org", Subject: "s", Body: "b"}, time.Time{})
	ctx := context.Background()

	if n, err := w.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("first: %d %v", n, err)
	}
	var next time.Time
	var attempts int
	_ = pool.QueryRow(ctx, `SELECT attempts, next_attempt_at FROM mail_outbox`).Scan(&attempts, &next)
	if attempts != 1 || next.Sub(now) < 29*time.Second {
		t.Fatalf("attempts %d next in %v", attempts, next.Sub(now))
	}
	// Not due yet: no attempt.
	_, _ = w.RunOnce(ctx)
	_ = pool.QueryRow(ctx, `SELECT attempts FROM mail_outbox`).Scan(&attempts)
	if attempts != 1 {
		t.Fatalf("retried before backoff: attempts %d", attempts)
	}
	for i := 0; i < MaxAttempts; i++ {
		now = now.Add(2 * time.Hour)
		_, _ = w.RunOnce(ctx)
	}
	var failed *time.Time
	var body string
	_ = pool.QueryRow(ctx, `SELECT failed_at, body FROM mail_outbox`).Scan(&failed, &body)
	if failed == nil || body != "" {
		t.Fatalf("failed_at %v body %q", failed, body)
	}
	// A recovered mailer does not resurrect a failed row.
	f.FailNext(0)
	now = now.Add(2 * time.Hour)
	if n, _ := w.RunOnce(ctx); n != 0 {
		t.Fatal("sent a failed message")
	}
}

func TestWorkerRecoversAfterFailure(t *testing.T) {
	pool := dbtest.Migrated(t)
	f := &Fake{}
	f.FailNext(1)
	now := time.Now().Add(time.Second) // after the row's DB-clock next_attempt_at
	w := &Worker{Pool: pool, Mailer: f, Now: func() time.Time { return now }}
	enqueue(t, pool, Message{To: "a@example.org", Subject: "s", Body: "b"}, time.Time{})
	_, _ = w.RunOnce(context.Background())
	now = now.Add(time.Minute)
	if n, err := w.RunOnce(context.Background()); err != nil || n != 1 {
		t.Fatalf("retry: %d %v", n, err)
	}
}

func TestWorkerDropsExpiredMessage(t *testing.T) {
	pool := dbtest.Migrated(t)
	f := &Fake{}
	now := time.Now().Add(time.Second) // after the row's DB-clock next_attempt_at
	w := &Worker{Pool: pool, Mailer: f, Now: func() time.Time { return now }}
	enqueue(t, pool, Message{To: "a@example.org", Subject: "s", Body: "b"}, now.Add(time.Minute))
	now = now.Add(2 * time.Minute)
	if n, _ := w.RunOnce(context.Background()); n != 0 || len(f.Sent()) != 0 {
		t.Fatal("sent an expired message")
	}
}

func TestBackoff(t *testing.T) {
	cases := map[int]time.Duration{1: 30 * time.Second, 2: time.Minute, 3: 2 * time.Minute, 20: time.Hour}
	for n, want := range cases {
		if got := Backoff(n); got != want {
			t.Errorf("Backoff(%d) = %v, want %v", n, got, want)
		}
	}
}
