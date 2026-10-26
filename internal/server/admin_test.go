package server

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/lhaig/sanholo-cloud/internal/admin"
)

// operator returns a client holding a passkey session for a new operator
// account, and a second client holding only a magic-link session for it.
func operator(t *testing.T, e *env, email string) (viaPasskey, viaLink *client) {
	t.Helper()
	e.account(t, email)
	if err := e.admin.SetOperator(context.Background(), admin.ActorCLI, email, true); err != nil {
		t.Fatal(err)
	}
	viaLink = e.signedIn(t, email)
	au := addPasskey(t, viaLink)
	viaPasskey = e.client(t)
	if r := passkeySignIn(t, viaPasskey, au); r.status != 200 {
		t.Fatalf("operator passkey sign-in: %d %s", r.status, r.body)
	}
	return viaPasskey, viaLink
}

func TestAdminAccess(t *testing.T) {
	e := passkeyEnv(t)
	op, opLink := operator(t, e, "op@example.org")
	e.account(t, "user@example.org")
	user := e.signedIn(t, "user@example.org")
	addPasskey(t, user)

	for _, path := range []string{"/admin", "/admin/waitlist", "/admin/audit"} {
		cases := []struct {
			name   string
			c      *client
			status int
			want   string // substring of the body
			never  string
		}{
			{"anonymous", e.client(t), 303, "", ""},
			{"non-operator, magic link", user, 404, "", "user@example.org"},
			{"operator, magic link", opLink, 403, `data-passkey="login"`, "user@example.org"},
			{"operator, passkey", op, 200, "Admin", ""},
		}
		for _, tc := range cases {
			t.Run(path+" "+tc.name, func(t *testing.T) {
				r := tc.c.get(path, nil)
				if r.status != tc.status {
					t.Fatalf("status %d, want %d: %s", r.status, tc.status, r.body)
				}
				if tc.want != "" && !strings.Contains(r.body, tc.want) {
					t.Errorf("body lacks %q: %s", tc.want, r.body)
				}
				if tc.never != "" && strings.Contains(r.body, tc.never) {
					t.Errorf("body shows %q", tc.never)
				}
				if tc.status == 303 && r.header.Get("Location") != "/signin" {
					t.Errorf("redirect to %q", r.header.Get("Location"))
				}
			})
		}
	}
}

func TestAdminDemotionAppliesAtOnce(t *testing.T) {
	e := passkeyEnv(t)
	op, _ := operator(t, e, "op@example.org")
	if r := op.get("/admin", nil); r.status != 200 {
		t.Fatalf("before: %d", r.status)
	}
	if err := e.admin.SetOperator(context.Background(), admin.ActorCLI, "op@example.org", false); err != nil {
		t.Fatal(err)
	}
	if r := op.get("/admin", nil); r.status != 404 {
		t.Fatalf("after revoke: %d, want 404", r.status)
	}
}

func TestAdminPostsRefusedWithoutOperatorPasskey(t *testing.T) {
	e := passkeyEnv(t)
	_, opLink := operator(t, e, "op@example.org")
	e.account(t, "user@example.org")
	user := e.signedIn(t, "user@example.org")
	var tenantID string
	_ = e.pool.QueryRow(context.Background(), `SELECT tenant_id FROM accounts WHERE email = 'user@example.org'`).Scan(&tenantID)
	e.svc.JoinWaitlist(context.Background(), "w@example.org")

	posts := []struct {
		path string
		form url.Values
	}{
		{"/admin/tenants/" + tenantID + "/suspend", nil},
		{"/admin/waitlist/invite", url.Values{"email": {"w@example.org"}}},
		{"/admin/waitlist/resend", url.Values{"email": {"w@example.org"}}},
	}
	for _, p := range posts {
		for name, c := range map[string]*client{"anonymous": e.client(t), "non-operator": user, "operator by link": opLink} {
			want := map[string]int{"anonymous": 303, "non-operator": 404, "operator by link": 403}[name]
			if r := c.post(p.path, p.form); r.status != want {
				t.Errorf("%s %s: %d, want %d", name, p.path, r.status, want)
			}
		}
	}
	if n := rows(t, e.pool, `SELECT count(*) FROM tenants WHERE status = 'suspended'`); n != 0 {
		t.Error("a refused request suspended a tenant")
	}
	if rows(t, e.pool, `SELECT count(*) FROM audit_log WHERE action <> 'operator.grant'`) != 0 || rows(t, e.pool, `SELECT count(*) FROM mail_outbox WHERE to_email = 'w@example.org'`) != 0 {
		t.Error("a refused request left audit rows or mail")
	}
}

func TestAdminPostsNeedCSRF(t *testing.T) {
	e := passkeyEnv(t)
	op, _ := operator(t, e, "op@example.org")
	e.account(t, "user@example.org")
	var tenantID string
	_ = e.pool.QueryRow(context.Background(), `SELECT tenant_id FROM accounts WHERE email = 'user@example.org'`).Scan(&tenantID)
	if r := op.do("POST", "/admin/tenants/"+tenantID+"/suspend", url.Values{}, nil); r.status != 403 {
		t.Fatalf("no csrf token: %d", r.status)
	}
	if r := op.do("POST", "/admin/tenants/"+tenantID+"/suspend", url.Values{"csrf": {"wrong"}}, nil); r.status != 403 {
		t.Fatalf("wrong csrf token: %d", r.status)
	}
	if n := rows(t, e.pool, `SELECT count(*) FROM tenants WHERE status = 'suspended'`); n != 0 {
		t.Fatal("tenant suspended without a CSRF token")
	}
}

func TestAdminActions(t *testing.T) {
	e := passkeyEnv(t)
	ctx := context.Background()
	op, _ := operator(t, e, "op@example.org")
	e.account(t, "user@example.org")
	var tenantID, opID string
	_ = e.pool.QueryRow(ctx, `SELECT tenant_id FROM accounts WHERE email = 'user@example.org'`).Scan(&tenantID)
	_ = e.pool.QueryRow(ctx, `SELECT id::text FROM accounts WHERE email = 'op@example.org'`).Scan(&opID)
	_ = e.svc.JoinWaitlist(ctx, "w@example.org")

	list := op.get("/admin", nil)
	for _, want := range []string{"user@example.org", tenantID, "pending", "not provisioned"} {
		if !strings.Contains(list.body, want) {
			t.Errorf("tenant list lacks %q", want)
		}
	}

	pages := []string{list.body}
	step := func(path string, form url.Values, wantLoc string) {
		t.Helper()
		r := op.post(path, form)
		if r.status != 303 || !strings.HasPrefix(r.header.Get("Location"), wantLoc) {
			t.Fatalf("POST %s: %d %q %s", path, r.status, r.header.Get("Location"), r.body)
		}
		pages = append(pages, r.body, op.get(r.header.Get("Location"), nil).body)
	}
	step("/admin/waitlist/invite", url.Values{"email": {"w@example.org"}}, "/admin/waitlist")
	var body string
	if err := e.pool.QueryRow(ctx, `SELECT body FROM mail_outbox WHERE to_email = 'w@example.org'`).Scan(&body); err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(body, baseURL+"/signup?code=")
	if !ok {
		t.Fatalf("invite mail has no signup link: %q", body)
	}
	code, _, _ := strings.Cut(rest, "\n")

	step("/admin/waitlist/resend", url.Values{"email": {"w@example.org"}}, "/admin/waitlist")
	step("/admin/tenants/"+tenantID+"/suspend", nil, "/admin")
	if rows(t, e.pool, `SELECT count(*) FROM tenants WHERE status = 'suspended'`) != 1 {
		t.Fatal("tenant not suspended")
	}
	if !strings.Contains(op.get("/admin", nil).body, "suspended") {
		t.Error("list does not show the suspension")
	}
	step("/admin/tenants/"+tenantID+"/unsuspend", nil, "/admin")
	if rows(t, e.pool, `SELECT count(*) FROM tenants WHERE status = 'suspended'`) != 0 {
		t.Fatal("tenant still suspended")
	}

	// One audit row per action, each naming the operator account.
	if n := rows(t, e.pool, `SELECT count(*) FROM audit_log WHERE actor = $1`, opID); n != 4 {
		t.Fatalf("audit rows by the operator = %d, want 4", n)
	}
	audit := op.get("/admin/audit", nil)
	for _, want := range []string{"invite.issue", "invite.resend", "tenant.suspend", "tenant.unsuspend", "operator.grant", "cli", opID} {
		if !strings.Contains(audit.body, want) {
			t.Errorf("audit page lacks %q", want)
		}
	}
	// The invite code is in the email only: no admin page ever showed it,
	// and the first code was revoked by the resend.
	pages = append(pages, audit.body, op.get("/admin/waitlist", nil).body)
	for i, p := range pages {
		if strings.Contains(p, code) || strings.Contains(p, "code=") {
			t.Errorf("page %d shows an invite code", i)
		}
	}
	if _, err := e.svc.InviteEmail(ctx, code); err == nil {
		t.Error("the first invite code still works after a resend")
	}
	// Repeating a change that no longer applies is an error page, no audit row.
	before := rows(t, e.pool, `SELECT count(*) FROM audit_log`)
	if r := op.post("/admin/tenants/"+tenantID+"/unsuspend", nil); r.status != 409 {
		t.Errorf("unsuspend of a running tenant: %d", r.status)
	}
	if r := op.post("/admin/tenants/nope/suspend", nil); r.status != 404 {
		t.Errorf("unknown tenant: %d", r.status)
	}
	if rows(t, e.pool, `SELECT count(*) FROM audit_log`) != before {
		t.Error("refused actions were audited")
	}
}
