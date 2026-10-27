package auth

import (
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Options are the beta limits and addresses the service needs.
type Options struct {
	MaxTenants int
	// BaseURL is the public origin sign-in links are built from.
	BaseURL string
	// WebAuthn enables passkeys; nil turns them off.
	WebAuthn *webauthn.WebAuthn
}

// Service is the account lifecycle over one PostgreSQL pool. It is safe for
// concurrent use by several goroutines and several instances.
type Service struct {
	pool *pgxpool.Pool
	opt  Options
	now  func() time.Time
	// beforeCounterUpdate runs between a passkey assertion being verified
	// and its counter being written (tests simulate a racing login).
	beforeCounterUpdate func()
}

// New returns a Service.
func New(pool *pgxpool.Pool, opt Options) *Service {
	return &Service{pool: pool, opt: opt, now: time.Now}
}

// SetClock replaces the time source (tests).
func (s *Service) SetClock(now func() time.Time) { s.now = now }
