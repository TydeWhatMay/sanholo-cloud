package admin

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lhaig/sanholo-cloud/internal/auth"
	"github.com/lhaig/sanholo-cloud/internal/dbtest"
	"github.com/lhaig/sanholo-cloud/internal/nomad"
	"github.com/lhaig/sanholo-cloud/internal/nomad/nomadfake"
	"github.com/lhaig/sanholo-cloud/internal/provision"
)

const base = "https://app.example.org"

type fixture struct {
	pool *pgxpool.Pool
	auth *auth.Service
	svc  *Service
}

func newFixture(t *testing.T, maxTenants int) *fixture {
	t.Helper()
	pool := dbtest.Migrated(t)
	a := auth.New(pool, auth.Options{MaxTenants: maxTenants, BaseURL: base})
	return &fixture{pool: pool, auth: a, svc: New(pool, a)}
}

func (f *fixture) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *fixture) account(t *testing.T, email string) (accountID, tenantID string) {
	t.Helper()
	ctx := context.Background()
	code, _, err := f.auth.IssueInvite(ctx, email)
	if err != nil {
		t.Fatal(err)
	}
	if tenantID, err = f.auth.Redeem(ctx, code); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT id::text FROM accounts WHERE tenant_id = $1`, tenantID).Scan(&accountID); err != nil {
		t.Fatal(err)
	}
	return accountID, tenantID
}

// passkey stores a credential row for the account, as a finished
// registration would leave it.
func (f *fixture) passkey(t *testing.T, accountID string) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO passkeys (id, account_id, credential) VALUES (gen_random_uuid()::text::bytea, $1, '{}')`, accountID); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) waitlist(t *testing.T, email string) {
	t.Helper()
	if err := f.auth.JoinWaitlist(context.Background(), email); err != nil {
		t.Fatal(err)
	}
}

// failAudit makes every insert into audit_log fail, to prove the audit row
// and the change share a transaction.
func (f *fixture) failAudit(t *testing.T) {
	t.Helper()
	for _, q := range []string{
		`CREATE FUNCTION audit_boom() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'audit refused'; END $$`,
		`CREATE TRIGGER audit_boom BEFORE INSERT ON audit_log FOR EACH ROW EXECUTE FUNCTION audit_boom()`,
	} {
		if _, err := f.pool.Exec(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSetOperatorWritesAuditAsCLI(t *testing.T) {
	f := newFixture(t, 20)
	ctx := context.Background()
	id, _ := f.account(t, "op@example.org")
	f.passkey(t, id)

	if err := f.svc.SetOperator(ctx, ActorCLI, "OP@example.org", true); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT count(*) FROM accounts WHERE is_operator`); n != 1 {
		t.Fatalf("operators = %d", n)
	}
	var actor, action, target string
	if err := f.pool.QueryRow(ctx, `SELECT actor, action, target FROM audit_log`).Scan(&actor, &action, &target); err != nil {
		t.Fatal(err)
	}
	if actor != "cli" || action != "operator.grant" || target != "account:"+id {
		t.Errorf("audit = %q %q %q", actor, action, target)
	}
	if err := f.svc.SetOperator(ctx, ActorCLI, "op@example.org", false); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT count(*) FROM accounts WHERE is_operator`); n != 0 {
		t.Fatal("revoke left the flag set")
	}
	if n := f.count(t, `SELECT count(*) FROM audit_log WHERE action = 'operator.revoke'`); n != 1 {
		t.Fatal("revoke not audited")
	}
}

func TestSetOperatorUnknownAccount(t *testing.T) {
	f := newFixture(t, 20)
	err := f.svc.SetOperator(context.Background(), ActorCLI, "nobody@example.org", true)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if f.count(t, `SELECT count(*) FROM audit_log`) != 0 {
		t.Fatal("audit row for a change that did not happen")
	}
}

func TestChangeAndAuditShareATransaction(t *testing.T) {
	f := newFixture(t, 20)
	ctx := context.Background()
	opID, _ := f.account(t, "op@example.org")
	f.passkey(t, opID)
	_, tenantID := f.account(t, "t@example.org")
	f.waitlist(t, "w@example.org")
	f.failAudit(t)

	if err := f.svc.SetOperator(ctx, ActorCLI, "op@example.org", true); err == nil {
		t.Error("grant succeeded without its audit row")
	}
	if err := f.svc.Suspend(ctx, "someone", tenantID); err == nil {
		t.Error("suspend succeeded without its audit row")
	}
	if err := f.svc.IssueInvite(ctx, "someone", "w@example.org"); err == nil {
		t.Error("invite succeeded without its audit row")
	}
	if f.count(t, `SELECT count(*) FROM accounts WHERE is_operator`) != 0 {
		t.Error("operator flag changed")
	}
	if f.count(t, `SELECT count(*) FROM tenants WHERE status = 'suspended'`) != 0 {
		t.Error("tenant suspended")
	}
	if f.count(t, `SELECT count(*) FROM invites WHERE email = 'w@example.org'`) != 0 || f.count(t, `SELECT count(*) FROM mail_outbox`) != 0 {
		t.Error("invite or mail was created")
	}
}

func TestIssueInviteQueuesMailAndHidesTheCode(t *testing.T) {
	f := newFixture(t, 20)
	ctx := context.Background()
	f.waitlist(t, "w@example.org")

	if err := f.svc.IssueInvite(ctx, "actor-1", "W@Example.org"); err != nil {
		t.Fatal(err)
	}
	var to, subject, body string
	if err := f.pool.QueryRow(ctx, `SELECT to_email, subject, body FROM mail_outbox`).Scan(&to, &subject, &body); err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(body, base+"/signup?code=")
	if !ok {
		t.Fatalf("no signup link in %q", body)
	}
	code, _, _ := strings.Cut(rest, "\n")
	if to != "w@example.org" || len(code) < 40 {
		t.Fatalf("to = %q, code = %q", to, code)
	}
	// The code exists in the mail and nowhere else in the database.
	for _, table := range []string{"invites", "audit_log", "waitlist"} {
		var dump string
		if err := f.pool.QueryRow(ctx, `SELECT coalesce(string_agg(t::text, ' '), '') FROM `+table+` t`).Scan(&dump); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(dump, code) {
			t.Errorf("plaintext code found in %s", table)
		}
	}
	var hash string
	_ = f.pool.QueryRow(ctx, `SELECT code_hash FROM invites`).Scan(&hash)
	if hash != auth.HashSecret(code) {
		t.Error("invite is not stored as the hash of the mailed code")
	}
	// It redeems.
	if _, err := f.auth.Redeem(ctx, code); err != nil {
		t.Fatalf("mailed code does not redeem: %v", err)
	}
	var actor, action, target, row string
	if err := f.pool.QueryRow(ctx, `SELECT actor, action, target, audit_log::text FROM audit_log`).Scan(&actor, &action, &target, &row); err != nil {
		t.Fatal(err)
	}
	var inviteID string
	if err := f.pool.QueryRow(ctx, `SELECT id::text FROM invites`).Scan(&inviteID); err != nil {
		t.Fatal(err)
	}
	if actor != "actor-1" || action != "invite.issue" || target != "invite:"+inviteID {
		t.Errorf("audit = %q %q %q", actor, action, target)
	}
	// Audit rows outlive an account, so they must not hold the address.
	if strings.Contains(strings.ToLower(row), "w@example.org") {
		t.Errorf("audit row holds the email address: %s", row)
	}
	// Nor an unsalted hash of it, which can be confirmed by guessing.
	if strings.Contains(row, auth.HashEmail("w@example.org")) {
		t.Errorf("audit row holds the hash of the email address: %s", row)
	}
}

func TestIssueInviteRefusals(t *testing.T) {
	f := newFixture(t, 20)
	ctx := context.Background()
	f.waitlist(t, "w@example.org")
	f.account(t, "has@example.org")
	f.waitlist(t, "has@example.org")

	cases := []struct {
		name  string
		email string
		want  error
	}{
		{"not on the waiting list", "stranger@example.org", ErrNotFound},
		{"already has an account", "has@example.org", auth.ErrAccountExists},
		{"not an address", "x\r\nBcc: y@z.co", nil},
	}
	for _, tc := range cases {
		err := f.svc.IssueInvite(ctx, "a", tc.email)
		if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
			t.Errorf("%s: err = %v", tc.name, err)
		}
	}
	if f.count(t, `SELECT count(*) FROM mail_outbox`) != 0 || f.count(t, `SELECT count(*) FROM audit_log`) != 0 {
		t.Fatal("a refused issue left mail or audit rows")
	}
}

func TestResendRevokesTheOldInvite(t *testing.T) {
	f := newFixture(t, 20)
	ctx := context.Background()
	f.waitlist(t, "w@example.org")
	if err := f.svc.IssueInvite(ctx, "a", "w@example.org"); err != nil {
		t.Fatal(err)
	}
	oldCode := codeFromOutbox(t, f, 1)
	if err := f.svc.ResendInvite(ctx, "a", "w@example.org"); err != nil {
		t.Fatal(err)
	}
	newCode := codeFromOutbox(t, f, 2)
	if oldCode == newCode {
		t.Fatal("resend reused the code")
	}
	if _, err := f.auth.InviteEmail(ctx, oldCode); !errors.Is(err, auth.ErrInviteInvalid) {
		t.Errorf("old code lookup = %v, want invalid", err)
	}
	if _, err := f.auth.Redeem(ctx, oldCode); !errors.Is(err, auth.ErrInviteInvalid) {
		t.Errorf("old code redeem = %v, want invalid", err)
	}
	if f.count(t, `SELECT count(*) FROM tenants`) != 0 {
		t.Fatal("the revoked code created a tenant")
	}
	if _, err := f.auth.Redeem(ctx, newCode); err != nil {
		t.Fatalf("new code: %v", err)
	}
	if f.count(t, `SELECT count(*) FROM audit_log WHERE action = 'invite.resend'`) != 1 {
		t.Error("resend not audited")
	}
	// Nothing to resend for an address that was never invited.
	if err := f.svc.ResendInvite(ctx, "a", "never@example.org"); !errors.Is(err, ErrNotFound) {
		t.Errorf("resend without invite = %v", err)
	}
}

func codeFromOutbox(t *testing.T, f *fixture, n int) string {
	t.Helper()
	var body string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT body FROM mail_outbox ORDER BY id OFFSET $1 LIMIT 1`, n-1).Scan(&body); err != nil {
		t.Fatal(err)
	}
	_, rest, _ := strings.Cut(body, "/signup?code=")
	code, _, _ := strings.Cut(rest, "\n")
	return code
}

func TestSuspendAndUnsuspend(t *testing.T) {
	f := newFixture(t, 20)
	ctx := context.Background()
	_, tenantID := f.account(t, "t@example.org")
	status := func() string {
		var s string
		_ = f.pool.QueryRow(ctx, `SELECT status FROM tenants WHERE id = $1`, tenantID).Scan(&s)
		return s
	}
	if err := f.svc.Unsuspend(ctx, "a", tenantID); !errors.Is(err, ErrState) {
		t.Errorf("unsuspend of a running tenant = %v", err)
	}
	if err := f.svc.Suspend(ctx, "a", tenantID); err != nil {
		t.Fatal(err)
	}
	if status() != "suspended" {
		t.Fatalf("status = %q", status())
	}
	if err := f.svc.Suspend(ctx, "a", tenantID); !errors.Is(err, ErrState) {
		t.Errorf("second suspend = %v", err)
	}
	if err := f.svc.Unsuspend(ctx, "a", tenantID); err != nil {
		t.Fatal(err)
	}
	if status() != "pending" { // the status it had before
		t.Fatalf("status after unsuspend = %q", status())
	}
	if err := f.svc.Suspend(ctx, "a", "no-such-tenant"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown tenant = %v", err)
	}
	// Two successful changes, two audit rows: refusals wrote none.
	if n := f.count(t, `SELECT count(*) FROM audit_log`); n != 2 {
		t.Fatalf("audit rows = %d, want 2", n)
	}
}

func TestSuspendedTenantStillHoldsItsPlace(t *testing.T) {
	f := newFixture(t, 1)
	ctx := context.Background()
	_, tenantID := f.account(t, "t@example.org")
	if err := f.svc.Suspend(ctx, "a", tenantID); err != nil {
		t.Fatal(err)
	}
	code, _, _ := f.auth.IssueInvite(ctx, "second@example.org")
	if _, err := f.auth.Redeem(ctx, code); !errors.Is(err, auth.ErrBetaFull) {
		t.Fatalf("redeem with a suspended tenant holding the only place = %v", err)
	}
}

func TestListings(t *testing.T) {
	f := newFixture(t, 20)
	ctx := context.Background()
	f.account(t, "a@example.org")
	f.waitlist(t, "w1@example.org")
	f.waitlist(t, "w2@example.org")
	if err := f.svc.IssueInvite(ctx, "a", "w2@example.org"); err != nil {
		t.Fatal(err)
	}
	tenants, err := f.svc.Tenants(ctx)
	if err != nil || len(tenants) != 1 || tenants[0].Email != "a@example.org" || tenants[0].Status != "pending" {
		t.Fatalf("tenants = %+v, %v", tenants, err)
	}
	wl, err := f.svc.Waitlist(ctx)
	if err != nil || len(wl) != 2 {
		t.Fatalf("waitlist = %+v, %v", wl, err)
	}
	states := map[string]string{}
	for _, w := range wl {
		states[w.Email] = w.InviteState
	}
	if states["w1@example.org"] != "none" || states["w2@example.org"] != "outstanding" {
		t.Errorf("invite states = %v", states)
	}
	rows, err := f.svc.Audit(ctx, 50)
	if err != nil || len(rows) != 1 || rows[0].Action != "invite.issue" {
		t.Fatalf("audit = %+v, %v", rows, err)
	}
	if rows[0].At.IsZero() || time.Since(rows[0].At) > time.Minute {
		t.Errorf("audit time = %v", rows[0].At)
	}
}

// No admin path can read a game token because none is stored. The schema is
// the proof: every column whose name mentions "token" is a hash (named
// *_hash), and a hash cannot be turned back into a token. Customer game
// tokens live in qm only (CLAUDE.md); a migration that adds a plain token
// column breaks this test.
func TestSchemaHasNoTokenColumnsExceptHashes(t *testing.T) {
	f := newFixture(t, 20)
	rows, err := f.pool.Query(context.Background(), `
		SELECT table_name || '.' || column_name FROM information_schema.columns
		WHERE table_schema = current_schema()
		  AND (column_name ILIKE '%token%' OR column_name ILIKE '%secret%' OR column_name ILIKE '%password%')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		seen++
		if !strings.HasSuffix(name, "_hash") {
			t.Errorf("column %s is not a hash; the control plane stores no tokens or secrets in clear", name)
		}
	}
	if seen == 0 {
		t.Fatal("found no token-like columns at all: the query is not looking at the schema")
	}
}

// The other direction: when the change itself fails at commit, the audit row
// that was written before it must not survive.
func TestFailedCommitLeavesNoAuditRow(t *testing.T) {
	f := newFixture(t, 20)
	ctx := context.Background()
	opID, _ := f.account(t, "op@example.org")
	f.passkey(t, opID)
	_, tenantID := f.account(t, "t@example.org")
	f.waitlist(t, "w@example.org")
	for _, q := range []string{
		`CREATE FUNCTION commit_boom() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'commit refused'; END $$`,
		`CREATE CONSTRAINT TRIGGER boom_tenants AFTER UPDATE ON tenants DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION commit_boom()`,
		`CREATE CONSTRAINT TRIGGER boom_accounts AFTER UPDATE ON accounts DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION commit_boom()`,
		`CREATE CONSTRAINT TRIGGER boom_invites AFTER INSERT ON invites DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION commit_boom()`,
	} {
		if _, err := f.pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.svc.SetOperator(ctx, ActorCLI, "op@example.org", true); err == nil {
		t.Error("grant committed")
	}
	if err := f.svc.Suspend(ctx, "a", tenantID); err == nil {
		t.Error("suspend committed")
	}
	if err := f.svc.IssueInvite(ctx, "a", "w@example.org"); err == nil {
		t.Error("invite committed")
	}
	if n := f.count(t, `SELECT count(*) FROM audit_log`); n != 0 {
		t.Fatalf("%d audit rows describe changes that did not happen", n)
	}
}

func TestSetOperatorNeedsAPasskey(t *testing.T) {
	f := newFixture(t, 20)
	ctx := context.Background()
	id, _ := f.account(t, "op@example.org")

	err := f.svc.SetOperator(ctx, ActorCLI, "op@example.org", true)
	if !errors.Is(err, ErrNoPasskey) {
		t.Fatalf("grant without a passkey = %v, want ErrNoPasskey", err)
	}
	if f.count(t, `SELECT count(*) FROM accounts WHERE is_operator`) != 0 || f.count(t, `SELECT count(*) FROM audit_log`) != 0 {
		t.Fatal("a refused grant changed something")
	}
	f.passkey(t, id)
	if err := f.svc.SetOperator(ctx, ActorCLI, "op@example.org", true); err != nil {
		t.Fatalf("grant with a passkey: %v", err)
	}
	// Revoking never needs a passkey.
	if _, err := f.pool.Exec(ctx, `DELETE FROM passkeys`); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SetOperator(ctx, ActorCLI, "op@example.org", false); err != nil {
		t.Fatalf("revoke without a passkey: %v", err)
	}
}

func TestSuspendRefusesTheActorsOwnTenant(t *testing.T) {
	f := newFixture(t, 20)
	ctx := context.Background()
	opID, opTenant := f.account(t, "op@example.org")
	_, otherTenant := f.account(t, "t@example.org")

	if err := f.svc.Suspend(ctx, opID, opTenant); !errors.Is(err, ErrOwnTenant) {
		t.Fatalf("own tenant = %v, want ErrOwnTenant", err)
	}
	var status string
	_ = f.pool.QueryRow(ctx, `SELECT status FROM tenants WHERE id = $1`, opTenant).Scan(&status)
	if status == "suspended" || f.count(t, `SELECT count(*) FROM audit_log`) != 0 {
		t.Fatalf("own tenant status %q, audit rows written", status)
	}
	// Another operator's tenant, and the CLI, are unaffected.
	if err := f.svc.Suspend(ctx, opID, otherTenant); err != nil {
		t.Fatalf("another tenant: %v", err)
	}
	if err := f.svc.Suspend(ctx, ActorCLI, opTenant); err != nil {
		t.Fatalf("cli suspends any tenant: %v", err)
	}
}

// provisioned gives the fixture a provisioning service over a fake Nomad and
// a tenant whose bot is running.
func (f *fixture) provisioned(t *testing.T, email string) (tenantID string, fake *nomadfake.Fake, prov *provision.Service) {
	t.Helper()
	ctx := context.Background()
	_, tenantID = f.account(t, email)
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO game_accounts (tenant_id, game_account_id, replicant_name, replicant_code) VALUES ($1, $2, 'Alpha', 'AAAA1111')`,
		tenantID, "replicant:"+tenantID); err != nil {
		t.Fatal(err)
	}
	fake = nomadfake.New(t, "tok-1234", "sanholo")
	nc, err := nomad.New(nomad.Config{Addr: fake.URL(), Token: "tok-1234", Namespace: "sanholo"})
	if err != nil {
		t.Fatal(err)
	}
	prov, err = provision.New(f.pool, nc, provision.Options{Settings: nomadfake.Settings(), Version: "0.7.1", Poll: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(prov.Close)
	f.svc.SetProvisioner(prov)
	if err := prov.Start(ctx, tenantID); err != nil {
		t.Fatal(err)
	}
	fake.SetPhase("sanholo-tenant-"+tenantID, nomadfake.Healthy)
	if _, err := prov.Refresh(ctx, tenantID); err != nil {
		t.Fatal(err)
	}
	return tenantID, fake, prov
}

func TestRestartInstanceWritesAuditAndRestarts(t *testing.T) {
	f := newFixture(t, 20)
	ctx := context.Background()
	tenantID, fake, prov := f.provisioned(t, "user@example.org")
	opID, _ := f.account(t, "op@example.org")

	if err := f.svc.RestartInstance(ctx, opID, tenantID); err != nil {
		t.Fatal(err)
	}
	prov.Wait()
	if fake.Restarts("sanholo-tenant-"+tenantID) != 1 {
		t.Error("the allocation was not restarted")
	}
	if n := f.count(t, `SELECT count(*) FROM audit_log WHERE action = 'instance.restart' AND actor = $1 AND target = $2`, opID, "tenant:"+tenantID); n != 1 {
		t.Errorf("%d audit rows, want 1", n)
	}
}

// The audit row and the new state commit together: when the audit write
// fails nothing changes and Nomad is never asked.
func TestRestartInstanceIsAtomicWithItsAuditRow(t *testing.T) {
	f := newFixture(t, 20)
	ctx := context.Background()
	tenantID, fake, prov := f.provisioned(t, "user@example.org")
	f.failAudit(t)
	if err := f.svc.RestartInstance(ctx, "op", tenantID); err == nil {
		t.Fatal("restart succeeded although the audit row could not be written")
	}
	inst, err := prov.Get(ctx, tenantID)
	if err != nil || inst.State != provision.StateRunning {
		t.Errorf("state after a failed audit = %+v, %v; want running", inst, err)
	}
	if fake.Restarts("sanholo-tenant-"+tenantID) != 0 {
		t.Error("Nomad was asked to restart although nothing was recorded")
	}
}

func TestRestartInstanceRefusals(t *testing.T) {
	f := newFixture(t, 20)
	ctx := context.Background()
	// Provisioning disabled.
	_, tenantNone := f.account(t, "none@example.org")
	if err := f.svc.RestartInstance(ctx, "op", tenantNone); !errors.Is(err, ErrState) {
		t.Errorf("disabled: %v, want ErrState", err)
	}
	tenantID, _, prov := f.provisioned(t, "user@example.org")
	if err := f.svc.RestartInstance(ctx, "op", tenantNone); !errors.Is(err, ErrState) {
		t.Errorf("tenant without an instance: %v, want ErrState", err)
	}
	if err := f.svc.RestartInstance(ctx, "op", "nobody"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown tenant: %v, want ErrNotFound", err)
	}
	if err := prov.Stop(ctx, tenantID, false); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.RestartInstance(ctx, "op", tenantID); !errors.Is(err, ErrState) {
		t.Errorf("stopped instance: %v, want ErrState", err)
	}
	if n := f.count(t, `SELECT count(*) FROM audit_log WHERE action = 'instance.restart'`); n != 0 {
		t.Errorf("%d audit rows for refused restarts", n)
	}
}

func TestTenantsListIncludesTheInstance(t *testing.T) {
	f := newFixture(t, 20)
	tenantID, _, _ := f.provisioned(t, "user@example.org")
	_, plain := f.account(t, "plain@example.org")
	list, err := f.svc.Tenants(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, tn := range list {
		switch tn.ID {
		case tenantID:
			if tn.InstanceState != "running" || tn.InstanceHealth != "healthy" || tn.InstanceVersion != "0.7.1" || tn.InstanceHealthAt == nil {
				t.Errorf("provisioned tenant: %+v", tn)
			}
		case plain:
			if tn.InstanceState != "" {
				t.Errorf("unprovisioned tenant: %+v", tn)
			}
		}
	}
}
