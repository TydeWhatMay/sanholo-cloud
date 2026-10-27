package admin

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lhaig/sanholo-cloud/internal/updater"
)

const testDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func withUpdater(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t, 20)
	f.svc.SetUpdater(updater.New(f.pool, nil, updater.Options{BatchSize: 5}))
	return f
}

type auditRow struct{ actor, action, target, details string }

func (f *fixture) auditRows(t *testing.T) []auditRow {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `SELECT actor, action, target, details::text FROM audit_log ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		if err := rows.Scan(&r.actor, &r.action, &r.target, &r.details); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func TestReleaseActionsAreAudited(t *testing.T) {
	f := withUpdater(t)
	ctx := context.Background()
	_, tenantID := f.account(t, "ann@example.org")

	steps := []struct {
		name   string
		do     func() error
		action string
		target string
		detail string
	}{
		{"add", func() error {
			return f.svc.AddRelease(ctx, "acct-1", "v0.8.0", testDigest, "notes", "https://example.org/r")
		}, "release.add", "release:0.8.0", testDigest},
		{"canary", func() error { return f.svc.SetCanary(ctx, ActorCLI, tenantID) }, "tenant.canary", "tenant:" + tenantID, ""},
		{"start", func() error { return f.svc.StartRollout(ctx, "acct-1", "0.8.0") }, "rollout.start", "release:0.8.0", `"batches"`},
	}
	for i, s := range steps {
		if err := s.do(); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		rows := f.auditRows(t)
		if len(rows) != i+1 {
			t.Fatalf("%s: %d audit rows, want %d", s.name, len(rows), i+1)
		}
		got := rows[i]
		if got.action != s.action || got.target != s.target || !strings.Contains(got.details, s.detail) {
			t.Errorf("%s: audit = %+v", s.name, got)
		}
	}
	if rows := f.auditRows(t); rows[0].actor != "acct-1" || rows[1].actor != "cli" {
		t.Errorf("audit actors = %q, %q", rows[0].actor, rows[1].actor)
	}

	// Halted by hand: resume, then halt again and abandon.
	if _, err := f.pool.Exec(ctx, `UPDATE releases SET state = 'halted', halted_tenant = 'x', halt_reason = 'the deployment failed' WHERE version = '0.8.0'`); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.ResumeRollout(ctx, "acct-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE releases SET state = 'halted', halted_tenant = 'x', halt_reason = 'the deployment failed' WHERE version = '0.8.0'`); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.AbandonRollout(ctx, "acct-1"); err != nil {
		t.Fatal(err)
	}
	rows := f.auditRows(t)
	if len(rows) != 5 || rows[3].action != "rollout.resume" || rows[4].action != "rollout.abandon" {
		t.Fatalf("audit rows = %+v", rows)
	}
	if !strings.Contains(rows[4].details, "the deployment failed") || !strings.Contains(rows[4].details, `"halted_tenant": "x"`) {
		t.Errorf("abandon details = %s", rows[4].details)
	}
	if err := f.svc.SetCanary(ctx, ActorCLI, ""); err != nil {
		t.Fatal(err)
	}
	if rows := f.auditRows(t); rows[5].action != "tenant.canary_clear" {
		t.Errorf("clear audit = %+v", rows[5])
	}
}

func TestRefusedReleaseActionsWriteNoAuditRow(t *testing.T) {
	f := withUpdater(t)
	ctx := context.Background()
	if err := f.svc.AddRelease(ctx, ActorCLI, "0.8.0", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.AddRelease(ctx, ActorCLI, "0.9.0", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.StartRollout(ctx, ActorCLI, "0.8.0"); err != nil {
		t.Fatal(err)
	}
	base := len(f.auditRows(t))
	cases := []struct {
		name string
		err  error
		want error
	}{
		{"duplicate", f.svc.AddRelease(ctx, ActorCLI, "v0.8.0", "", "", ""), ErrReleaseExists},
		{"bad version", f.svc.AddRelease(ctx, ActorCLI, "soon", "", "", ""), ErrReleaseInvalid},
		{"bad digest", f.svc.AddRelease(ctx, ActorCLI, "1.0.0", "sha256:00", "", ""), ErrReleaseInvalid},
		{"second rollout", f.svc.StartRollout(ctx, ActorCLI, "0.9.0"), ErrRolloutActive},
		{"same rollout again", f.svc.StartRollout(ctx, ActorCLI, "0.8.0"), ErrState},
		{"unknown release", f.svc.StartRollout(ctx, ActorCLI, "7.7.7"), ErrNotFound},
		{"resume while rolling", f.svc.ResumeRollout(ctx, ActorCLI), ErrState},
		{"abandon while rolling", f.svc.AbandonRollout(ctx, ActorCLI), ErrState},
		{"unknown canary", f.svc.SetCanary(ctx, ActorCLI, "nope"), ErrNotFound},
	}
	for _, tc := range cases {
		if !errors.Is(tc.err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, tc.err, tc.want)
		}
	}
	if got := len(f.auditRows(t)); got != base {
		t.Errorf("refused actions left %d audit rows", got-base)
	}
	if n := f.count(t, `SELECT count(*) FROM releases`); n != 2 {
		t.Errorf("%d releases", n)
	}
}

func TestReleaseChangeAndAuditShareATransaction(t *testing.T) {
	f := withUpdater(t)
	ctx := context.Background()
	f.failAudit(t)
	if err := f.svc.AddRelease(ctx, ActorCLI, "0.8.0", "", "", ""); err == nil {
		t.Fatal("AddRelease reported success without its audit row")
	}
	if n := f.count(t, `SELECT count(*) FROM releases`); n != 0 {
		t.Error("the release was kept without its audit row")
	}
}

func TestStartRolloutAndAuditShareATransaction(t *testing.T) {
	f := withUpdater(t)
	ctx := context.Background()
	if err := f.svc.AddRelease(ctx, ActorCLI, "0.8.0", "", "", ""); err != nil {
		t.Fatal(err)
	}
	f.failAudit(t)
	if err := f.svc.StartRollout(ctx, ActorCLI, "0.8.0"); err == nil {
		t.Fatal("StartRollout reported success without its audit row")
	}
	if n := f.count(t, `SELECT count(*) FROM releases WHERE state <> 'available'`); n != 0 {
		t.Error("the rollout started without its audit row")
	}
}

func TestReleaseActionsWithoutAnUpdater(t *testing.T) {
	f := newFixture(t, 20)
	ctx := context.Background()
	if err := f.svc.AddRelease(ctx, ActorCLI, "0.8.0", "", "", ""); !errors.Is(err, ErrState) {
		t.Errorf("AddRelease: %v", err)
	}
	if list, err := f.svc.Releases(ctx); err != nil || list != nil {
		t.Errorf("Releases = %v, %v", list, err)
	}
}
