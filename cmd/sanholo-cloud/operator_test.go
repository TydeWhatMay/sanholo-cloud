package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/lhaig/sanholo-cloud/internal/admin"
	"github.com/lhaig/sanholo-cloud/internal/auth"
	"github.com/lhaig/sanholo-cloud/internal/dbtest"
)

func TestOperatorCommand(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Migrated(t)
	a := auth.New(pool, auth.Options{MaxTenants: 5, BaseURL: "http://127.0.0.1:8090"})
	svc := admin.New(pool, a)
	code, _, _ := a.IssueInvite(ctx, "op@example.org")
	if _, err := a.Redeem(ctx, code); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	// No passkey yet: the grant is refused and says what to do.
	err := operatorCmd(ctx, svc, []string{"grant", "op@example.org"}, &out)
	if err == nil || !strings.Contains(err.Error(), "passkey") {
		t.Fatalf("grant without a passkey: %v", err)
	}
	var on bool
	_ = pool.QueryRow(ctx, `SELECT is_operator FROM accounts`).Scan(&on)
	if on {
		t.Fatal("a refused grant set the flag")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO passkeys (id, account_id, credential) SELECT 'k', id, '{}' FROM accounts`); err != nil {
		t.Fatal(err)
	}
	flag := func() bool {
		var on bool
		_ = pool.QueryRow(ctx, `SELECT is_operator FROM accounts`).Scan(&on)
		return on
	}

	if err := operatorCmd(ctx, svc, []string{"grant", "op@example.org"}, &out); err != nil || !flag() {
		t.Fatalf("grant: %v, flag %v", err, flag())
	}
	if !strings.Contains(out.String(), "granted") {
		t.Errorf("output %q", out.String())
	}
	if err := operatorCmd(ctx, svc, []string{"revoke", "op@example.org"}, &out); err != nil || flag() {
		t.Fatalf("revoke: %v, flag %v", err, flag())
	}
	var actors []string
	rows, _ := pool.Query(ctx, `SELECT actor FROM audit_log ORDER BY id`)
	for rows.Next() {
		var a string
		_ = rows.Scan(&a)
		actors = append(actors, a)
	}
	if strings.Join(actors, ",") != "cli,cli" {
		t.Errorf("audit actors = %v", actors)
	}

	for _, bad := range [][]string{nil, {"grant"}, {"promote", "op@example.org"}, {"grant", "a@b.co", "x"}, {"grant", "nobody@example.org"}} {
		if err := operatorCmd(ctx, svc, bad, &out); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	if flag() {
		t.Error("a refused command changed the flag")
	}
}
