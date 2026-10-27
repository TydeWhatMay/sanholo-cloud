package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lhaig/sanholo-cloud/internal/auth"
	"github.com/lhaig/sanholo-cloud/internal/connect"
	"github.com/lhaig/sanholo-cloud/internal/dbtest"
	"github.com/lhaig/sanholo-cloud/internal/deletionlog"
	"github.com/lhaig/sanholo-cloud/internal/game"
	"github.com/lhaig/sanholo-cloud/internal/game/gamefake"
	"github.com/lhaig/sanholo-cloud/internal/secrets"
	"github.com/lhaig/sanholo-cloud/internal/secrets/qmfake"
)

// After a restore from backup the database holds accounts that were deleted
// later. The deletion log is not part of the backup; reapply deletes them
// again, and only them.
func TestReapplyDeletesAgainWhatTheLogRecords(t *testing.T) {
	pool := dbtest.Migrated(t)
	ctx := context.Background()
	qm := qmfake.New(t, "cp-token")
	sec, err := secrets.New(secrets.Config{Addr: qm.URL(), Token: "cp-token", Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	gc, err := game.New(game.Config{BaseURL: gamefake.New(t).URL(), Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := connect.New(pool, sec, gc, slog.New(slog.DiscardHandler), 1)
	if err != nil {
		t.Fatal(err)
	}
	// "Restored" state: two tenants that exist in the backup. The log says
	// the first was deleted after the backup was taken.
	for _, q := range []string{
		`INSERT INTO tenants (id) VALUES ('deleted-later'), ('kept-tenant'), ('half-deleted')`,
		`UPDATE tenants SET status = 'deleting' WHERE id = 'half-deleted'`,
		`INSERT INTO accounts (tenant_id, email, email_hash) VALUES
		   ('deleted-later', 'gone@example.org', '` + auth.HashEmail("gone@example.org") + `'),
		   ('kept-tenant', 'stay@example.org', '` + auth.HashEmail("stay@example.org") + `'),
		   ('half-deleted', 'half@example.org', '` + auth.HashEmail("half@example.org") + `')`,
		`INSERT INTO waitlist (email) VALUES ('gone@example.org')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	path := filepath.Join(t.TempDir(), "deletions.jsonl")
	log := deletionlog.New(path)
	for _, tenant := range []string{"deleted-later", "never-existed-here", "deleted-later", "half-deleted"} {
		if err := log.Append(deletionlog.Entry{At: time.Now(), Tenant: tenant, Account: "acct-of-" + tenant}); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := reapplyDeletions(ctx, conn, path, &out); err != nil {
		t.Fatalf("reapply: %v\n%s", err, out.String())
	}
	for _, tenant := range []string{"deleted-later", "half-deleted"} {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM accounts WHERE tenant_id = $1`, tenant).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s: %d accounts after the reapply (%v)", tenant, n, err)
		}
	}
	var kept, waiting, audits int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM accounts WHERE tenant_id = 'kept-tenant'`).Scan(&kept)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM waitlist WHERE lower(email) = 'gone@example.org'`).Scan(&waiting)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'account.delete' AND actor = 'cli'`).Scan(&audits)
	if kept != 1 {
		t.Error("an account the log does not name was deleted")
	}
	if waiting != 0 {
		t.Error("the restored waiting-list entry of a deleted account came back")
	}
	if audits != 1 { // half-deleted was already marked: no second mark
		t.Errorf("%d cli audit rows, want 1 (the half-deleted tenant is not marked twice)", audits)
	}
	for _, want := range []string{"deleted-later: deleted again", "never-existed-here: nothing to delete", "half-deleted: deleted again", "3 tenants in the log, 0 failed"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	// A second run finds nothing to do.
	out.Reset()
	if err := reapplyDeletions(ctx, conn, path, &out); err != nil || !strings.Contains(out.String(), "deleted-later: nothing to delete") {
		t.Errorf("second run: %v\n%s", err, out.String())
	}
}

func TestReapplyRefusesAnUnreadableLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d")
	if err := os.WriteFile(path, []byte("garbage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reapplyDeletions(context.Background(), nil, path, &bytes.Buffer{}); err == nil {
		t.Error("an unreadable log was accepted")
	}
}

func TestDeletionsCommandUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"nope"}, {"reapply", "a", "b"}} {
		if err := runDeletions(context.Background(), args, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Errorf("args %v: %v", args, err)
		}
	}
}
