package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/lhaig/sanholo-cloud/internal/admin"
	"github.com/lhaig/sanholo-cloud/internal/auth"
	"github.com/lhaig/sanholo-cloud/internal/dbtest"
	"github.com/lhaig/sanholo-cloud/internal/updater"
)

const cliDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestReleaseCommand(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Migrated(t)
	a := auth.New(pool, auth.Options{MaxTenants: 5, BaseURL: "http://127.0.0.1:8090"})
	svc := admin.New(pool, a)
	svc.SetUpdater(updater.New(pool, nil, updater.Options{}))
	code, _, _ := a.IssueInvite(ctx, "ann@example.org")
	tenantID, err := a.Redeem(ctx, code)
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	run := func(args ...string) error {
		out.Reset()
		return releaseCmd(ctx, svc, args, &out)
	}
	if err := run("add", "v0.8.0", "--digest", cliDigest, "--notes", "first", "--url", "https://example.org/r"); err != nil {
		t.Fatal(err)
	}
	if err := run("add", "0.9.0"); err != nil {
		t.Fatal(err)
	}
	if err := run("list"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"v0.8.0", cliDigest, "v0.9.0", "tag", "available"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("list lacks %q: %q", want, out.String())
		}
	}
	if err := run("add", "0.8.0"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("duplicate: %v", err)
	}
	if err := run("canary", tenantID); err != nil {
		t.Fatal(err)
	}
	if err := run("canary", "nope"); err == nil {
		t.Error("an unknown canary tenant was accepted")
	}
	if err := run("start", "v0.8.0"); err != nil {
		t.Fatal(err)
	}
	if err := run("start", "0.9.0"); err == nil || !strings.Contains(err.Error(), "another rollout") {
		t.Errorf("second rollout: %v", err)
	}
	if err := run("resume"); err == nil {
		t.Error("resume of a rolling rollout was accepted")
	}
	if err := run("canary", "none"); err != nil {
		t.Fatal(err)
	}

	var actors, actions []string
	rows, _ := pool.Query(ctx, `SELECT actor, action FROM audit_log ORDER BY id`)
	for rows.Next() {
		var actor, action string
		_ = rows.Scan(&actor, &action)
		actors, actions = append(actors, actor), append(actions, action)
	}
	if got := strings.Join(actions, ","); got != "release.add,release.add,tenant.canary,rollout.start,tenant.canary_clear" {
		t.Errorf("audit actions = %s", got)
	}
	for _, actor := range actors {
		if actor != "cli" {
			t.Errorf("audit actor %q, want cli", actor)
		}
	}

	for _, bad := range [][]string{nil, {"add"}, {"add", "--digest", cliDigest}, {"add", "1.0.0", "extra"}, {"add", "1.0.0", "--bogus"}, {"list", "x"}, {"start"}, {"start", "a", "b"}, {"resume", "x"}, {"abandon", "x"}, {"canary"}, {"promote"}} {
		if err := run(bad...); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	if err := run("add", "1.0.0", "--digest", "sha256:00"); err == nil {
		t.Error("a bad digest was accepted")
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM releases`).Scan(&n)
	if n != 2 {
		t.Errorf("%d releases, want the two valid ones", n)
	}
}

func TestReleaseOverrideCanaryCommand(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Migrated(t)
	a := auth.New(pool, auth.Options{MaxTenants: 5, BaseURL: "http://127.0.0.1:8090"})
	svc := admin.New(pool, a)
	svc.SetUpdater(updater.New(pool, nil, updater.Options{}))
	var out bytes.Buffer
	run := func(args ...string) error {
		out.Reset()
		return releaseCmd(ctx, svc, args, &out)
	}
	if err := run("add", "0.8.0"); err != nil {
		t.Fatal(err)
	}
	if err := run("override-canary", "v0.8.0"); err != nil || !strings.Contains(out.String(), "overridden") {
		t.Fatalf("override: %v %q", err, out.String())
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'rollout.canary_override' AND actor = 'cli' AND target = 'release:0.8.0'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("audit rows = %d, %v", n, err)
	}
	if err := run("override-canary", "9.9.9"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("unknown release: %v", err)
	}
	if err := run("override-canary"); err == nil {
		t.Error("no version accepted")
	}
}
