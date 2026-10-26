package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lhaig/sanholo-cloud/internal/dbtest"
)

func newSvc(t *testing.T, opt Options) (*Service, *pgxpool.Pool) {
	t.Helper()
	pool := dbtest.Migrated(t)
	if opt.MaxTenants == 0 {
		opt.MaxTenants = 20
	}
	return New(pool, opt), pool
}

func count(t *testing.T, pool *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Every account is created through an invite; the address is the invite's.
func TestAccountStoresEmailHash(t *testing.T) {
	s, pool := newSvc(t, Options{})
	newAccount(t, s, "Mixed@Example.org")
	var h string
	if err := pool.QueryRow(context.Background(), `SELECT email_hash FROM accounts`).Scan(&h); err != nil {
		t.Fatal(err)
	}
	if h != HashEmail("mixed@example.org") {
		t.Fatalf("email_hash = %q", h)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE accounts SET email_hash = ''`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO accounts (tenant_id, email, email_hash) SELECT tenant_id, 'x@example.org', '' FROM accounts`); err == nil {
		t.Fatal("duplicate email_hash accepted")
	}
}

func TestWaitlistStoresOnce(t *testing.T) {
	s, pool := newSvc(t, Options{})
	ctx := context.Background()
	for _, e := range []string{"a@example.org", "a@example.org", "A@Example.ORG"} {
		if err := s.JoinWaitlist(ctx, e); err != nil {
			t.Fatalf("JoinWaitlist(%q): %v", e, err)
		}
	}
	if n := count(t, pool, `SELECT count(*) FROM waitlist`); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM mail_outbox`); n != 0 {
		t.Fatalf("waitlist queued %d mails", n)
	}
	if err := s.JoinWaitlist(ctx, "bad\r\n@example.org"); err == nil {
		t.Fatal("control characters accepted")
	}
}

func TestInviteStoredHashedAndSingleUse(t *testing.T) {
	s, pool := newSvc(t, Options{})
	ctx := context.Background()
	code, expires, err := s.IssueInvite(ctx, "a@example.org")
	if err != nil {
		t.Fatal(err)
	}
	if len(code) < 43 {
		t.Fatalf("code too short: %d", len(code))
	}
	if d := expires.Sub(s.now()); d < 13*24*time.Hour || d > 14*24*time.Hour+time.Minute {
		t.Fatalf("expiry in %v, want 14 days", d)
	}
	if n := count(t, pool, `SELECT count(*) FROM invites WHERE code_hash = $1`, code); n != 0 {
		t.Fatal("plaintext code stored")
	}
	if n := count(t, pool, `SELECT count(*) FROM invites WHERE code_hash = $1`, HashSecret(code)); n != 1 {
		t.Fatal("hash not stored")
	}
	tid, err := s.Redeem(ctx, code)
	if err != nil || tid == "" {
		t.Fatalf("first redeem: %q %v", tid, err)
	}
	var email string
	if err := pool.QueryRow(ctx, `SELECT email FROM accounts WHERE tenant_id = $1`, tid).Scan(&email); err != nil || email != "a@example.org" {
		t.Fatalf("account email = %q, %v (must be the invite's)", email, err)
	}
	if _, err := s.Redeem(ctx, code); !errors.Is(err, ErrInviteUsed) {
		t.Fatalf("second redeem: %v, want ErrInviteUsed", err)
	}
	if n := count(t, pool, `SELECT count(*) FROM tenants`); n != 1 {
		t.Fatalf("tenants = %d", n)
	}
}

func TestRedeemRefusals(t *testing.T) {
	ctx := context.Background()
	t.Run("no code while invite only", func(t *testing.T) {
		s, pool := newSvc(t, Options{})
		if _, err := s.Redeem(ctx, ""); !errors.Is(err, ErrInviteRequired) {
			t.Fatalf("err = %v", err)
		}
		if count(t, pool, `SELECT count(*) FROM tenants`) != 0 {
			t.Fatal("tenant created")
		}
	})
	t.Run("unknown code", func(t *testing.T) {
		s, pool := newSvc(t, Options{})
		if _, err := s.Redeem(ctx, NewSecret()); !errors.Is(err, ErrInviteInvalid) {
			t.Fatalf("err = %v", err)
		}
		if count(t, pool, `SELECT count(*) FROM tenants`) != 0 {
			t.Fatal("tenant created")
		}
	})
	t.Run("expired", func(t *testing.T) {
		s, pool := newSvc(t, Options{})
		code, _, _ := s.IssueInvite(ctx, "a@example.org")
		s.SetClock(func() time.Time { return time.Now().Add(InviteTTL + time.Second) })
		if _, err := s.Redeem(ctx, code); !errors.Is(err, ErrInviteExpired) {
			t.Fatalf("err = %v", err)
		}
		if count(t, pool, `SELECT count(*) FROM tenants`) != 0 {
			t.Fatal("tenant created")
		}
	})
	t.Run("existing account leaves the invite unused", func(t *testing.T) {
		s, pool := newSvc(t, Options{})
		newAccount(t, s, "a@example.org")
		code, _, _ := s.IssueInvite(ctx, "a@example.org")
		if _, err := s.Redeem(ctx, code); !errors.Is(err, ErrAccountExists) {
			t.Fatalf("err = %v", err)
		}
		if count(t, pool, `SELECT count(*) FROM invites WHERE used_at IS NOT NULL`) != 1 {
			t.Fatal("the second invite was burned (only the first may be used)")
		}
	})
}

func TestBetaCapCreatesNothing(t *testing.T) {
	s, pool := newSvc(t, Options{MaxTenants: 2})
	ctx := context.Background()
	var codes []string
	for _, e := range []string{"a@example.org", "b@example.org", "c@example.org"} {
		c, _, _ := s.IssueInvite(ctx, e)
		codes = append(codes, c)
	}
	for i := 0; i < 2; i++ {
		if _, err := s.Redeem(ctx, codes[i]); err != nil {
			t.Fatalf("redeem %d: %v", i, err)
		}
	}
	if _, err := s.Redeem(ctx, codes[2]); !errors.Is(err, ErrBetaFull) {
		t.Fatalf("third: %v, want ErrBetaFull", err)
	}
	if n := count(t, pool, `SELECT count(*) FROM tenants`); n != 2 {
		t.Fatalf("tenants = %d", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM accounts`); n != 2 {
		t.Fatalf("accounts = %d", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM invites WHERE used_at IS NOT NULL`); n != 2 {
		t.Fatalf("used invites = %d, the refused invite must stay unused", n)
	}
	// A deleting tenant frees a place.
	if _, err := pool.Exec(ctx, `UPDATE tenants SET status = 'deleting' WHERE id = (SELECT id FROM tenants LIMIT 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Redeem(ctx, codes[2]); err != nil {
		t.Fatalf("after freeing a place: %v", err)
	}
}

// Concurrent redemptions at the boundary: exactly MaxTenants succeed.
func TestBetaCapHoldsUnderConcurrency(t *testing.T) {
	const cap, tries = 5, 16
	s, pool := newSvc(t, Options{MaxTenants: cap})
	ctx := context.Background()
	codes := make([]string, tries)
	for i := range codes {
		codes[i], _, _ = s.IssueInvite(ctx, "u"+string(rune('a'+i))+"@example.org")
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make([]error, tries)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, results[i] = s.Redeem(ctx, codes[i])
		}()
	}
	close(start)
	wg.Wait()
	ok, full := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrBetaFull):
			full++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != cap || full != tries-cap {
		t.Fatalf("ok=%d full=%d, want %d and %d", ok, full, cap, tries-cap)
	}
	if n := count(t, pool, `SELECT count(*) FROM tenants`); n != cap {
		t.Fatalf("tenants = %d, want %d", n, cap)
	}
}

// Two redemptions of one invite: one wins.
func TestInviteRedeemedOnceUnderConcurrency(t *testing.T) {
	s, pool := newSvc(t, Options{})
	ctx := context.Background()
	code, _, _ := s.IssueInvite(ctx, "a@example.org")
	var wg sync.WaitGroup
	errs := make([]error, 6)
	for i := range errs {
		wg.Add(1)
		go func() { defer wg.Done(); _, errs[i] = s.Redeem(ctx, code) }()
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		if err == nil {
			won++
		}
	}
	if won != 1 || count(t, pool, `SELECT count(*) FROM tenants`) != 1 {
		t.Fatalf("winners = %d", won)
	}
}
