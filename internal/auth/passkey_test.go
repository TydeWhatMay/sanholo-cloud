package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lhaig/sanholo-cloud/internal/webauthntest"
)

const (
	testRPID     = "localhost"
	testRPOrigin = "http://localhost:8090"
)

func newPasskeySvc(t *testing.T) (*Service, *pgxpool.Pool) {
	t.Helper()
	wa, err := NewWebAuthn(testRPID, testRPOrigin, "SanHolo")
	if err != nil {
		t.Fatal(err)
	}
	return newSvc(t, Options{BaseURL: testRPOrigin, WebAuthn: wa})
}

func accountID(t *testing.T, pool *pgxpool.Pool, email string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`SELECT id::text FROM accounts WHERE email_hash = $1`, HashEmail(email)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// register runs a whole registration for the account and returns the
// authenticator and the stored credential id.
func register(t *testing.T, s *Service, acct string) *webauthntest.Authenticator {
	t.Helper()
	ctx := context.Background()
	opts, secret, err := s.BeginPasskeyRegistration(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	au := webauthntest.New(testRPID, testRPOrigin)
	body, err := au.Create(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishPasskeyRegistration(ctx, acct, secret, body); err != nil {
		t.Fatalf("finish registration: %v", err)
	}
	return au
}

func TestPasskeyRegisterAndList(t *testing.T) {
	s, pool := newPasskeySvc(t)
	newAccount(t, s, "a@example.org")
	acct := accountID(t, pool, "a@example.org")
	au := register(t, s, acct)
	list, err := s.ListPasskeys(context.Background(), acct)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v, %v", list, err)
	}
	if list[0].ID != base64.RawURLEncoding.EncodeToString(au.ID) {
		t.Errorf("listed id %q", list[0].ID)
	}
	if n := count(t, pool, `SELECT count(*) FROM webauthn_challenges`); n != 0 {
		t.Errorf("%d challenges left behind", n)
	}
}

func TestPasskeysDisabled(t *testing.T) {
	s, pool := newSvc(t, Options{})
	newAccount(t, s, "a@example.org")
	_, _, err := s.BeginPasskeyRegistration(context.Background(), accountID(t, pool, "a@example.org"))
	if !errors.Is(err, ErrPasskeysDisabled) {
		t.Fatalf("err = %v", err)
	}
	if _, _, err := s.BeginPasskeyLogin(context.Background()); !errors.Is(err, ErrPasskeysDisabled) {
		t.Fatalf("login err = %v", err)
	}
}

func TestPasskeyChallengeIsSingleUse(t *testing.T) {
	s, pool := newPasskeySvc(t)
	newAccount(t, s, "a@example.org")
	acct := accountID(t, pool, "a@example.org")
	ctx := context.Background()

	opts, secret, _ := s.BeginPasskeyRegistration(ctx, acct)
	body, _ := webauthntest.New(testRPID, testRPOrigin).Create(opts)
	if _, err := s.FinishPasskeyRegistration(ctx, acct, secret, body); err != nil {
		t.Fatal(err)
	}
	// The same challenge and the same valid response, replayed.
	if _, err := s.FinishPasskeyRegistration(ctx, acct, secret, body); !errors.Is(err, ErrPasskeyChallenge) {
		t.Fatalf("replay err = %v, want ErrPasskeyChallenge", err)
	}

	// A failed attempt spends the challenge too: garbage first, then the
	// valid answer to the same challenge is refused.
	opts, secret, _ = s.BeginPasskeyRegistration(ctx, acct)
	body, _ = webauthntest.New(testRPID, testRPOrigin).Create(opts)
	if _, err := s.FinishPasskeyRegistration(ctx, acct, secret, []byte(`{}`)); err == nil {
		t.Fatal("garbage accepted")
	}
	if _, err := s.FinishPasskeyRegistration(ctx, acct, secret, body); !errors.Is(err, ErrPasskeyChallenge) {
		t.Fatalf("after failed attempt err = %v, want ErrPasskeyChallenge", err)
	}

	// Sign-in challenges are single-use as well.
	au := register(t, s, acct)
	lopts, lsecret, _ := s.BeginPasskeyLogin(ctx)
	assertion, _ := au.Get(lopts)
	if _, _, err := s.FinishPasskeyLogin(ctx, lsecret, assertion, "ua"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.FinishPasskeyLogin(ctx, lsecret, assertion, "ua"); !errors.Is(err, ErrPasskeyChallenge) {
		t.Fatalf("login replay err = %v", err)
	}
}

func TestPasskeyChallengeExpires(t *testing.T) {
	s, pool := newPasskeySvc(t)
	newAccount(t, s, "a@example.org")
	acct := accountID(t, pool, "a@example.org")
	ctx := context.Background()
	now := time.Now()
	s.SetClock(func() time.Time { return now })

	opts, secret, _ := s.BeginPasskeyRegistration(ctx, acct)
	body, _ := webauthntest.New(testRPID, testRPOrigin).Create(opts)
	now = now.Add(ChallengeTTL + time.Second)
	if _, err := s.FinishPasskeyRegistration(ctx, acct, secret, body); !errors.Is(err, ErrPasskeyChallenge) {
		t.Fatalf("expired err = %v, want ErrPasskeyChallenge", err)
	}

	// One second inside the window still works.
	now = time.Now()
	opts, secret, _ = s.BeginPasskeyRegistration(ctx, acct)
	body, _ = webauthntest.New(testRPID, testRPOrigin).Create(opts)
	now = now.Add(ChallengeTTL - time.Second)
	if _, err := s.FinishPasskeyRegistration(ctx, acct, secret, body); err != nil {
		t.Fatalf("inside window: %v", err)
	}
}

func TestPasskeyChallengeBinding(t *testing.T) {
	s, pool := newPasskeySvc(t)
	newAccount(t, s, "a@example.org")
	newAccount(t, s, "b@example.org")
	a, b := accountID(t, pool, "a@example.org"), accountID(t, pool, "b@example.org")
	ctx := context.Background()

	opts, secret, _ := s.BeginPasskeyRegistration(ctx, a)
	body, _ := webauthntest.New(testRPID, testRPOrigin).Create(opts)

	cases := []struct {
		name    string
		account string
		secret  string
	}{
		{"another browser (no secret)", a, ""},
		{"another browser (wrong secret)", a, NewSecret()},
		{"another account", b, secret},
	}
	for _, tc := range cases {
		if _, err := s.FinishPasskeyRegistration(ctx, tc.account, tc.secret, body); !errors.Is(err, ErrPasskeyChallenge) {
			t.Errorf("%s: err = %v, want ErrPasskeyChallenge", tc.name, err)
		}
	}
	// None of the refusals spent the challenge: the owner can still finish.
	if _, err := s.FinishPasskeyRegistration(ctx, a, secret, body); err != nil {
		t.Fatalf("owner after refusals: %v", err)
	}

	// A sign-in challenge cannot complete a registration, nor the reverse.
	lopts, lsecret, _ := s.BeginPasskeyLogin(ctx)
	rbody, _ := webauthntest.New(testRPID, testRPOrigin).Create(lopts)
	if _, err := s.FinishPasskeyRegistration(ctx, a, lsecret, rbody); !errors.Is(err, ErrPasskeyChallenge) {
		t.Errorf("login challenge used for registration: %v", err)
	}
	opts, secret, _ = s.BeginPasskeyRegistration(ctx, a)
	au := webauthntest.New(testRPID, testRPOrigin)
	reg, _ := au.Create(opts)
	if _, err := s.FinishPasskeyRegistration(ctx, a, secret, reg); err != nil {
		t.Fatal(err)
	}
	opts, secret, _ = s.BeginPasskeyRegistration(ctx, a)
	assertion, _ := au.Get(opts)
	if _, _, err := s.FinishPasskeyLogin(ctx, secret, assertion, "ua"); !errors.Is(err, ErrPasskeyChallenge) {
		t.Errorf("registration challenge used for sign-in: %v", err)
	}
}

func TestPasskeyDeleteOnlyOwn(t *testing.T) {
	s, pool := newPasskeySvc(t)
	newAccount(t, s, "a@example.org")
	newAccount(t, s, "b@example.org")
	a, b := accountID(t, pool, "a@example.org"), accountID(t, pool, "b@example.org")
	ctx := context.Background()
	au := register(t, s, a)
	id := base64.RawURLEncoding.EncodeToString(au.ID)

	if ok, err := s.DeletePasskey(ctx, b, id); err != nil || ok {
		t.Fatalf("other account deleted it: %v, %v", ok, err)
	}
	if n := count(t, pool, `SELECT count(*) FROM passkeys`); n != 1 {
		t.Fatalf("%d rows after a foreign delete", n)
	}
	if ok, _ := s.DeletePasskey(ctx, a, "not base64 !!"); ok {
		t.Fatal("malformed id deleted something")
	}
	if ok, err := s.DeletePasskey(ctx, a, id); err != nil || !ok {
		t.Fatalf("owner delete = %v, %v", ok, err)
	}
	if n := count(t, pool, `SELECT count(*) FROM passkeys`); n != 0 {
		t.Fatalf("%d rows after delete", n)
	}
	// A deleted passkey no longer signs in.
	lopts, lsecret, _ := s.BeginPasskeyLogin(ctx)
	assertion, _ := au.Get(lopts)
	if _, _, err := s.FinishPasskeyLogin(ctx, lsecret, assertion, "ua"); err == nil {
		t.Fatal("deleted passkey signed in")
	}
}

func TestPasskeySignInCounterAndSession(t *testing.T) {
	s, pool := newPasskeySvc(t)
	newAccount(t, s, "a@example.org")
	acct := accountID(t, pool, "a@example.org")
	ctx := context.Background()
	au := register(t, s, acct)

	signIn := func() (string, error) {
		opts, secret, err := s.BeginPasskeyLogin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := au.Get(opts)
		sec, _, err := s.FinishPasskeyLogin(ctx, secret, body, "Test UA")
		return sec, err
	}
	sec, err := signIn()
	if err != nil {
		t.Fatalf("sign in: %v", err)
	}
	var sc int64
	var used *time.Time
	if err := pool.QueryRow(ctx, `SELECT sign_count, last_used_at FROM passkeys`).Scan(&sc, &used); err != nil {
		t.Fatal(err)
	}
	if sc != 1 || used == nil {
		t.Fatalf("sign_count = %d, last_used_at = %v", sc, used)
	}
	if _, err := signIn(); err != nil {
		t.Fatal(err)
	}
	_ = pool.QueryRow(ctx, `SELECT sign_count FROM passkeys`).Scan(&sc)
	if sc != 2 {
		t.Fatalf("sign_count = %d after second use, want 2", sc)
	}

	// The session is the same kind of row the magic link makes.
	ses, err := s.LookupSession(ctx, sec)
	if err != nil {
		t.Fatal(err)
	}
	if ses.AccountID != acct || ses.Email != "a@example.org" || ses.UserAgent != "Test UA" || ses.Method != MethodPasskey {
		t.Errorf("session = %+v", ses)
	}
	if d := ses.ExpiresAt.Sub(ses.CreatedAt); d < SessionTTL-time.Minute || d > SessionTTL+time.Minute {
		t.Errorf("lifetime %v, want %v", d, SessionTTL)
	}
	var hash string
	_ = pool.QueryRow(ctx, `SELECT secret_hash FROM account_sessions WHERE id = $1`, ses.ID).Scan(&hash)
	if hash != HashSecret(sec) || hash == sec {
		t.Error("session secret not stored as a hash")
	}

	// A magic-link session on the same account is marked as such.
	_, err = pool.Exec(ctx, `INSERT INTO magic_links (token_hash, account_id, expires_at) VALUES ($1, $2, now() + interval '1 minute')`, HashSecret("tok"), acct)
	if err != nil {
		t.Fatal(err)
	}
	msec, _, err := s.ConsumeMagicLink(ctx, "tok", "ua")
	if err != nil {
		t.Fatal(err)
	}
	mses, _ := s.LookupSession(ctx, msec)
	if mses.Method != MethodMagicLink {
		t.Errorf("magic-link session method = %q", mses.Method)
	}
}

func TestPasskeyRejectsReplayedCounter(t *testing.T) {
	s, pool := newPasskeySvc(t)
	newAccount(t, s, "a@example.org")
	acct := accountID(t, pool, "a@example.org")
	ctx := context.Background()
	au := register(t, s, acct)
	au.Counter = 10
	opts, secret, _ := s.BeginPasskeyLogin(ctx)
	body, _ := au.Get(opts) // counter 11
	if _, _, err := s.FinishPasskeyLogin(ctx, secret, body, "ua"); err != nil {
		t.Fatal(err)
	}
	// A clone that kept an older counter.
	au.Counter = 4
	opts, secret, _ = s.BeginPasskeyLogin(ctx)
	body, _ = au.Get(opts) // counter 5
	before := count(t, pool, `SELECT count(*) FROM account_sessions`)
	if _, _, err := s.FinishPasskeyLogin(ctx, secret, body, "ua"); err == nil {
		t.Fatal("a counter that went backwards signed in")
	}
	if n := count(t, pool, `SELECT count(*) FROM account_sessions`); n != before {
		t.Fatal("a session was created")
	}
	var sc int64
	_ = pool.QueryRow(ctx, `SELECT sign_count FROM passkeys`).Scan(&sc)
	if sc != 11 {
		t.Fatalf("sign_count = %d, want it unchanged at 11", sc)
	}
}

func TestPasskeyAccountDeleteCascades(t *testing.T) {
	s, pool := newPasskeySvc(t)
	newAccount(t, s, "a@example.org")
	acct := accountID(t, pool, "a@example.org")
	register(t, s, acct)
	if _, err := pool.Exec(context.Background(), `DELETE FROM tenants`); err != nil {
		t.Fatal(err)
	}
	if n := count(t, pool, `SELECT count(*) FROM passkeys`); n != 0 {
		t.Fatalf("%d passkeys outlived the account", n)
	}
}

// The kind column is a check of its own: a row that is bound to the right
// account but filed as a sign-in challenge cannot complete a registration.
func TestPasskeyChallengeKindIsChecked(t *testing.T) {
	s, pool := newPasskeySvc(t)
	newAccount(t, s, "a@example.org")
	acct := accountID(t, pool, "a@example.org")
	ctx := context.Background()
	opts, secret, _ := s.BeginPasskeyRegistration(ctx, acct)
	body, _ := webauthntest.New(testRPID, testRPOrigin).Create(opts)
	if _, err := pool.Exec(ctx, `UPDATE webauthn_challenges SET kind = 'login'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishPasskeyRegistration(ctx, acct, secret, body); !errors.Is(err, ErrPasskeyChallenge) {
		t.Fatalf("err = %v, want ErrPasskeyChallenge", err)
	}
}
