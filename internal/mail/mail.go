// Package mail sends the service's email. Callers never send inline: they
// Enqueue into the mail_outbox table, in the same transaction as the change
// that caused the mail, and a Worker delivers with retry and backoff. That
// keeps the HTTP path identical whether or not a mail will go out.
package mail

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Message is one plain-text email.
type Message struct {
	To      string
	Subject string
	Body    string
}

// Mailer delivers one message. An error means it may be retried.
type Mailer interface {
	Send(ctx context.Context, m Message) error
}

// Enqueue stores m for delivery. A message not sent by expires (zero for
// never) is dropped: a sign-in link that is already dead is not worth mailing.
func Enqueue(ctx context.Context, tx pgx.Tx, m Message, expires time.Time) error {
	_, err := EnqueueID(ctx, tx, m, expires)
	return err
}

// EnqueueID is Enqueue that returns the outbox row's id.
func EnqueueID(ctx context.Context, tx pgx.Tx, m Message, expires time.Time) (int64, error) {
	var exp *time.Time
	if !expires.IsZero() {
		exp = &expires
	}
	var id int64
	err := tx.QueryRow(ctx,
		`INSERT INTO mail_outbox (to_email, subject, body, expires_at) VALUES ($1, $2, $3, $4) RETURNING id`,
		m.To, m.Subject, m.Body, exp).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("mail: enqueue: %w", err)
	}
	return id, nil
}
