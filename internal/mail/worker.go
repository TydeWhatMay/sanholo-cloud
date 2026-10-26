package mail

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MaxAttempts is how many sends are tried before a message is marked failed.
const MaxAttempts = 8

// Worker delivers the outbox.
type Worker struct {
	Pool   *pgxpool.Pool
	Mailer Mailer
	Now    func() time.Time
	// Timeout bounds one send. Default 15 seconds.
	Timeout time.Duration
}

// Backoff is the wait after the nth failed attempt (1-based): 30 seconds,
// doubling, capped at one hour.
func Backoff(attempt int) time.Duration {
	d := 30 * time.Second
	for i := 1; i < attempt && d < time.Hour; i++ {
		d *= 2
	}
	return min(d, time.Hour)
}

// RunOnce delivers the messages that are due and returns how many it sent.
// Rows are locked with SKIP LOCKED, so several instances never double-send.
// A sent message's body is erased: it may carry a sign-in link.
func (w *Worker) RunOnce(ctx context.Context) (int, error) {
	now := w.now()
	tx, err := w.Pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("mail: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		SELECT id, to_email, subject, body, attempts, expires_at
		FROM mail_outbox
		WHERE sent_at IS NULL AND failed_at IS NULL AND next_attempt_at <= $1
		ORDER BY id LIMIT 20
		FOR UPDATE SKIP LOCKED`, now)
	if err != nil {
		return 0, fmt.Errorf("mail: select: %w", err)
	}
	type row struct {
		id       int64
		m        Message
		attempts int
		expires  *time.Time
	}
	var due []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.m.To, &r.m.Subject, &r.m.Body, &r.attempts, &r.expires); err != nil {
			rows.Close()
			return 0, fmt.Errorf("mail: scan: %w", err)
		}
		due = append(due, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("mail: rows: %w", err)
	}

	sent := 0
	for _, r := range due {
		if r.expires != nil && !now.Before(*r.expires) {
			err = w.finish(ctx, tx, `UPDATE mail_outbox SET failed_at = $2, body = '', last_error = 'expired' WHERE id = $1`, r.id, now)
		} else if serr := w.send(ctx, r.m); serr == nil {
			sent++
			err = w.finish(ctx, tx, `UPDATE mail_outbox SET sent_at = $2, body = '', attempts = attempts + 1 WHERE id = $1`, r.id, now)
		} else if r.attempts+1 >= MaxAttempts {
			err = w.finish(ctx, tx, `UPDATE mail_outbox SET failed_at = $2, body = '', attempts = attempts + 1, last_error = 'gave up' WHERE id = $1`, r.id, now)
			log.Printf("mail: message %d given up after %d attempts", r.id, r.attempts+1)
		} else {
			_, err = tx.Exec(ctx,
				`UPDATE mail_outbox SET attempts = attempts + 1, next_attempt_at = $2, last_error = 'send failed' WHERE id = $1`,
				r.id, now.Add(Backoff(r.attempts+1)))
		}
		if err != nil {
			return sent, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return sent, fmt.Errorf("mail: commit: %w", err)
	}
	return sent, nil
}

func (w *Worker) finish(ctx context.Context, tx pgx.Tx, q string, id int64, now time.Time) error {
	if _, err := tx.Exec(ctx, q, id, now); err != nil {
		return fmt.Errorf("mail: update: %w", err)
	}
	return nil
}

func (w *Worker) send(ctx context.Context, m Message) error {
	timeout := w.Timeout
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return w.Mailer.Send(ctx, m)
}

func (w *Worker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

// Run calls RunOnce every interval until ctx ends. Errors are logged without
// message content.
func (w *Worker) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if _, err := w.RunOnce(ctx); err != nil && ctx.Err() == nil {
			log.Printf("mail worker: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
