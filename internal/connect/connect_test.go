package connect_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lhaig/sanholo-cloud/internal/connect"
	"github.com/lhaig/sanholo-cloud/internal/dbtest"
	"github.com/lhaig/sanholo-cloud/internal/game"
	"github.com/lhaig/sanholo-cloud/internal/game/gamefake"
	"github.com/lhaig/sanholo-cloud/internal/secrets"
	"github.com/lhaig/sanholo-cloud/internal/secrets/qmfake"
)

const (
	cpToken = "cp-client-token-7f3a"
	tokenA  = "GAMETOKEN-aaaaaaaaaaaaaaaa"
	tokenA2 = "GAMETOKEN-a2a2a2a2a2a2a2a2" // a second token, same game account as tokenA
	tokenB  = "GAMETOKEN-bbbbbbbbbbbbbbbb"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type fixture struct {
	pool *pgxpool.Pool
	qm   *qmfake.Fake
	gm   *gamefake.Fake
	svc  *connect.Service
	logs *syncBuf
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := dbtest.Migrated(t)
	qm := qmfake.New(t, cpToken)
	gm := gamefake.New(t)
	logs := &syncBuf{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	sec, err := secrets.New(secrets.Config{Addr: qm.URL(), Token: cpToken, Timeout: 2 * time.Second, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	gc, err := game.New(game.Config{BaseURL: gm.URL(), Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := connect.New(pool, sec, gc, log, 1)
	if err != nil {
		t.Fatal(err)
	}
	// Account X has two tokens; account Y has one.
	gm.AddAccount(tokenA, gamefake.Replicant{Code: "AAAA1111", Name: "Alpha", CreatedAt: "2026-01-01T00:00:00Z"})
	gm.AddAccount(tokenA2, gamefake.Replicant{Code: "AAAA1111", Name: "Alpha", CreatedAt: "2026-01-01T00:00:00Z"})
	gm.AddAccount(tokenB, gamefake.Replicant{Code: "BBBB2222", Name: "Bravo", CreatedAt: "2026-01-02T00:00:00Z"})
	return &fixture{pool: pool, qm: qm, gm: gm, svc: svc, logs: logs}
}

func (f *fixture) tenant(t *testing.T, id string) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO tenants (id) VALUES ($1)`, id); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) rows(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM game_accounts`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func gamePath(tenant string) string { return "sanholo/tenants/" + tenant + "/game" }

// connectAs runs Begin and Confirm and fails the test on error.
func (f *fixture) connectAs(t *testing.T, acct, tenant, token string) connect.Connected {
	t.Helper()
	_, secret, err := f.svc.Begin(context.Background(), acct, tenant, token)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	c, err := f.svc.Confirm(context.Background(), acct, tenant, secret)
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	return c
}

func TestBeginWritesNothingConfirmStores(t *testing.T) {
	f := newFixture(t)
	f.tenant(t, "ann")
	ctx := context.Background()
	prev, secret, err := f.svc.Begin(ctx, "acct-ann", "ann", tokenA)
	if err != nil {
		t.Fatal(err)
	}
	if prev.ReplicantName != "Alpha" || prev.ReplicantCode != "AAAA1111" || prev.Reconnect {
		t.Fatalf("preview = %+v", prev)
	}
	if n := len(f.gm.Requests()); n != 1 {
		t.Fatalf("%d game requests, want exactly 1", n)
	}
	if len(f.qm.Requests()) != 0 || f.rows(t) != 0 {
		t.Fatal("Begin wrote to qm or the database")
	}
	got, err := f.svc.Confirm(ctx, "acct-ann", "ann", secret)
	if err != nil {
		t.Fatal(err)
	}
	if got.ReplicantName != "Alpha" || got.ReplicantCode != "AAAA1111" {
		t.Fatalf("connected = %+v", got)
	}
	if n := len(f.gm.Requests()); n != 1 {
		t.Fatalf("Confirm made another game request (%d total)", n)
	}
	stored, ok := f.qm.Stored(gamePath("ann"))
	if !ok || stored["api_token"] != tokenA || stored["replicant_code"] != "AAAA1111" {
		t.Fatalf("qm holds %v", stored)
	}
	g, err := f.svc.Get(ctx, "ann")
	if err != nil || g == nil || g.ReplicantCode != "AAAA1111" || g.ConnectedAt.IsZero() {
		t.Fatalf("Get = %+v, %v", g, err)
	}
	var id string
	if err := f.pool.QueryRow(ctx, `SELECT game_account_id FROM game_accounts WHERE tenant_id = 'ann'`).Scan(&id); err != nil || id != "replicant:AAAA1111" {
		t.Fatalf("game_account_id = %q, %v", id, err)
	}
}

func TestGetBeforeConnect(t *testing.T) {
	f := newFixture(t)
	f.tenant(t, "ann")
	if g, err := f.svc.Get(context.Background(), "ann"); err != nil || g != nil {
		t.Fatalf("Get = %v, %v", g, err)
	}
}

func TestBeginFailuresWriteNothing(t *testing.T) {
	cases := []struct {
		name  string
		token string
		prep  func(f *fixture)
		want  error
	}{
		{"invalid token", "not-a-token", nil, connect.ErrRejected},
		{"malformed token", "bad token", nil, connect.ErrRejected},
		{"game down", tokenA, func(f *fixture) { f.gm.Close() }, connect.ErrGameUnreachable},
		{"game failing", tokenA, func(f *fixture) { f.gm.SetStatus(503) }, connect.ErrGameUnreachable},
		{"game rate limiting", tokenA, func(f *fixture) { f.gm.SetStatus(429) }, connect.ErrGameUnreachable},
		{"game 403", tokenA, func(f *fixture) { f.gm.SetStatus(403) }, connect.ErrRejected},
		{"no replicant", "empty", func(f *fixture) { f.gm.AddAccount("empty") }, connect.ErrNoReplicant},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.tenant(t, "ann")
			if tc.prep != nil {
				tc.prep(f)
			}
			_, secret, err := f.svc.Begin(context.Background(), "acct-ann", "ann", tc.token)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if secret != "" {
				t.Error("a secret was issued")
			}
			if len(f.qm.Requests()) != 0 || f.rows(t) != 0 {
				t.Fatal("something was written")
			}
		})
	}
}

func TestAnotherTenantsGameAccountIsRefusedByAccountIDNotToken(t *testing.T) {
	f := newFixture(t)
	f.tenant(t, "ann")
	f.tenant(t, "bob")
	f.connectAs(t, "acct-ann", "ann", tokenA)
	// Bob uses a DIFFERENT token for the same game account.
	_, secret, err := f.svc.Begin(context.Background(), "acct-bob", "bob", tokenA2)
	if !errors.Is(err, connect.ErrTaken) {
		t.Fatalf("err = %v, want ErrTaken", err)
	}
	if secret != "" {
		t.Error("a secret was issued")
	}
	if _, ok := f.qm.Stored(gamePath("bob")); ok || f.rows(t) != 1 {
		t.Fatal("bob wrote something")
	}
	if err := errors.Unwrap(err); err != nil {
		t.Errorf("error wraps detail: %v", err)
	}
	if strings.Contains(err.Error(), "ann") {
		t.Errorf("error names the other tenant: %v", err)
	}
}

func TestSecondGameAccountIsRefused(t *testing.T) {
	f := newFixture(t)
	f.tenant(t, "ann")
	f.connectAs(t, "acct-ann", "ann", tokenA)
	_, _, err := f.svc.Begin(context.Background(), "acct-ann", "ann", tokenB)
	if !errors.Is(err, connect.ErrLimit) {
		t.Fatalf("err = %v, want ErrLimit", err)
	}
	if f.qm.Versions(gamePath("ann")) != 1 || f.rows(t) != 1 {
		t.Fatal("the refusal changed state")
	}
}

// A pending connection validated while the tenant had nothing must not slip
// a second game account in when another browser connected first.
func TestSecondGameAccountRefusedAtConfirm(t *testing.T) {
	f := newFixture(t)
	f.tenant(t, "ann")
	ctx := context.Background()
	_, secretB, err := f.svc.Begin(ctx, "acct-ann2", "ann", tokenB)
	if err != nil {
		t.Fatal(err)
	}
	f.connectAs(t, "acct-ann", "ann", tokenA)
	if _, err := f.svc.Confirm(ctx, "acct-ann2", "ann", secretB); !errors.Is(err, connect.ErrLimit) {
		t.Fatalf("err = %v, want ErrLimit", err)
	}
	stored, _ := f.qm.Stored(gamePath("ann"))
	if stored["api_token"] != tokenA || f.qm.Versions(gamePath("ann")) != 1 {
		t.Fatal("the second game account reached qm")
	}
}

func TestReconnectSameAccountRotates(t *testing.T) {
	f := newFixture(t)
	f.tenant(t, "ann")
	f.connectAs(t, "acct-ann", "ann", tokenA)
	prev, secret, err := f.svc.Begin(context.Background(), "acct-ann", "ann", tokenA2)
	if err != nil {
		t.Fatal(err)
	}
	if !prev.Reconnect {
		t.Error("preview does not say this replaces the token")
	}
	if _, err := f.svc.Confirm(context.Background(), "acct-ann", "ann", secret); err != nil {
		t.Fatal(err)
	}
	stored, _ := f.qm.Stored(gamePath("ann"))
	if stored["api_token"] != tokenA2 {
		t.Fatalf("qm holds %v", stored)
	}
	if live := f.qm.Live("sanholo/tenants/ann/"); live != 1 {
		t.Fatalf("%d live versions after rotate, want 1 (the old token destroyed)", live)
	}
	if f.rows(t) != 1 {
		t.Fatalf("%d rows", f.rows(t))
	}
}

// Both tenants validate before either confirms, so both pass the early
// check. The unique constraint must stop the second, before qm is touched.
func TestLostRaceWritesNothingToQM(t *testing.T) {
	f := newFixture(t)
	f.tenant(t, "ann")
	f.tenant(t, "bob")
	ctx := context.Background()
	_, sa, err := f.svc.Begin(ctx, "acct-ann", "ann", tokenA)
	if err != nil {
		t.Fatal(err)
	}
	_, sb, err := f.svc.Begin(ctx, "acct-bob", "bob", tokenA2)
	if err != nil {
		t.Fatalf("both should pass the early check: %v", err)
	}
	if _, err := f.svc.Confirm(ctx, "acct-ann", "ann", sa); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Confirm(ctx, "acct-bob", "bob", sb); !errors.Is(err, connect.ErrTaken) {
		t.Fatalf("err = %v, want ErrTaken", err)
	}
	if f.qm.Versions(gamePath("bob")) != 0 {
		t.Fatal("the loser wrote to qm")
	}
	if _, ok := f.qm.Stored(gamePath("ann")); !ok || f.rows(t) != 1 {
		t.Fatal("the winner lost its connection")
	}
}

func TestConcurrentConfirmsConnectOnce(t *testing.T) {
	f := newFixture(t)
	const n = 6
	secrets := make([]string, n)
	for i := range n {
		id := fmt.Sprintf("t%d", i)
		f.tenant(t, id)
		_, s, err := f.svc.Begin(context.Background(), "acct-"+id, id, tokenA)
		if err != nil {
			t.Fatal(err)
		}
		secrets[i] = s
	}
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("t%d", i)
			_, errs[i] = f.svc.Confirm(context.Background(), "acct-"+id, id, secrets[i])
		}()
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, connect.ErrTaken):
			t.Errorf("unexpected error %v", err)
		}
	}
	if ok != 1 || f.rows(t) != 1 {
		t.Fatalf("%d succeeded, %d rows; want exactly one", ok, f.rows(t))
	}
	if live := f.qm.Live("sanholo/tenants/"); live != 1 {
		t.Fatalf("%d live secrets, want 1", live)
	}
}

func TestQMWriteFailureLeavesNoRow(t *testing.T) {
	f := newFixture(t)
	f.tenant(t, "ann")
	_, secret, err := f.svc.Begin(context.Background(), "acct-ann", "ann", tokenA)
	if err != nil {
		t.Fatal(err)
	}
	f.qm.SetOverride(func(r qmfake.Request) (int, string, bool) {
		if r.Method == "POST" && strings.Contains(r.URL, "/secret/data/") {
			return 503, `{"errors":["sealed"]}`, true
		}
		return 0, "", false
	})
	if _, err := f.svc.Confirm(context.Background(), "acct-ann", "ann", secret); !errors.Is(err, connect.ErrStore) {
		t.Fatalf("err = %v, want ErrStore", err)
	}
	if f.rows(t) != 0 {
		t.Fatal("a row was kept although qm did not store the token")
	}
	// A retry needs the token again: the pending entry is gone.
	if _, err := f.svc.Confirm(context.Background(), "acct-ann", "ann", secret); !errors.Is(err, connect.ErrExpired) {
		t.Fatalf("retry err = %v, want ErrExpired", err)
	}
	f.qm.SetOverride(nil)
	f.connectAs(t, "acct-ann", "ann", tokenA)
}

func TestQMDeniedLeavesNoRow(t *testing.T) {
	f := newFixture(t)
	f.tenant(t, "ann")
	_, secret, _ := f.svc.Begin(context.Background(), "acct-ann", "ann", tokenA)
	f.qm.SetOverride(func(r qmfake.Request) (int, string, bool) {
		if strings.Contains(r.URL, "/secret/data/") {
			return 403, `{}`, true
		}
		return 0, "", false
	})
	if _, err := f.svc.Confirm(context.Background(), "acct-ann", "ann", secret); !errors.Is(err, connect.ErrStore) {
		t.Fatalf("err = %v", err)
	}
	if f.rows(t) != 0 {
		t.Fatal("a row was kept")
	}
}

// qm stored the token but the commit failed: the stray secret must be purged,
// since this tenant had nothing before.
func TestCommitFailureAfterQMWritePurges(t *testing.T) {
	f := newFixture(t)
	f.tenant(t, "ann")
	ctx := context.Background()
	for _, q := range []string{
		`CREATE FUNCTION commit_boom() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'commit refused'; END $$`,
		`CREATE CONSTRAINT TRIGGER boom AFTER INSERT ON game_accounts DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION commit_boom()`,
	} {
		if _, err := f.pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	_, secret, _ := f.svc.Begin(ctx, "acct-ann", "ann", tokenA)
	if _, err := f.svc.Confirm(ctx, "acct-ann", "ann", secret); !errors.Is(err, connect.ErrStore) {
		t.Fatalf("err = %v", err)
	}
	if f.qm.Versions(gamePath("ann")) == 0 {
		t.Fatal("the test never reached the qm write; the fixture is vacuous")
	}
	if live := f.qm.Live("sanholo/tenants/ann/"); live != 0 {
		t.Fatalf("%d live secrets left in qm after a failed commit", live)
	}
	if f.rows(t) != 0 {
		t.Fatal("a row was kept")
	}
}

// qm's answer is lost after it stored the token (a timeout): a first connect
// purges, a reconnect must NOT purge the tenant's working token.
func TestLostQMAnswerPurgesOnlyAFirstConnect(t *testing.T) {
	f := newFixture(t)
	f.tenant(t, "ann")
	ctx := context.Background()
	lose := func(on bool) {
		if !on {
			f.qm.SetOverride(nil)
			return
		}
		f.qm.SetOverride(func(r qmfake.Request) (int, string, bool) {
			if r.Method == "POST" && strings.Contains(r.URL, "/secret/data/") {
				return 503, `{}`, true
			}
			return 0, "", false
		})
	}
	lose(true)
	_, s1, _ := f.svc.Begin(ctx, "acct-ann", "ann", tokenA)
	// The write landed although the answer was lost: simulate it by putting
	// the version into the store, as a request that timed out would have.
	f.qm.Seed(gamePath("ann"), map[string]string{"api_token": tokenA, "replicant_code": "AAAA1111"})
	if _, err := f.svc.Confirm(ctx, "acct-ann", "ann", s1); !errors.Is(err, connect.ErrStore) {
		t.Fatalf("err = %v", err)
	}
	if live := f.qm.Live("sanholo/tenants/ann/"); live != 0 {
		t.Fatalf("first connect left %d live secrets", live)
	}
	lose(false)
	f.connectAs(t, "acct-ann", "ann", tokenA)
	// Reconnect with the answer lost: the working credential survives.
	lose(true)
	_, s2, _ := f.svc.Begin(ctx, "acct-ann", "ann", tokenA2)
	if _, err := f.svc.Confirm(ctx, "acct-ann", "ann", s2); !errors.Is(err, connect.ErrStore) {
		t.Fatalf("err = %v", err)
	}
	if _, ok := f.qm.Stored(gamePath("ann")); !ok {
		t.Fatal("a failed reconnect destroyed the working credential")
	}
}

func TestPendingExpires(t *testing.T) {
	f := newFixture(t)
	f.tenant(t, "ann")
	now := time.Now()
	f.svc.SetClock(func() time.Time { return now })
	ctx := context.Background()
	_, secret, _ := f.svc.Begin(ctx, "acct-ann", "ann", tokenA)
	now = now.Add(connect.PendingTTL - time.Second)
	_, secret2, _ := f.svc.Begin(ctx, "acct-ann", "ann", tokenA) // a fresh one is unaffected
	now = now.Add(2 * time.Second)
	if _, err := f.svc.Confirm(ctx, "acct-ann", "ann", secret); !errors.Is(err, connect.ErrExpired) {
		t.Fatalf("old secret: err = %v", err)
	}
	now = now.Add(connect.PendingTTL)
	if _, err := f.svc.Confirm(ctx, "acct-ann", "ann", secret2); !errors.Is(err, connect.ErrExpired) {
		t.Fatalf("expired: err = %v", err)
	}
	if len(f.qm.Requests()) != 0 || f.rows(t) != 0 {
		t.Fatal("an expired confirmation wrote something")
	}
}

func TestPendingBoundToAccountAndSingleUse(t *testing.T) {
	f := newFixture(t)
	f.tenant(t, "ann")
	f.tenant(t, "bob")
	ctx := context.Background()
	_, secret, _ := f.svc.Begin(ctx, "acct-ann", "ann", tokenA)
	// Another signed-in account presenting Ann's cookie value.
	if _, err := f.svc.Confirm(ctx, "acct-bob", "bob", secret); !errors.Is(err, connect.ErrExpired) {
		t.Fatalf("other account: err = %v", err)
	}
	f.svc.Cancel("acct-bob", secret)
	// A second account of the same tenant presenting the secret: the tenant
	// matches, so only the account binding stops it.
	if _, err := f.svc.Confirm(ctx, "acct-ann-other", "ann", secret); !errors.Is(err, connect.ErrExpired) {
		t.Fatalf("other account, same tenant: err = %v", err)
	}
	f.svc.Cancel("acct-ann-other", secret)
	if _, err := f.svc.Confirm(ctx, "acct-ann", "ann", ""); !errors.Is(err, connect.ErrExpired) {
		t.Fatalf("empty secret: err = %v", err)
	}
	if _, err := f.svc.Confirm(ctx, "acct-ann", "ann", secret); err != nil {
		t.Fatalf("the owner lost the entry to a stranger: %v", err)
	}
	if _, err := f.svc.Confirm(ctx, "acct-ann", "ann", secret); !errors.Is(err, connect.ErrExpired) {
		t.Fatalf("second use: err = %v", err)
	}
	if f.qm.Versions(gamePath("ann")) != 1 {
		t.Fatal("the secret worked twice")
	}
}

func TestNewBeginReplacesTheOldPending(t *testing.T) {
	f := newFixture(t)
	f.tenant(t, "ann")
	ctx := context.Background()
	_, s1, _ := f.svc.Begin(ctx, "acct-ann", "ann", tokenA)
	_, s2, _ := f.svc.Begin(ctx, "acct-ann", "ann", tokenA2)
	if _, err := f.svc.Confirm(ctx, "acct-ann", "ann", s1); !errors.Is(err, connect.ErrExpired) {
		t.Fatalf("replaced entry still works: %v", err)
	}
	if _, err := f.svc.Confirm(ctx, "acct-ann", "ann", s2); err != nil {
		t.Fatal(err)
	}
	if stored, _ := f.qm.Stored(gamePath("ann")); stored["api_token"] != tokenA2 {
		t.Fatalf("qm holds %v", stored)
	}
}

func TestCancelDiscards(t *testing.T) {
	f := newFixture(t)
	f.tenant(t, "ann")
	ctx := context.Background()
	_, s, _ := f.svc.Begin(ctx, "acct-ann", "ann", tokenA)
	f.svc.Cancel("acct-ann", s)
	if _, err := f.svc.Confirm(ctx, "acct-ann", "ann", s); !errors.Is(err, connect.ErrExpired) {
		t.Fatalf("err = %v", err)
	}
}

func TestUnavailableWithoutQM(t *testing.T) {
	pool := dbtest.Migrated(t)
	gm := gamefake.New(t)
	sec, err := secrets.New(secrets.Config{})
	if err != nil {
		t.Fatal(err)
	}
	gc, _ := game.New(game.Config{BaseURL: gm.URL()})
	svc, err := connect.New(pool, sec, gc, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if svc.Available() {
		t.Fatal("available without qm")
	}
	if _, _, err := svc.Begin(context.Background(), "a", "ann", tokenA); !errors.Is(err, connect.ErrNotAvailable) {
		t.Fatalf("err = %v", err)
	}
	if _, err := svc.Confirm(context.Background(), "a", "ann", "x"); !errors.Is(err, connect.ErrNotAvailable) {
		t.Fatalf("err = %v", err)
	}
	if n := len(gm.Requests()); n != 0 {
		t.Fatalf("%d game requests although connecting is unavailable", n)
	}
	var nilSvc *connect.Service
	if nilSvc.Available() {
		t.Fatal("nil service available")
	}
}

func TestNewRefusesMoreThanOneGameAccount(t *testing.T) {
	for _, n := range []int{0, 2, 5} {
		if _, err := connect.New(nil, nil, nil, nil, n); err == nil {
			t.Errorf("max %d accepted", n)
		}
	}
}

// The token is in PostgreSQL nowhere, in any column of any table, and in no
// log line, whatever happened.
func TestTokenIsInNeitherPostgresNorLogs(t *testing.T) {
	f := newFixture(t)
	f.tenant(t, "ann")
	f.tenant(t, "bob")
	ctx := context.Background()
	f.connectAs(t, "acct-ann", "ann", tokenA)
	f.assertNoTokens(t)                                   // before the rotate below overwrites anything
	f.connectAs(t, "acct-ann", "ann", tokenA2)            // rotate
	_, _, _ = f.svc.Begin(ctx, "acct-bob", "bob", tokenA) // taken
	_, _, _ = f.svc.Begin(ctx, "acct-bob", "bob", "nope") // rejected
	f.gm.SetStatus(503)
	_, _, _ = f.svc.Begin(ctx, "acct-bob", "bob", tokenB) // unreachable
	f.gm.SetStatus(0)
	_, sb, _ := f.svc.Begin(ctx, "acct-bob", "bob", tokenB)
	f.qm.SetOverride(func(r qmfake.Request) (int, string, bool) { return 503, `{}`, true })
	_, _ = f.svc.Confirm(ctx, "acct-bob", "bob", sb) // qm failure
	f.qm.SetOverride(nil)

	f.assertNoTokens(t)
	if !strings.Contains(f.logs.String(), "connect:") {
		t.Fatal("no connect log lines captured; the log check is vacuous")
	}
}

// assertNoTokens fails if any test token is in the logs or in any column of
// any table.
func (f *fixture) assertNoTokens(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for _, tok := range []string{tokenA, tokenA2, tokenB} {
		if strings.Contains(f.logs.String(), tok) {
			t.Errorf("token %q is in the logs:\n%s", tok, f.logs.String())
		}
		rows, err := f.pool.Query(ctx, `
			SELECT table_name, column_name FROM information_schema.columns
			WHERE table_schema = current_schema()`)
		if err != nil {
			t.Fatal(err)
		}
		type col struct{ table, column string }
		var cols []col
		for rows.Next() {
			var c col
			if err := rows.Scan(&c.table, &c.column); err != nil {
				t.Fatal(err)
			}
			cols = append(cols, c)
		}
		rows.Close()
		if len(cols) < 20 {
			t.Fatalf("only %d columns scanned; the scan is vacuous", len(cols))
		}
		for _, c := range cols {
			q := fmt.Sprintf(`SELECT count(*) FROM %q t WHERE t.%q::text LIKE $1`, c.table, c.column)
			var n int
			if err := f.pool.QueryRow(ctx, q, "%"+tok+"%").Scan(&n); err != nil {
				t.Fatalf("%s.%s: %v", c.table, c.column, err)
			}
			if n != 0 {
				t.Errorf("token %q is stored in %s.%s", tok, c.table, c.column)
			}
		}
	}
}
