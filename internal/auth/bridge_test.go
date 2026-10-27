package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// session opens an account session for email and returns its row id and
// secret; tenant is the account's tenant.
func session(t *testing.T, s *Service, pool *pgxpool.Pool, email string) (id, secret, tenantID string) {
	t.Helper()
	ctx := context.Background()
	newAccount(t, s, email)
	var accountID string
	if err := pool.QueryRow(ctx, `SELECT id::text, tenant_id FROM accounts WHERE email = $1`, email).Scan(&accountID, &tenantID); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	secret, _, err = s.createSession(ctx, tx, accountID, "test", MethodMagicLink, s.now())
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT id::text FROM account_sessions WHERE secret_hash = $1`, HashSecret(secret)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id, secret, tenantID
}

func TestHandoffIsStoredHashedBoundAndSingleUse(t *testing.T) {
	s, pool := newSvc(t, Options{BaseURL: "http://127.0.0.1:8090"})
	ctx := context.Background()
	sid, _, tenantID := session(t, s, pool, "ann@example.org")

	code, gotTenant, err := s.CreateHandoff(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if gotTenant != tenantID {
		t.Fatalf("handoff tenant %q, want the account's %q", gotTenant, tenantID)
	}
	if n := count(t, pool, `SELECT count(*) FROM bridge_handoffs WHERE code_hash = $1`, HashSecret(code)); n != 1 {
		t.Errorf("hashed code rows = %d, want 1", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM bridge_handoffs WHERE code_hash = $1`, code); n != 0 {
		t.Error("the plain code is stored")
	}

	secret, expires, err := s.RedeemHandoff(ctx, code, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	if n := count(t, pool, `SELECT count(*) FROM bridge_sessions WHERE secret_hash = $1`, secret); n != 0 {
		t.Error("the plain Bridge secret is stored")
	}
	if want := s.now().Add(BridgeSessionMax); expires.After(want.Add(time.Second)) {
		t.Errorf("expiry %v beyond the %v maximum", expires, BridgeSessionMax)
	}
	b, err := s.LookupBridgeSession(ctx, secret)
	if err != nil || b.TenantID != tenantID || b.Unavailable {
		t.Fatalf("lookup = %+v, %v", b, err)
	}
	// Single use.
	if _, _, err := s.RedeemHandoff(ctx, code, tenantID); !errors.Is(err, ErrHandoffInvalid) {
		t.Errorf("second redeem = %v, want ErrHandoffInvalid", err)
	}
	if n := count(t, pool, `SELECT count(*) FROM bridge_sessions`); n != 1 {
		t.Errorf("bridge sessions = %d, want 1", n)
	}
}

func TestHandoffRefusals(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name  string
		setup func(t *testing.T, s *Service, pool *pgxpool.Pool, sid, tenantID string)
		// redeemAs is the tenant the code is presented to; "" means the account's own.
		redeemAs string
		code     string // "" means the real one
	}{
		{name: "another tenant", redeemAs: "someone-else"},
		{name: "empty code", code: "-"},
		{name: "unknown code", code: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
		{name: "expired", setup: func(t *testing.T, s *Service, _ *pgxpool.Pool, _, _ string) {
			base := s.now()
			s.SetClock(func() time.Time { return base.Add(HandoffTTL + time.Second) })
		}},
		{name: "account session revoked", setup: func(t *testing.T, s *Service, pool *pgxpool.Pool, sid, _ string) {
			if _, err := pool.Exec(ctx, `DELETE FROM account_sessions WHERE id = $1`, sid); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "account session expired", setup: func(t *testing.T, s *Service, pool *pgxpool.Pool, sid, _ string) {
			if _, err := pool.Exec(ctx, `UPDATE account_sessions SET expires_at = now() - interval '1 minute' WHERE id = $1`, sid); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "tenant suspended", setup: func(t *testing.T, s *Service, pool *pgxpool.Pool, _, tenantID string) {
			if _, err := pool.Exec(ctx, `UPDATE tenants SET status = 'suspended' WHERE id = $1`, tenantID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "tenant being deleted", setup: func(t *testing.T, s *Service, pool *pgxpool.Pool, _, tenantID string) {
			if _, err := pool.Exec(ctx, `UPDATE tenants SET status = 'deleting' WHERE id = $1`, tenantID); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, pool := newSvc(t, Options{BaseURL: "http://127.0.0.1:8090"})
			sid, _, tenantID := session(t, s, pool, "ann@example.org")
			code, _, err := s.CreateHandoff(ctx, sid)
			if err != nil {
				t.Fatal(err)
			}
			if tc.setup != nil {
				tc.setup(t, s, pool, sid, tenantID)
			}
			use := tc.code
			if use == "" {
				use = code
			} else if use == "-" {
				use = ""
			}
			as := tc.redeemAs
			if as == "" {
				as = tenantID
			}
			if _, _, err := s.RedeemHandoff(ctx, use, as); !errors.Is(err, ErrHandoffInvalid) {
				t.Fatalf("redeem = %v, want ErrHandoffInvalid", err)
			}
			if n := count(t, pool, `SELECT count(*) FROM bridge_sessions`); n != 0 {
				t.Errorf("a refused redeem left %d bridge sessions", n)
			}
		})
	}
}

// Presenting a code to the wrong tenant must not spend it: a stray request
// would otherwise burn the person's code.
func TestWrongTenantDoesNotSpendTheCode(t *testing.T) {
	s, pool := newSvc(t, Options{BaseURL: "http://127.0.0.1:8090"})
	ctx := context.Background()
	sid, _, tenantID := session(t, s, pool, "ann@example.org")
	code, _, _ := s.CreateHandoff(ctx, sid)
	if _, _, err := s.RedeemHandoff(ctx, code, "other"); !errors.Is(err, ErrHandoffInvalid) {
		t.Fatal("wrong tenant accepted")
	}
	if _, _, err := s.RedeemHandoff(ctx, code, tenantID); err != nil {
		t.Errorf("the code was spent by the wrong-tenant attempt: %v", err)
	}
}

func TestHandoffRedeemedOnceUnderConcurrency(t *testing.T) {
	s, pool := newSvc(t, Options{BaseURL: "http://127.0.0.1:8090"})
	ctx := context.Background()
	sid, _, tenantID := session(t, s, pool, "ann@example.org")
	code, _, _ := s.CreateHandoff(ctx, sid)
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := s.RedeemHandoff(ctx, code, tenantID); err == nil {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if won != 1 {
		t.Errorf("%d redeems succeeded, want exactly 1", won)
	}
}

func TestBridgeSessionLifetime(t *testing.T) {
	ctx := context.Background()
	s, pool := newSvc(t, Options{BaseURL: "http://127.0.0.1:8090"})
	sid, _, tenantID := session(t, s, pool, "ann@example.org")
	open := func() string {
		code, _, err := s.CreateHandoff(ctx, sid)
		if err != nil {
			t.Fatal(err)
		}
		secret, _, err := s.RedeemHandoff(ctx, code, tenantID)
		if err != nil {
			t.Fatal(err)
		}
		return secret
	}

	// Revoking the account session ends the Bridge session.
	secret := open()
	if _, err := s.LookupBridgeSession(ctx, secret); err != nil {
		t.Fatal(err)
	}
	var accountID string
	if err := pool.QueryRow(ctx, `SELECT account_id::text FROM account_sessions WHERE id = $1`, sid).Scan(&accountID); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.RevokeSession(ctx, accountID, sid); err != nil || !ok {
		t.Fatalf("revoke: %v %v", ok, err)
	}
	if _, err := s.LookupBridgeSession(ctx, secret); !errors.Is(err, ErrNoSession) {
		t.Errorf("after revoking the account session: %v, want ErrNoSession", err)
	}
	if n := count(t, pool, `SELECT count(*) FROM bridge_sessions`); n != 0 {
		t.Errorf("%d bridge sessions remain after the account session was revoked", n)
	}
}

func TestBridgeSessionRefusals(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name  string
		setup func(t *testing.T, s *Service, pool *pgxpool.Pool, sid, tenantID string)
		// suspended: the lookup succeeds but reports the tenant suspended.
		suspended bool
	}{
		{name: "account session expired", setup: func(t *testing.T, s *Service, pool *pgxpool.Pool, sid, _ string) {
			if _, err := pool.Exec(ctx, `UPDATE account_sessions SET expires_at = now() - interval '1 minute' WHERE id = $1`, sid); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "bridge session expired", setup: func(t *testing.T, s *Service, pool *pgxpool.Pool, _, _ string) {
			if _, err := pool.Exec(ctx, `UPDATE bridge_sessions SET expires_at = now() - interval '1 minute'`); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "tenant suspended", suspended: true, setup: func(t *testing.T, s *Service, pool *pgxpool.Pool, _, tenantID string) {
			if _, err := pool.Exec(ctx, `UPDATE tenants SET status = 'suspended' WHERE id = $1`, tenantID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "tenant being deleted", suspended: true, setup: func(t *testing.T, s *Service, pool *pgxpool.Pool, _, tenantID string) {
			if _, err := pool.Exec(ctx, `UPDATE tenants SET status = 'deleting' WHERE id = $1`, tenantID); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, pool := newSvc(t, Options{BaseURL: "http://127.0.0.1:8090"})
			sid, _, tenantID := session(t, s, pool, "ann@example.org")
			code, _, _ := s.CreateHandoff(ctx, sid)
			secret, _, err := s.RedeemHandoff(ctx, code, tenantID)
			if err != nil {
				t.Fatal(err)
			}
			tc.setup(t, s, pool, sid, tenantID)
			b, err := s.LookupBridgeSession(ctx, secret)
			if tc.suspended {
				if err != nil || !b.Unavailable {
					t.Fatalf("suspended tenant: %+v, %v", b, err)
				}
				return
			}
			if !errors.Is(err, ErrNoSession) {
				t.Fatalf("lookup = %+v, %v; want ErrNoSession", b, err)
			}
		})
	}
	s, _ := newSvc(t, Options{BaseURL: "http://127.0.0.1:8090"})
	if _, err := s.LookupBridgeSession(ctx, ""); !errors.Is(err, ErrNoSession) {
		t.Error("empty secret accepted")
	}
	if _, err := s.LookupBridgeSession(ctx, "nope"); !errors.Is(err, ErrNoSession) {
		t.Error("unknown secret accepted")
	}
}

// A Bridge session never outlives the account session it came from.
func TestBridgeSessionExpiryIsCappedByTheAccountSession(t *testing.T) {
	ctx := context.Background()
	s, pool := newSvc(t, Options{BaseURL: "http://127.0.0.1:8090"})
	sid, _, tenantID := session(t, s, pool, "ann@example.org")
	short := s.now().Add(2 * time.Hour)
	if _, err := pool.Exec(ctx, `UPDATE account_sessions SET expires_at = $2 WHERE id = $1`, sid, short); err != nil {
		t.Fatal(err)
	}
	code, _, _ := s.CreateHandoff(ctx, sid)
	_, expires, err := s.RedeemHandoff(ctx, code, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	if expires.After(short.Add(time.Second)) {
		t.Errorf("Bridge session expires %v, after the account session's %v", expires, short)
	}
}

func TestCreateHandoffNeedsALiveSessionAndTenant(t *testing.T) {
	ctx := context.Background()
	s, pool := newSvc(t, Options{BaseURL: "http://127.0.0.1:8090"})
	sid, _, tenantID := session(t, s, pool, "ann@example.org")
	if _, err := pool.Exec(ctx, `UPDATE tenants SET status = 'suspended' WHERE id = $1`, tenantID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateHandoff(ctx, sid); !errors.Is(err, ErrNoSession) {
		t.Errorf("suspended tenant: %v", err)
	}
	if _, _, err := s.CreateHandoff(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNoSession) {
		t.Errorf("unknown session: %v", err)
	}
}
