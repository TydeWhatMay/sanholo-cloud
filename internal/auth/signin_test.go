package auth

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lhaig/sanholo-cloud/internal/mail"
)

func newAccount(t *testing.T, s *Service, email string) {
	t.Helper()
	ctx := context.Background()
	code, _, err := s.IssueInvite(ctx, email)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Redeem(ctx, code); err != nil {
		t.Fatal(err)
	}
}

func outbox(t *testing.T, pool *pgxpool.Pool) []mail.Message {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT to_email, subject, body FROM mail_outbox ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []mail.Message
	for rows.Next() {
		var m mail.Message
		if err := rows.Scan(&m.To, &m.Subject, &m.Body); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func tokenFrom(t *testing.T, m mail.Message) string {
	t.Helper()
	_, rest, ok := strings.Cut(m.Body, "http://127.0.0.1:8090/signin/verify?token=")
	if !ok {
		t.Fatalf("no link in %q", m.Body)
	}
	tok, _, _ := strings.Cut(rest, "\n")
	return tok
}

// The request path does identical work for a known and an unknown address:
// one counted request, one signin_requests row, no mail.
func TestSignInRequestPathIsExistenceIndependent(t *testing.T) {
	s, pool := newSvc(t, Options{BaseURL: "http://127.0.0.1:8090"})
	ctx := context.Background()
	newAccount(t, s, "known@example.org")

	snapshot := func() [3]int {
		return [3]int{
			count(t, pool, `SELECT count(*) FROM signin_requests`),
			count(t, pool, `SELECT coalesce(sum(count),0) FROM rate_counters`),
			count(t, pool, `SELECT count(*) FROM mail_outbox`) + count(t, pool, `SELECT count(*) FROM magic_links`),
		}
	}
	before := snapshot()
	if err := s.RequestSignIn(ctx, "known@example.org", "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	known := snapshot()
	if err := s.RequestSignIn(ctx, "unknown@example.org", "192.0.2.2"); err != nil {
		t.Fatal(err)
	}
	both := snapshot()
	for i := range before {
		dKnown, dUnknown := known[i]-before[i], both[i]-known[i]
		if dKnown != dUnknown {
			t.Fatalf("effect %d differs: known +%d, unknown +%d", i, dKnown, dUnknown)
		}
	}
	if both[2] != 0 {
		t.Fatal("request path queued mail or created a token")
	}
	// The unknown address is stored only as a hash.
	if n := count(t, pool, `SELECT count(*) FROM signin_requests WHERE email_hash LIKE '%@%'`); n != 0 {
		t.Fatal("plaintext address stored")
	}
}

func TestSignInLinkLifecycle(t *testing.T) {
	s, pool := newSvc(t, Options{BaseURL: "http://127.0.0.1:8090"})
	ctx := context.Background()
	newAccount(t, s, "known@example.org")
	_ = s.RequestSignIn(ctx, "KNOWN@example.org", "192.0.2.1") // case-insensitive
	_ = s.RequestSignIn(ctx, "unknown@example.org", "192.0.2.2")
	made, err := s.ProcessSignInRequests(ctx)
	if err != nil || made != 1 {
		t.Fatalf("made %d, %v", made, err)
	}
	msgs := outbox(t, pool)
	if len(msgs) != 1 || msgs[0].To != "known@example.org" {
		t.Fatalf("outbox = %+v", msgs)
	}
	tok := tokenFrom(t, msgs[0])
	if len(tok) < 43 {
		t.Fatalf("token too short: %d", len(tok))
	}
	if n := count(t, pool, `SELECT count(*) FROM magic_links WHERE token_hash = $1`, tok); n != 0 {
		t.Fatal("plaintext token stored")
	}
	if n := count(t, pool, `SELECT count(*) FROM magic_links WHERE token_hash = $1`, HashSecret(tok)); n != 1 {
		t.Fatal("token hash not stored")
	}
	secret, exp, err := s.ConsumeMagicLink(ctx, tok, "UA/1.0")
	if err != nil || secret == "" {
		t.Fatalf("consume: %v", err)
	}
	if d := exp.Sub(s.now()); d < 29*24*time.Hour {
		t.Fatalf("session expiry in %v", d)
	}
	if n := count(t, pool, `SELECT count(*) FROM account_sessions WHERE secret_hash = $1`, secret); n != 0 {
		t.Fatal("plaintext session id stored")
	}
	if _, _, err := s.ConsumeMagicLink(ctx, tok, ""); !errors.Is(err, ErrLinkInvalid) {
		t.Fatalf("replay: %v, want ErrLinkInvalid", err)
	}
	if n := count(t, pool, `SELECT count(*) FROM account_sessions`); n != 1 {
		t.Fatalf("sessions = %d after replay", n)
	}
	ses, err := s.LookupSession(ctx, secret)
	if err != nil || ses.Email != "known@example.org" {
		t.Fatalf("lookup: %+v %v", ses, err)
	}
}

func TestMagicLinkExpiresAfter15Minutes(t *testing.T) {
	s, pool := newSvc(t, Options{BaseURL: "http://127.0.0.1:8090"})
	ctx := context.Background()
	newAccount(t, s, "a@example.org")
	_ = s.RequestSignIn(ctx, "a@example.org", "192.0.2.1")
	_, _ = s.ProcessSignInRequests(ctx)
	tok := tokenFrom(t, outbox(t, pool)[0])
	s.SetClock(func() time.Time { return time.Now().Add(LinkTTL + time.Second) })
	if _, _, err := s.ConsumeMagicLink(ctx, tok, ""); !errors.Is(err, ErrLinkInvalid) {
		t.Fatalf("expired link: %v", err)
	}
	if n := count(t, pool, `SELECT count(*) FROM account_sessions`); n != 0 {
		t.Fatal("session created from an expired link")
	}
	if _, _, err := s.ConsumeMagicLink(ctx, "garbage", ""); !errors.Is(err, ErrLinkInvalid) {
		t.Fatalf("garbage: %v", err)
	}
}

func TestStaleSignInRequestIsDropped(t *testing.T) {
	s, pool := newSvc(t, Options{BaseURL: "http://127.0.0.1:8090"})
	ctx := context.Background()
	newAccount(t, s, "a@example.org")
	_ = s.RequestSignIn(ctx, "a@example.org", "192.0.2.1")
	s.SetClock(func() time.Time { return time.Now().Add(LinkTTL + time.Minute) })
	if made, _ := s.ProcessSignInRequests(ctx); made != 0 || len(outbox(t, pool)) != 0 {
		t.Fatal("stale request produced a link")
	}
	if made, _ := s.ProcessSignInRequests(ctx); made != 0 {
		t.Fatal("processed twice")
	}
}

func TestSignInRateLimits(t *testing.T) {
	ctx := context.Background()
	t.Run("per email, any case", func(t *testing.T) {
		s, _ := newSvc(t, Options{})
		for i := 0; i < SigninLimit; i++ {
			if err := s.RequestSignIn(ctx, "a@example.org", "192.0.2."+string(rune('1'+i))); err != nil {
				t.Fatalf("request %d: %v", i, err)
			}
		}
		if err := s.RequestSignIn(ctx, "A@Example.org", "192.0.2.99"); !errors.Is(err, ErrRateLimited) {
			t.Fatalf("6th: %v, want ErrRateLimited", err)
		}
		if err := s.RequestSignIn(ctx, "other@example.org", "192.0.2.98"); err != nil {
			t.Fatalf("other email: %v", err)
		}
	})
	t.Run("per ip", func(t *testing.T) {
		s, _ := newSvc(t, Options{})
		for i := 0; i < SigninLimit; i++ {
			if err := s.RequestSignIn(ctx, "u"+string(rune('a'+i))+"@example.org", "203.0.113.5"); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.RequestSignIn(ctx, "new@example.org", "203.0.113.5"); !errors.Is(err, ErrRateLimited) {
			t.Fatalf("6th: %v", err)
		}
		if err := s.RequestSignIn(ctx, "new@example.org", "203.0.113.6"); err != nil {
			t.Fatalf("other ip: %v", err)
		}
	})
	t.Run("window rolls over", func(t *testing.T) {
		s, _ := newSvc(t, Options{})
		base := time.Date(2026, 1, 1, 10, 30, 0, 0, time.UTC)
		s.SetClock(func() time.Time { return base })
		for i := 0; i < SigninLimit+1; i++ {
			_ = s.RequestSignIn(ctx, "a@example.org", "192.0.2.1")
		}
		s.SetClock(func() time.Time { return base.Add(time.Hour) })
		if err := s.RequestSignIn(ctx, "a@example.org", "192.0.2.1"); err != nil {
			t.Fatalf("next hour: %v", err)
		}
	})
	t.Run("invalid email is not counted as a request row", func(t *testing.T) {
		s, pool := newSvc(t, Options{})
		if err := s.RequestSignIn(ctx, "a@example.org\r\nBcc: x@y.z", "192.0.2.1"); err == nil {
			t.Fatal("accepted")
		}
		if count(t, pool, `SELECT count(*) FROM signin_requests`) != 0 {
			t.Fatal("row created")
		}
	})
}

func TestClientIP(t *testing.T) {
	edge := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	cases := []struct {
		name, peer, xff, want string
	}{
		{"untrusted peer ignores header", "198.51.100.7", "1.2.3.4", "198.51.100.7"},
		{"untrusted peer ignores chain", "198.51.100.7", "1.2.3.4, 10.0.0.1", "198.51.100.7"},
		{"trusted peer, one entry", "10.0.0.1", "203.0.113.9", "203.0.113.9"},
		{"trusted peer, spoof prefix ignored", "10.0.0.1", "6.6.6.6, 203.0.113.9", "203.0.113.9"},
		{"trusted peer, inner proxy skipped", "10.0.0.1", "203.0.113.9, 10.0.0.2", "203.0.113.9"},
		{"trusted peer, no header", "10.0.0.1", "", "10.0.0.1"},
		{"trusted peer, garbage header", "10.0.0.1", "not-an-ip", "10.0.0.1"},
		{"trusted peer, all trusted", "10.0.0.1", "10.0.0.2", "10.0.0.1"},
		{"v4-mapped peer", "::ffff:10.0.0.1", "203.0.113.9", "203.0.113.9"},
		{"no trusted list", "198.51.100.7", "1.2.3.4", "198.51.100.7"},
		{"v6 peer keyed on its /64", "2001:db8:1:2:aaaa:bbbb:cccc:dddd", "", "2001:db8:1:2::/64"},
		{"v6 same /64 same key", "2001:db8:1:2:1::9", "", "2001:db8:1:2::/64"},
		{"v6 next /64 differs", "2001:db8:1:3::1", "", "2001:db8:1:3::/64"},
		{"v6 client behind the edge", "10.0.0.1", "2001:db8:1:2:aaaa:bbbb:cccc:dddd", "2001:db8:1:2::/64"},
		{"v4-mapped client behind the edge", "10.0.0.1", "::ffff:203.0.113.9", "203.0.113.9"},
		{"v4-mapped peer unchanged", "::ffff:198.51.100.7", "", "198.51.100.7"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			trusted := edge
			if tc.name == "no trusted list" {
				trusted = nil
			}
			if got := ClientIP(netip.MustParseAddr(tc.peer), tc.xff, trusted); got != tc.want {
				t.Fatalf("ClientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSessionsSlideListAndRevoke(t *testing.T) {
	s, pool := newSvc(t, Options{BaseURL: "http://127.0.0.1:8090"})
	ctx := context.Background()
	newAccount(t, s, "a@example.org")
	newAccount(t, s, "b@example.org")

	login := func(email, ua string) string {
		_ = s.RequestSignIn(ctx, email, "192.0.2."+ua)
		_, _ = s.ProcessSignInRequests(ctx)
		msgs := outbox(t, pool)
		sec, _, err := s.ConsumeMagicLink(ctx, tokenFrom(t, msgs[len(msgs)-1]), "UA "+ua+"\r\n")
		if err != nil {
			t.Fatal(err)
		}
		return sec
	}
	s1, s2, sb := login("a@example.org", "1"), login("a@example.org", "2"), login("b@example.org", "3")

	ses, err := s.LookupSession(ctx, s1)
	if err != nil {
		t.Fatal(err)
	}
	list, _ := s.ListSessions(ctx, ses.AccountID)
	if len(list) != 2 {
		t.Fatalf("sessions listed = %d", len(list))
	}
	for _, l := range list {
		if strings.ContainsAny(l.UserAgent, "\r\n") {
			t.Fatalf("control characters kept in %q", l.UserAgent)
		}
	}

	// Sliding: a lookup after refreshAfter extends the expiry.
	later := time.Now().Add(10 * 24 * time.Hour)
	s.SetClock(func() time.Time { return later })
	ses2, err := s.LookupSession(ctx, s1)
	if err != nil || !ses2.Refreshed || ses2.ExpiresAt.Before(later.Add(29*24*time.Hour)) {
		t.Fatalf("slide: %+v %v", ses2, err)
	}
	// An immediate second lookup does not write again.
	if ses3, _ := s.LookupSession(ctx, s1); ses3.Refreshed {
		t.Fatal("refreshed twice in a row")
	}
	// Past the 30 days without use the session is gone.
	s.SetClock(func() time.Time { return later.Add(31 * 24 * time.Hour) })
	if _, err := s.LookupSession(ctx, s1); !errors.Is(err, ErrNoSession) {
		t.Fatalf("expired session: %v", err)
	}
	s.SetClock(time.Now)

	// Revoking: not another account's, not a bad id; own works.
	bses, _ := s.LookupSession(ctx, sb)
	if ok, _ := s.RevokeSession(ctx, ses.AccountID, bses.ID); ok {
		t.Fatal("revoked another account's session")
	}
	if ok, _ := s.RevokeSession(ctx, ses.AccountID, "not-a-uuid"); ok {
		t.Fatal("revoked a bad id")
	}
	ses1, _ := s.LookupSession(ctx, s2)
	if ok, err := s.RevokeSession(ctx, ses.AccountID, ses1.ID); !ok || err != nil {
		t.Fatalf("own revoke: %v %v", ok, err)
	}
	if _, err := s.LookupSession(ctx, s2); !errors.Is(err, ErrNoSession) {
		t.Fatalf("revoked session still works: %v", err)
	}
	if _, err := s.LookupSession(ctx, sb); err != nil {
		t.Fatalf("other session affected: %v", err)
	}
	if err := s.RevokeSecret(ctx, sb); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LookupSession(ctx, sb); !errors.Is(err, ErrNoSession) {
		t.Fatal("sign out left the session")
	}
	if _, err := s.LookupSession(ctx, ""); !errors.Is(err, ErrNoSession) {
		t.Fatal("empty secret")
	}
}

func TestAllowStopsCountingOverTheLimit(t *testing.T) {
	s, pool := newSvc(t, Options{})
	ctx := context.Background()
	for i := range 10 {
		ok, err := s.Allow(ctx, "k", 3, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if want := i < 3; ok != want {
			t.Fatalf("hit %d: allowed = %v, want %v", i+1, ok, want)
		}
	}
	if n := count(t, pool, `SELECT count FROM rate_counters WHERE key = 'k'`); n != 4 {
		t.Fatalf("counter = %d, want 4 (limit+1)", n)
	}
}

func TestSigninIPLimitDoesNotBurnAnotherIPsEmailBudget(t *testing.T) {
	s, pool := newSvc(t, Options{})
	ctx := context.Background()
	// One IP exhausts its own budget by asking for a victim's address, then
	// keeps hammering.
	for range SigninLimit + 20 {
		_ = s.RequestSignIn(ctx, "victim@example.org", "192.0.2.1")
	}
	if n := count(t, pool, `SELECT count(*) FROM signin_requests`); n != SigninLimit {
		t.Fatalf("requests accepted = %d, want %d", n, SigninLimit)
	}
	if n := count(t, pool, `SELECT count FROM rate_counters WHERE key = 'signin:email:`+HashEmail("victim@example.org")+`'`); n != SigninLimit {
		t.Fatalf("victim's counter = %d, want %d: a limited IP kept counting", n, SigninLimit)
	}
	// A second IP asking for a different address is untouched.
	if err := s.RequestSignIn(ctx, "other@example.org", "192.0.2.1"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("limited IP: %v", err)
	}
	if err := s.RequestSignIn(ctx, "other@example.org", "192.0.2.2"); err != nil {
		t.Fatalf("second IP, other address: %v", err)
	}
}

func TestSuspendedTenantSessionsAreRefused(t *testing.T) {
	s, pool := newSvc(t, Options{BaseURL: "http://127.0.0.1:8090"})
	ctx := context.Background()
	newAccount(t, s, "a@example.org")
	newAccount(t, s, "b@example.org")
	login := func(email string) string {
		_ = s.RequestSignIn(ctx, email, "192.0.2.1")
		_, _ = s.ProcessSignInRequests(ctx)
		msgs := outbox(t, pool)
		sec, _, err := s.ConsumeMagicLink(ctx, tokenFrom(t, msgs[len(msgs)-1]), "ua")
		if err != nil {
			t.Fatal(err)
		}
		return sec
	}
	a, b := login("a@example.org"), login("b@example.org")
	if _, err := s.LookupSession(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE tenants SET status_before_suspend = status, status = 'suspended' WHERE id = (SELECT tenant_id FROM accounts WHERE email = 'a@example.org')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LookupSession(ctx, a); !errors.Is(err, ErrNoSession) {
		t.Fatalf("suspended tenant's session: %v", err)
	}
	if _, err := s.LookupSession(ctx, b); err != nil {
		t.Fatalf("another tenant's session: %v", err)
	}
	// Unsuspended, the same session works again.
	if _, err := pool.Exec(ctx, `UPDATE tenants SET status = status_before_suspend WHERE status = 'suspended'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LookupSession(ctx, a); err != nil {
		t.Fatalf("after unsuspend: %v", err)
	}
}
