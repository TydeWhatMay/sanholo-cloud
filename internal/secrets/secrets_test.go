package secrets_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lhaig/sanholo-cloud/internal/secrets"
	"github.com/lhaig/sanholo-cloud/internal/secrets/qmfake"
)

const (
	cpToken   = "cp-client-token-7f3a"
	roleID    = "ROLEID-3b1f-prov"
	secretID  = "SECRETID-9e77-prov"
	gameToken = "GAMETOKEN-d41d8cd98f00b204"
	gameCode  = "61C94360"
)

var creds = secrets.Credentials{APIToken: gameToken, ReplicantCode: gameCode}

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

// setup returns a client against a fresh fake and the captured log.
func setup(t *testing.T) (*secrets.Client, *qmfake.Fake, *syncBuf) {
	t.Helper()
	f := qmfake.New(t, cpToken)
	logs := &syncBuf{}
	c, err := secrets.New(secrets.Config{
		Addr: f.URL(), Token: cpToken, Timeout: 2 * time.Second,
		Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, f, logs
}

// clock is a settable time source for token renewal.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// setupRole returns an AppRole client against a fresh fake with a 30 second
// lease, its clock and the captured log.
func setupRole(t *testing.T) (*secrets.Client, *qmfake.Fake, *clock, *syncBuf) {
	t.Helper()
	f := qmfake.New(t, cpToken)
	f.AddAppRole(roleID, secretID)
	f.SetLease(30)
	clk := newClock()
	logs := &syncBuf{}
	c, err := secrets.New(secrets.Config{
		Addr: f.URL(), RoleID: roleID, SecretID: secretID, Timeout: 2 * time.Second, Now: clk.Now,
		Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, f, clk, logs
}

func TestNewValidation(t *testing.T) {
	const a = "https://qm.example.org"
	cases := []struct {
		name    string
		cfg     secrets.Config
		wantErr string
	}{
		{"disabled", secrets.Config{}, ""},
		{"https", secrets.Config{Addr: a, Token: "t"}, ""},
		{"http loopback", secrets.Config{Addr: "http://127.0.0.1:8222", Token: "t"}, ""},
		{"http localhost", secrets.Config{Addr: "http://localhost:8222", Token: "t"}, ""},
		{"http ipv6 loopback", secrets.Config{Addr: "http://[::1]:8222", Token: "t"}, ""},
		{"http real host", secrets.Config{Addr: "http://qm.example.org", Token: "t"}, "loopback"},
		{"http lookalike", secrets.Config{Addr: "http://localhost.example.org", Token: "t"}, "loopback"},
		{"ftp", secrets.Config{Addr: "ftp://qm.example.org", Token: "t"}, "https"},
		{"path", secrets.Config{Addr: "https://qm.example.org/x", Token: "t"}, "origin"},
		{"userinfo", secrets.Config{Addr: "https://u:p@qm.example.org", Token: "t"}, "origin"},
		{"no credentials", secrets.Config{Addr: a}, "role id"},
		{"both tokens", secrets.Config{Addr: a, Token: "t", TokenFile: "/x"}, "exactly one"},
		{"token without addr", secrets.Config{Token: "t"}, "no qm address"},
		{"role without addr", secrets.Config{RoleID: "r", SecretID: "s"}, "no qm address"},
		{"newline in token", secrets.Config{Addr: a, Token: "a\nb"}, "header"},
		{"missing token file", secrets.Config{Addr: a, TokenFile: "/nonexistent/qm-token"}, "token file"},
		{"approle", secrets.Config{Addr: a, RoleID: "r", SecretID: "s"}, ""},
		{"role without secret id", secrets.Config{Addr: a, RoleID: "r"}, "exactly one"},
		{"secret id without role", secrets.Config{Addr: a, SecretID: "s"}, "role id"},
		{"secret id and file", secrets.Config{Addr: a, RoleID: "r", SecretID: "s", SecretIDFile: "/x"}, "exactly one"},
		{"missing secret id file", secrets.Config{Addr: a, RoleID: "r", SecretIDFile: "/nonexistent/qm-sid"}, "secret id file"},
		{"newline in role id", secrets.Config{Addr: a, RoleID: "r\nx", SecretID: "s"}, "header"},
		{"approle and token", secrets.Config{Addr: a, RoleID: "r", SecretID: "s", Token: "t"}, "not both"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := secrets.New(tc.cfg)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			for _, v := range []string{tc.cfg.Token, tc.cfg.SecretID} {
				if len(v) > 1 && strings.Contains(err.Error(), v) {
					t.Fatalf("error echoes a secret: %v", err)
				}
			}
		})
	}
}

func TestDisabled(t *testing.T) {
	ctx := context.Background()
	c, err := secrets.New(secrets.Config{})
	if err != nil {
		t.Fatal(err)
	}
	var nilClient *secrets.Client
	for name, cl := range map[string]*secrets.Client{"disabled": c, "nil": nilClient} {
		if cl.Enabled() {
			t.Errorf("%s: Enabled", name)
		}
		checks := map[string]error{
			"put":   cl.Put(ctx, "abc", creds),
			"purge": cl.PurgeTenant(ctx, "abc"),
			"ready": cl.Ready(ctx),
		}
		_, checks["version"] = cl.Version(ctx, "abc")
		for op, err := range checks {
			if !errors.Is(err, secrets.ErrSecretsUnavailable) {
				t.Errorf("%s %s: err = %v, want ErrSecretsUnavailable", name, op, err)
			}
		}
	}
}

func TestInvalidTenantSendsNothing(t *testing.T) {
	c, f, _ := setup(t)
	ctx := context.Background()
	for _, id := range []string{
		"", "../victim", "a/b", "a/../b", "/", "A", "ABC", "a.b", "a b", "-a", "a-", "a\n", "a%2fb", "a?x=1", "é",
		strings.Repeat("a", 64),
	} {
		_, versionErr := c.Version(ctx, id)
		for op, err := range map[string]error{
			"put": c.Put(ctx, id, creds), "version": versionErr, "purge": c.PurgeTenant(ctx, id),
		} {
			if !errors.Is(err, secrets.ErrInvalidTenant) {
				t.Errorf("%s(%q): err = %v, want ErrInvalidTenant", op, id, err)
			}
		}
	}
	if n := len(f.Requests()); n != 0 {
		t.Fatalf("%d requests reached qm for invalid tenant ids", n)
	}
	// The boundary that is valid does go through.
	if err := c.Put(ctx, strings.Repeat("a", 63), creds); err != nil {
		t.Fatalf("63-character id: %v", err)
	}
}

func TestPut(t *testing.T) {
	c, f, _ := setup(t)
	ctx := context.Background()
	if err := c.Put(ctx, "t1", creds); err != nil {
		t.Fatal(err)
	}
	got, ok := f.Stored("sanholo/tenants/t1/game")
	if !ok || got["api_token"] != gameToken || got["replicant_code"] != gameCode || len(got) != 2 {
		t.Fatalf("stored = %v", got)
	}
	r := f.Requests()[0]
	if r.Method != http.MethodPost || r.URL != "/qm/v1/secret/data/sanholo/tenants/t1/game" || r.Header.Get("X-QM-Token") != cpToken {
		t.Fatalf("request = %+v", r)
	}
	if !strings.HasPrefix(r.Body, `{"data":{`) {
		t.Fatalf("body is not the {\"data\":{...}} envelope: %s", r.Body)
	}
	for _, bad := range []secrets.Credentials{{}, {APIToken: gameToken}, {ReplicantCode: gameCode}, {APIToken: "a\nb", ReplicantCode: gameCode}} {
		before := len(f.Requests())
		if err := c.Put(ctx, "t1", bad); !errors.Is(err, secrets.ErrInvalidCredentials) {
			t.Errorf("Put(%+v) err = %v", bad, err)
		}
		if len(f.Requests()) != before {
			t.Errorf("Put(%+v) sent a request", bad)
		}
	}
}

func TestVersion(t *testing.T) {
	c, f, _ := setup(t)
	ctx := context.Background()
	if v, err := c.Version(ctx, "t1"); err != nil || v != 0 {
		t.Fatalf("absent: %d, %v", v, err)
	}
	for want := 1; want <= 2; want++ {
		if err := c.Put(ctx, "t1", creds); err != nil {
			t.Fatal(err)
		}
		if v, err := c.Version(ctx, "t1"); err != nil || v != want {
			t.Fatalf("after %d writes: %d, %v", want, v, err)
		}
	}
	last := f.Requests()[len(f.Requests())-1]
	if last.Method != http.MethodGet || last.URL != "/qm/v1/secret/metadata/sanholo/tenants/t1/game" {
		t.Fatalf("version request = %+v", last)
	}
	// A destroyed current version holds no value: reported as absent.
	f.SetOverride(func(r qmfake.Request) (int, string, bool) {
		return 200, `{"current_version":2,"oldest_version":1,"versions":{"1":{"destroyed":false},"2":{"destroyed":true}}}`, true
	})
	if v, err := c.Version(ctx, "t1"); err != nil || v != 0 {
		t.Fatalf("destroyed current: %d, %v", v, err)
	}
	// Metadata is all this package ever reads.
	for _, r := range f.Requests() {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL, "/qm/v1/secret/data/") {
			t.Fatalf("client read a value: %+v", r)
		}
	}
}

// A rotation is a plain new write: no destroy, no delete, no purge, and qm
// prunes to 16 versions on its own.
func TestRotationIsAPlainWrite(t *testing.T) {
	c, f, _ := setup(t)
	ctx := context.Background()
	path := "sanholo/tenants/t1/game"
	for i := range 20 {
		next := secrets.Credentials{APIToken: fmt.Sprintf("TOKEN-%02d", i), ReplicantCode: gameCode}
		if err := c.Put(ctx, "t1", next); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := f.Stored(path); got["api_token"] != "TOKEN-19" {
		t.Fatalf("stored = %v", got)
	}
	if f.Versions(path) != 20 || f.Live(path) != 16 {
		t.Fatalf("versions = %d, retained = %d; want 20 and 16", f.Versions(path), f.Live(path))
	}
	for _, r := range f.Requests() {
		if r.Method != http.MethodPost || !strings.HasPrefix(r.URL, "/qm/v1/secret/data/") {
			t.Fatalf("a write sent %s %s", r.Method, r.URL)
		}
	}
}

func TestPurgeTenant(t *testing.T) {
	c, f, _ := setup(t)
	ctx := context.Background()
	// Neighbours whose ids share a leading string must survive.
	for _, id := range []string{"abc", "abc-d", "ab"} {
		if err := c.Put(ctx, id, creds); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Put(ctx, "abc", creds); err != nil { // a second version
		t.Fatal(err)
	}
	f.Seed("sanholo/tenants/abc/other/nested", map[string]string{"k": "v"})
	before := len(f.Requests())
	if err := c.PurgeTenant(ctx, "abc"); err != nil {
		t.Fatal(err)
	}
	if n := f.Live("sanholo/tenants/abc/"); n != 0 {
		t.Fatalf("%d versions remain under the prefix", n)
	}
	for _, id := range []string{"abc-d", "ab"} {
		if n := f.Live("sanholo/tenants/" + id + "/"); n != 1 {
			t.Errorf("%s has %d versions after purging abc, want 1", id, n)
		}
	}
	// One purge request on the tenant prefix, then a metadata check of the
	// game path.
	var seq []string
	for _, r := range f.Requests()[before:] {
		seq = append(seq, r.Method+" "+r.URL)
	}
	want := "[DELETE /qm/v1/secret/purge/sanholo/tenants/abc GET /qm/v1/secret/metadata/sanholo/tenants/abc/game]"
	if fmt.Sprint(seq) != want {
		t.Fatalf("requests = %v\nwant       %s", seq, want)
	}
	// Repeatable (qm says 404 for nothing to purge), and fine for a tenant
	// with nothing stored.
	if err := c.PurgeTenant(ctx, "abc"); err != nil {
		t.Fatalf("second purge: %v", err)
	}
	if err := c.PurgeTenant(ctx, "never-existed"); err != nil {
		t.Fatalf("purge of unknown tenant: %v", err)
	}
	if v, err := c.Version(ctx, "abc"); err != nil || v != 0 {
		t.Fatalf("version after purge = %d, %v", v, err)
	}
}

func TestPurgeAcceptsOKAndNoContent(t *testing.T) {
	for _, status := range []int{200, 204} {
		c, f, _ := setup(t)
		f.SetOverride(func(r qmfake.Request) (int, string, bool) {
			if r.Method == http.MethodDelete {
				return status, "", true
			}
			return 0, "", false
		})
		if err := c.PurgeTenant(context.Background(), "t1"); err != nil {
			t.Errorf("status %d: %v", status, err)
		}
	}
}

func TestPurgeFailsLoudlyIfSecretRemains(t *testing.T) {
	c, f, _ := setup(t)
	ctx := context.Background()
	if err := c.Put(ctx, "t1", creds); err != nil {
		t.Fatal(err)
	}
	// A qm that acknowledges the purge but keeps the data.
	f.SetOverride(func(r qmfake.Request) (int, string, bool) {
		if r.Method == http.MethodDelete {
			return http.StatusNoContent, "", true
		}
		return 0, "", false
	})
	if err := c.PurgeTenant(ctx, "t1"); err == nil || !errors.Is(err, secrets.ErrProtocol) {
		t.Fatalf("err = %v, want ErrProtocol", err)
	}
}

// Before Part E ships the provisioner has no delete grant: the purge is
// refused and says so, and nothing is removed.
func TestPurgeBeforePartEIsDenied(t *testing.T) {
	c, f, _ := setup(t)
	ctx := context.Background()
	if err := c.Put(ctx, "t1", creds); err != nil {
		t.Fatal(err)
	}
	f.SetDeleteAllowed(false)
	err := c.PurgeTenant(ctx, "t1")
	if !errors.Is(err, secrets.ErrDenied) {
		t.Fatalf("err = %v, want ErrDenied", err)
	}
	if _, ok := f.Stored("sanholo/tenants/t1/game"); !ok {
		t.Fatal("the secret was removed")
	}
}

func TestWriteOnlyPolicy(t *testing.T) {
	c, f, _ := setup(t)
	if err := c.Put(context.Background(), "t1", creds); err != nil {
		t.Fatal(err)
	}
	// The fake enforces the principal's policy: even with the right token a
	// read of a value, a list and a destroy are refused. The client has no
	// way to ask for any of them.
	do := func(method, path string) (int, string) {
		req, _ := http.NewRequest(method, f.URL()+path, strings.NewReader(`{"versions":[1]}`))
		req.Header.Set("X-QM-Token", cpToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	for name, tc := range map[string][2]string{
		"read data": {"GET", "/qm/v1/secret/data/sanholo/tenants/t1/game"},
		"list":      {"GET", "/qm/v1/secret/metadata/sanholo/tenants/t1/?list=true"},
		"destroy":   {"POST", "/qm/v1/secret/destroy/sanholo/tenants/t1/game"},
	} {
		status, body := do(tc[0], tc[1])
		if status != http.StatusForbidden || strings.Contains(body, gameToken) || !strings.Contains(body, "forbidden") {
			t.Errorf("%s: %d %s", name, status, body)
		}
	}
	if status, _ := do("GET", "/qm/v1/secret/metadata/sanholo/tenants/t1/game"); status != 200 {
		t.Errorf("metadata read: %d", status)
	}
	// No token, a wrong token: 403, never 401.
	for _, tok := range []string{"", "wrong"} {
		req, _ := http.NewRequest("GET", f.URL()+"/qm/v1/secret/metadata/sanholo/tenants/t1/game", nil)
		if tok != "" {
			req.Header.Set("X-QM-Token", tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("token %q: %d, want 403", tok, resp.StatusCode)
		}
	}
}

func TestErrorMapping(t *testing.T) {
	sealed := `{"error":{"code":"vault_sealed","message":"x"}}`
	cases := []struct {
		name   string
		status int
		body   string
		want   error
		not    error
	}{
		{"403", 403, `{"error":{"code":"forbidden"}}`, secrets.ErrDenied, secrets.ErrUnavailable},
		{"401 is not a qm answer", 401, `{}`, secrets.ErrProtocol, secrets.ErrDenied},
		{"429", 429, `{}`, secrets.ErrUnavailable, secrets.ErrSealed},
		{"500", 500, `{}`, secrets.ErrUnavailable, secrets.ErrSealed},
		{"503 plain", 503, `{}`, secrets.ErrUnavailable, secrets.ErrSealed},
		{"503 sealed", 503, sealed, secrets.ErrSealed, secrets.ErrDenied},
		{"503 sealed, flat code", 503, `{"code":"vault_sealed"}`, secrets.ErrSealed, secrets.ErrDenied},
		{"503 sealed, string error", 503, `{"error":"vault_sealed"}`, secrets.ErrSealed, secrets.ErrDenied},
		{"503 other code", 503, `{"error":{"code":"busy"}}`, secrets.ErrUnavailable, secrets.ErrSealed},
		{"400", 400, `{}`, secrets.ErrProtocol, secrets.ErrUnavailable},
		{"409", 409, `{}`, secrets.ErrProtocol, secrets.ErrUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, f, _ := setup(t)
			f.SetOverride(func(r qmfake.Request) (int, string, bool) { return tc.status, tc.body, true })
			err := c.Put(context.Background(), "t1", creds)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if errors.Is(err, tc.not) {
				t.Fatalf("err = %v must not be %v", err, tc.not)
			}
			var e *secrets.Error
			if !errors.As(err, &e) || e.Status != tc.status || e.Op != "put" {
				t.Fatalf("error = %#v", err)
			}
		})
	}
	// A sealed qm is an unavailable one for callers that only know that.
	if !errors.Is(secrets.ErrSealed, secrets.ErrUnavailable) {
		t.Fatal("ErrSealed is not an ErrUnavailable")
	}
	t.Run("body not echoed", func(t *testing.T) {
		c, f, _ := setup(t)
		f.SetOverride(func(r qmfake.Request) (int, string, bool) {
			return 500, `{"errors":["` + gameToken + `"]}`, true
		})
		if err := c.Put(context.Background(), "t1", creds); err == nil || strings.Contains(err.Error(), gameToken) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestSealed(t *testing.T) {
	c, f, _ := setup(t)
	ctx := context.Background()
	f.SetSealed(true)
	for op, err := range map[string]error{
		"put": c.Put(ctx, "t1", creds), "purge": c.PurgeTenant(ctx, "t1"), "ready": c.Ready(ctx),
	} {
		if !errors.Is(err, secrets.ErrSealed) || !errors.Is(err, secrets.ErrUnavailable) {
			t.Errorf("%s: err = %v, want ErrSealed (and ErrUnavailable)", op, err)
		}
	}
	_, err := c.Version(ctx, "t1")
	if !errors.Is(err, secrets.ErrSealed) {
		t.Errorf("version: %v", err)
	}
	f.SetSealed(false)
	if err := c.Put(ctx, "t1", creds); err != nil {
		t.Fatalf("after unseal: %v", err)
	}
}

func TestUnreachableAndTimeout(t *testing.T) {
	c, f, _ := setup(t)
	f.Close()
	if err := c.Put(context.Background(), "t1", creds); !errors.Is(err, secrets.ErrUnavailable) {
		t.Fatalf("closed server: %v", err)
	} else if strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("error quotes the address: %v", err)
	}

	f2 := qmfake.New(t, cpToken)
	slow, err := secrets.New(secrets.Config{Addr: f2.URL(), Token: cpToken, Timeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	f2.SetOverride(func(qmfake.Request) (int, string, bool) {
		<-release
		return 200, "{}", true
	})
	start := time.Now()
	if err := slow.Put(context.Background(), "t1", creds); !errors.Is(err, secrets.ErrUnavailable) {
		t.Fatalf("slow server: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("returned after %v, timeout not applied", d)
	}
}

func TestHostileResponses(t *testing.T) {
	ctx := context.Background()
	t.Run("oversize", func(t *testing.T) {
		c, f, _ := setup(t)
		f.SetOverride(func(qmfake.Request) (int, string, bool) {
			return 200, `{"version":1,"pad":"` + strings.Repeat("x", 2<<20) + `"}`, true
		})
		if err := c.Put(ctx, "t1", creds); !errors.Is(err, secrets.ErrProtocol) {
			t.Fatalf("err = %v, want ErrProtocol", err)
		}
	})
	for name, body := range map[string]string{
		"junk": `not json`, "no version": `{}`, "zero version": `{"version":0}`,
		"old data envelope": `{"data":{"version":1}}`,
	} {
		t.Run("put reply "+name, func(t *testing.T) {
			c, f, _ := setup(t)
			f.SetOverride(func(qmfake.Request) (int, string, bool) { return 200, body, true })
			if err := c.Put(ctx, "t1", creds); !errors.Is(err, secrets.ErrProtocol) {
				t.Fatalf("err = %v, want ErrProtocol", err)
			}
		})
	}
	for name, body := range map[string]string{
		"junk": `not json`, "no current": `{"versions":{}}`, "old data envelope": `{"data":{"current_version":1}}`,
	} {
		t.Run("metadata reply "+name, func(t *testing.T) {
			c, f, _ := setup(t)
			f.SetOverride(func(qmfake.Request) (int, string, bool) { return 200, body, true })
			if _, err := c.Version(ctx, "t1"); !errors.Is(err, secrets.ErrProtocol) {
				t.Fatalf("err = %v, want ErrProtocol", err)
			}
		})
	}
	t.Run("redirect not followed", func(t *testing.T) {
		other := qmfake.New(t, cpToken)
		redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, other.URL()+r.URL.Path, http.StatusTemporaryRedirect)
		}))
		t.Cleanup(redirector.Close)
		c, err := secrets.New(secrets.Config{Addr: redirector.URL, Token: cpToken})
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put(ctx, "t1", creds); err == nil {
			t.Fatal("3xx accepted")
		}
		if n := len(other.Requests()); n != 0 {
			t.Fatal("followed a redirect with the token")
		}
	})
	t.Run("redirect not followed on login", func(t *testing.T) {
		other := qmfake.New(t, cpToken)
		other.AddAppRole(roleID, secretID)
		redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, other.URL()+r.URL.Path, http.StatusTemporaryRedirect)
		}))
		t.Cleanup(redirector.Close)
		c, err := secrets.New(secrets.Config{Addr: redirector.URL, RoleID: roleID, SecretID: secretID})
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put(ctx, "t1", creds); err == nil {
			t.Fatal("3xx accepted")
		}
		if n := len(other.Requests()); n != 0 {
			t.Fatal("followed a redirect with the credentials")
		}
	})
}

func TestReady(t *testing.T) {
	c, f, _ := setup(t)
	ctx := context.Background()
	if err := c.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	if r := f.Requests()[0]; r.Method != "GET" || r.URL != "/qm/v1/sys/health" || r.Header.Get("X-QM-Token") != "" {
		t.Fatalf("ready request = %+v", r)
	}
	f.SetSealed(true)
	if err := c.Ready(ctx); !errors.Is(err, secrets.ErrSealed) {
		t.Fatalf("sealed: %v", err)
	}
	f.SetSealed(false)
	f.SetInitialized(false)
	if err := c.Ready(ctx); !errors.Is(err, secrets.ErrUnavailable) {
		t.Fatalf("uninitialized: %v", err)
	}
	f.SetInitialized(true)
	for name, body := range map[string]string{"junk": "x", "empty": "{}", "one flag": `{"sealed":false}`} {
		f.SetOverride(func(qmfake.Request) (int, string, bool) { return 200, body, true })
		if err := c.Ready(ctx); !errors.Is(err, secrets.ErrProtocol) {
			t.Errorf("%s: %v, want ErrProtocol", name, err)
		}
	}
}

func TestWrongStaticToken(t *testing.T) {
	f := qmfake.New(t, cpToken)
	c, _ := secrets.New(secrets.Config{Addr: f.URL(), Token: "wrong"})
	if err := c.Put(context.Background(), "t1", creds); !errors.Is(err, secrets.ErrDenied) {
		t.Fatalf("err = %v", err)
	}
	// A static token has no login to retry: exactly one attempt, no login.
	if n := f.Count("POST", "/qm/v1/secret/data/"); n != 1 {
		t.Fatalf("%d attempts", n)
	}
	if n := f.Count("POST", "/qm/v1/auth/"); n != 0 {
		t.Fatalf("a static-token client logged in %d times", n)
	}
}

func TestTokenFileIsReRead(t *testing.T) {
	f := qmfake.New(t, cpToken)
	path := filepath.Join(t.TempDir(), "qm-token")
	if err := os.WriteFile(path, []byte("stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := secrets.New(secrets.Config{Addr: f.URL(), TokenFile: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Put(context.Background(), "t1", creds); !errors.Is(err, secrets.ErrDenied) {
		t.Fatalf("stale token: %v", err)
	}
	if err := os.WriteFile(path, []byte(cpToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.Put(context.Background(), "t1", creds); err != nil {
		t.Fatalf("renewed token: %v", err)
	}
	if err := os.WriteFile(path, []byte("a\nb"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = c.Put(context.Background(), "t1", creds)
	if err == nil || strings.Contains(err.Error(), "a\nb") {
		t.Fatalf("bad token file: %v", err)
	}
}

func TestParseAddr(t *testing.T) {
	got, err := secrets.ParseAddr("https://qm.example.org/")
	if err != nil || got != "https://qm.example.org" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := secrets.ParseAddr("http://qm.example.org"); err == nil {
		t.Fatal("http to a real host accepted")
	}
}

// ---- AppRole ----

func TestAppRoleLoginOnFirstUseOnly(t *testing.T) {
	c, f, _, _ := setupRole(t)
	ctx := context.Background()
	if f.Count("POST", "/qm/v1/auth/approle/login") != 0 {
		t.Fatal("New logged in; login should wait for the first request")
	}
	for range 3 {
		if err := c.Put(ctx, "t1", creds); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.Count("POST", "/qm/v1/auth/approle/login"); n != 1 {
		t.Fatalf("%d logins for three writes, want 1", n)
	}
	reqs := f.Requests()
	if reqs[0].Body != `{"role_id":"`+roleID+`","secret_id":"`+secretID+`"}` || reqs[0].Header.Get("X-QM-Token") != "" {
		t.Fatalf("login request = %+v", reqs[0])
	}
	tok := f.LastIssuedToken()
	for _, r := range reqs[1:] {
		if r.Header.Get("X-QM-Token") != tok {
			t.Fatalf("%s %s used token %q, want the issued one", r.Method, r.URL, r.Header.Get("X-QM-Token"))
		}
	}
	// Ready needs no login.
	c2, f2, _, _ := setupRole(t)
	if err := c2.Ready(ctx); err != nil || len(f2.Requests()) != 1 {
		t.Fatalf("ready: %v, %d requests", err, len(f2.Requests()))
	}
}

func TestAppRoleRenewsPastTwoThirdsOfTheLease(t *testing.T) {
	c, f, clk, _ := setupRole(t) // lease 30s: renew from 20s on
	ctx := context.Background()
	if err := c.Put(ctx, "t1", creds); err != nil {
		t.Fatal(err)
	}
	tok := f.LastIssuedToken()
	clk.Advance(19 * time.Second)
	if err := c.Put(ctx, "t1", creds); err != nil {
		t.Fatal(err)
	}
	if n := f.Count("POST", "/qm/v1/auth/token/renew-self"); n != 0 {
		t.Fatalf("renewed after 19s of a 30s lease (%d)", n)
	}
	clk.Advance(2 * time.Second) // 21s
	if err := c.Put(ctx, "t1", creds); err != nil {
		t.Fatal(err)
	}
	if n := f.Count("POST", "/qm/v1/auth/token/renew-self"); n != 1 {
		t.Fatalf("%d renewals after 21s, want 1", n)
	}
	for _, r := range f.Requests() {
		if strings.HasPrefix(r.URL, "/qm/v1/auth/token/renew-self") && r.Header.Get("X-QM-Token") != tok {
			t.Fatalf("renew used token %q", r.Header.Get("X-QM-Token"))
		}
	}
	if n := f.Count("POST", "/qm/v1/auth/approle/login"); n != 1 {
		t.Fatalf("%d logins, a successful renewal needs no new one", n)
	}
	// The renewal restarted the clock: no second renewal 10s later.
	clk.Advance(10 * time.Second)
	if err := c.Put(ctx, "t1", creds); err != nil {
		t.Fatal(err)
	}
	if n := f.Count("POST", "/qm/v1/auth/token/renew-self"); n != 1 {
		t.Fatalf("%d renewals, want still 1", n)
	}
}

func TestAppRoleRenewFailureLogsInAgain(t *testing.T) {
	c, f, clk, _ := setupRole(t)
	ctx := context.Background()
	if err := c.Put(ctx, "t1", creds); err != nil {
		t.Fatal(err)
	}
	first := f.LastIssuedToken()
	f.SetRenewFails(true)
	clk.Advance(25 * time.Second)
	if err := c.Put(ctx, "t1", creds); err != nil {
		t.Fatal(err)
	}
	if n := f.Count("POST", "/qm/v1/auth/approle/login"); n != 2 {
		t.Fatalf("%d logins, want 2", n)
	}
	if f.LastIssuedToken() == first {
		t.Fatal("the token was not replaced")
	}
	last := f.Requests()[len(f.Requests())-1]
	if last.Header.Get("X-QM-Token") != f.LastIssuedToken() {
		t.Fatal("the write did not use the new token")
	}
}

func TestAppRoleExpiredTokenRetriesOnceAfterLogin(t *testing.T) {
	c, f, _, _ := setupRole(t)
	ctx := context.Background()
	if err := c.Put(ctx, "t1", creds); err != nil {
		t.Fatal(err)
	}
	f.ExpireTokens()
	before := f.Count("POST", "/qm/v1/secret/data/")
	if err := c.Put(ctx, "t1", creds); err != nil {
		t.Fatalf("expired token: %v", err)
	}
	if n := f.Count("POST", "/qm/v1/auth/approle/login"); n != 2 {
		t.Fatalf("%d logins, want 2", n)
	}
	if n := f.Count("POST", "/qm/v1/secret/data/") - before; n != 2 {
		t.Fatalf("%d write attempts, want 2 (rejected, then retried)", n)
	}
	if f.Versions("sanholo/tenants/t1/game") != 2 {
		t.Fatalf("versions = %d, the retry must write exactly once", f.Versions("sanholo/tenants/t1/game"))
	}
}

func TestAppRoleSecond403IsDeniedAfterOneRetry(t *testing.T) {
	c, f, _, _ := setupRole(t)
	f.SetOverride(func(r qmfake.Request) (int, string, bool) {
		if strings.HasPrefix(r.URL, "/qm/v1/secret/data/") {
			return 403, `{"error":{"code":"forbidden"}}`, true
		}
		return 0, "", false
	})
	err := c.Put(context.Background(), "t1", creds)
	if !errors.Is(err, secrets.ErrDenied) {
		t.Fatalf("err = %v, want ErrDenied", err)
	}
	if n := f.Count("POST", "/qm/v1/secret/data/"); n != 2 {
		t.Fatalf("%d write attempts, want exactly 2", n)
	}
	if n := f.Count("POST", "/qm/v1/auth/approle/login"); n != 2 {
		t.Fatalf("%d logins, want exactly 2 (first use, one re-login)", n)
	}
}

func TestAppRoleBadCredentialsAreDenied(t *testing.T) {
	f := qmfake.New(t, cpToken)
	f.AddAppRole(roleID, secretID)
	c, err := secrets.New(secrets.Config{Addr: f.URL(), RoleID: roleID, SecretID: "wrong-secret-id"})
	if err != nil {
		t.Fatal(err)
	}
	err = c.Put(context.Background(), "t1", creds)
	if !errors.Is(err, secrets.ErrDenied) {
		t.Fatalf("err = %v, want ErrDenied", err)
	}
	if strings.Contains(err.Error(), "wrong-secret-id") || strings.Contains(err.Error(), roleID) {
		t.Fatalf("error echoes credentials: %v", err)
	}
	if n := f.Count("POST", "/qm/v1/secret/data/"); n != 0 {
		t.Fatalf("a write was sent without a token (%d)", n)
	}
	if n := f.Count("POST", "/qm/v1/auth/approle/login"); n != 1 {
		t.Fatalf("%d logins for one rejected request, want 1", n)
	}
}

func TestAppRoleLoginWhileSealedAndMalformed(t *testing.T) {
	c, f, _, _ := setupRole(t)
	ctx := context.Background()
	f.SetSealed(true)
	if err := c.Put(ctx, "t1", creds); !errors.Is(err, secrets.ErrSealed) {
		t.Fatalf("sealed login: %v", err)
	}
	f.SetSealed(false)
	for name, body := range map[string]string{
		"no token": `{"auth":{"lease_duration":30}}`, "junk": `x`, "flat": `{"client_token":"abc"}`,
		"newline token": `{"auth":{"client_token":"a\nb","lease_duration":30}}`,
	} {
		f.SetOverride(func(r qmfake.Request) (int, string, bool) {
			if strings.HasPrefix(r.URL, "/qm/v1/auth/approle/login") {
				return 200, body, true
			}
			return 0, "", false
		})
		if err := c.Put(ctx, "t1", creds); !errors.Is(err, secrets.ErrProtocol) {
			t.Errorf("%s: err = %v, want ErrProtocol", name, err)
		}
	}
	if n := f.Count("POST", "/qm/v1/secret/data/"); n != 0 {
		t.Fatalf("%d writes without a valid token", n)
	}
}

func TestAppRoleMissingLeaseDefaultsToFifteenMinutes(t *testing.T) {
	c, f, clk, _ := setupRole(t)
	ctx := context.Background()
	f.SetOverride(func(r qmfake.Request) (int, string, bool) {
		if strings.HasPrefix(r.URL, "/qm/v1/auth/approle/login") {
			return 200, `{"auth":{"client_token":"qmc-nolease"}}`, true
		}
		if strings.HasPrefix(r.URL, "/qm/v1/auth/token/renew-self") {
			return 200, `{"auth":{}}`, true
		}
		if strings.HasPrefix(r.URL, "/qm/v1/secret/data/") {
			return 200, `{"version":1}`, true
		}
		return 0, "", false
	})
	renews := func() int { return f.Count("POST", "/qm/v1/auth/token/renew-self") }
	if err := c.Put(ctx, "t1", creds); err != nil {
		t.Fatal(err)
	}
	clk.Advance(9 * time.Minute)
	_ = c.Put(ctx, "t1", creds)
	if renews() != 0 {
		t.Fatal("renewed before 10 minutes of the 15 minute default")
	}
	clk.Advance(2 * time.Minute)
	_ = c.Put(ctx, "t1", creds)
	if renews() != 1 {
		t.Fatalf("%d renewals after 11 minutes, want 1", renews())
	}
}

func TestAppRoleSecretIDFileIsReReadOnLogin(t *testing.T) {
	f := qmfake.New(t, cpToken)
	f.AddAppRole(roleID, secretID)
	path := filepath.Join(t.TempDir(), "secret-id")
	if err := os.WriteFile(path, []byte("stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := secrets.New(secrets.Config{Addr: f.URL(), RoleID: roleID, SecretIDFile: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Put(context.Background(), "t1", creds); !errors.Is(err, secrets.ErrDenied) {
		t.Fatalf("stale secret id: %v", err)
	}
	if err := os.WriteFile(path, []byte(secretID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.Put(context.Background(), "t1", creds); err != nil {
		t.Fatalf("fresh secret id: %v", err)
	}
}

func TestAppRoleConcurrentExpiryLogsInOnce(t *testing.T) {
	c, f, _, _ := setupRole(t)
	ctx := context.Background()
	if err := c.Put(ctx, "t1", creds); err != nil {
		t.Fatal(err)
	}
	f.ExpireTokens()
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- c.Put(ctx, "t1", creds)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := f.Count("POST", "/qm/v1/auth/approle/login"); n != 2 {
		t.Fatalf("%d logins for 20 concurrent requests on an expired token, want 2 in total", n)
	}
}

// ---- Leaks ----

// The game token appears only in the body of the data write; the AppRole
// identity only in the login body; the client token only in X-QM-Token; and
// none of them in logs or errors.
func TestSecretsNeverLeak(t *testing.T) {
	c, f, clk, logs := setupRole(t)
	ctx := context.Background()
	_ = c.Put(ctx, "t1", creds)
	_, _ = c.Version(ctx, "t1")
	_ = c.Ready(ctx)
	_ = c.PurgeTenant(ctx, "t1")
	_ = c.Put(ctx, "../x", creds)
	f.ExpireTokens()
	_ = c.Put(ctx, "t2", creds) // 403, re-login, retry
	f.SetRenewFails(true)
	clk.Advance(time.Minute)
	_ = c.Put(ctx, "t2", creds) // failed renewal, login again
	// Failures that echo the request back.
	f.SetOverride(func(r qmfake.Request) (int, string, bool) {
		return 500, `{"echo":"` + r.Body + `"}`, true
	})
	errs := []error{c.Put(ctx, "t3", creds)}
	f.SetOverride(func(r qmfake.Request) (int, string, bool) {
		return 403, `{"echo":"` + r.Body + `","t":"` + r.Header.Get("X-QM-Token") + `"}`, true
	})
	errs = append(errs, c.Put(ctx, "t3", creds), c.Ready(ctx))
	f.SetOverride(nil)
	f.SetSealed(true)
	errs = append(errs, c.Put(ctx, "t4", creds))
	f.SetSealed(false)

	clientTokens := map[string]bool{}
	for _, r := range f.Requests() {
		if v := r.Header.Get("X-QM-Token"); v != "" {
			clientTokens[v] = true
		}
	}
	if len(clientTokens) < 3 {
		t.Fatalf("only %d client tokens seen; the scenario did not exercise re-login", len(clientTokens))
	}
	isLogin := func(r qmfake.Request) bool { return r.Method == "POST" && r.URL == "/qm/v1/auth/approle/login" }
	isData := func(r qmfake.Request) bool {
		return r.Method == "POST" && strings.HasPrefix(r.URL, "/qm/v1/secret/data/")
	}
	for _, r := range f.Requests() {
		where := r.Method + " " + r.URL
		headers := fmt.Sprint(r.Header)
		for _, id := range []string{roleID, secretID} {
			if strings.Contains(r.URL, id) || strings.Contains(headers, id) || (!isLogin(r) && strings.Contains(r.Body, id)) {
				t.Errorf("AppRole identity outside the login body: %s", where)
			}
		}
		if strings.Contains(r.URL, gameToken) || strings.Contains(headers, gameToken) || (!isData(r) && strings.Contains(r.Body, gameToken)) {
			t.Errorf("game token outside the data body: %s", where)
		}
		for tok := range clientTokens {
			if strings.Contains(r.URL, tok) || strings.Contains(r.Body, tok) {
				t.Errorf("client token outside X-QM-Token: %s", where)
			}
			for name, vs := range r.Header {
				if name != "X-Qm-Token" && strings.Contains(strings.Join(vs, ","), tok) {
					t.Errorf("client token in header %s: %s", name, where)
				}
			}
		}
		if isLogin(r) && strings.Contains(r.Body, gameToken) {
			t.Errorf("game token in a login body")
		}
	}
	secretsList := []string{gameToken, roleID, secretID, cpToken}
	for tok := range clientTokens {
		secretsList = append(secretsList, tok)
	}
	s := logs.String()
	if s == "" {
		t.Fatal("expected some log output to scan")
	}
	for _, sec := range secretsList {
		if strings.Contains(s, sec) {
			t.Fatalf("log contains %q:\n%s", sec, s)
		}
		for _, err := range errs {
			if err == nil {
				t.Fatal("a failing call returned nil")
			}
			if strings.Contains(err.Error(), sec) {
				t.Fatalf("error contains %q: %v", sec, err)
			}
		}
	}
}

func TestTokenOnlyInPutBodyAndNeverLoggedStaticMode(t *testing.T) {
	c, f, logs := setup(t)
	ctx := context.Background()
	_ = c.Put(ctx, "t1", creds)
	_, _ = c.Version(ctx, "t1")
	_ = c.PurgeTenant(ctx, "t1")
	_ = c.Put(ctx, "../x", creds)
	f.SetOverride(func(r qmfake.Request) (int, string, bool) {
		return 500, `{"echo":"` + r.Body + `"}`, true
	})
	err := c.Put(ctx, "t2", creds)
	f.SetOverride(nil)
	for _, r := range f.Requests() {
		if strings.Contains(r.URL, gameToken) || strings.Contains(fmt.Sprint(r.Header), gameToken) {
			t.Errorf("token outside the body of %s %s", r.Method, r.URL)
		}
		if strings.Contains(r.Body, gameToken) && !(r.Method == "POST" && strings.HasPrefix(r.URL, "/qm/v1/secret/data/")) {
			t.Errorf("token in the body of %s %s", r.Method, r.URL)
		}
	}
	if s := logs.String(); s == "" || strings.Contains(s, gameToken) || strings.Contains(s, cpToken) {
		t.Fatalf("log empty or contains a secret:\n%s", s)
	}
	if err == nil || strings.Contains(err.Error(), gameToken) || strings.Contains(err.Error(), cpToken) {
		t.Fatalf("error echoes a secret or is nil: %v", err)
	}
}

// ---- Fake: Part E and the surface ----

func TestFakeImplementsPartE(t *testing.T) {
	f := qmfake.New(t, cpToken)
	f.Seed("sanholo/tenants/a/game", map[string]string{"k": "v"})
	f.Seed("sanholo/tenants/a/other", map[string]string{"k": "v"})
	f.Seed("sanholo/tenants/ab/game", map[string]string{"k": "v"})
	del := func(path string) int {
		req, _ := http.NewRequest("DELETE", f.URL()+path, nil)
		req.Header.Set("X-QM-Token", cpToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if s := del("/qm/v1/secret/metadata/sanholo/tenants/a/other"); s != 204 || f.Live("sanholo/tenants/a/") != 1 {
		t.Fatalf("delete one path: %d, %d left", s, f.Live("sanholo/tenants/a/"))
	}
	if s := del("/qm/v1/secret/purge/sanholo/tenants/a"); s != 204 || f.Live("sanholo/tenants/a/") != 0 || f.Live("sanholo/tenants/ab/") != 1 {
		t.Fatalf("purge: %d", s)
	}
	f.SetDeleteAllowed(false)
	if s := del("/qm/v1/secret/purge/sanholo/tenants/ab"); s != 403 || f.Live("sanholo/tenants/ab/") != 1 {
		t.Fatalf("interim policy: %d", s)
	}
}

// TestAPISurface is the structural guarantee that this package cannot hand a
// secret value back: every exported function and method is on an allow-list
// with its exact result types, which are only errors, booleans, a version
// number and strings that are names or messages. There is no list and no
// read. Adding an exported function, or changing a result type, fails here
// until a human decides it is safe.
func TestAPISurface(t *testing.T) {
	want := map[string]string{
		"New":                "(*Client, error)",
		"ParseAddr":          "(string, error)",
		"Client.Enabled":     "bool",
		"Client.Ready":       "error",
		"Client.Put":         "error",
		"Client.Version":     "(int, error)",
		"Client.PurgeTenant": "error",
		"Error.Error":        "string",
		"Error.Unwrap":       "error",
	}
	got := exportedSignatures(t)
	for name, res := range got {
		w, ok := want[name]
		if !ok {
			t.Errorf("exported %s %s is not on the allow-list", name, res)
			continue
		}
		if w != res {
			t.Errorf("%s returns %s, allow-list says %s", name, res, w)
		}
	}
	for name := range want {
		if _, ok := got[name]; !ok {
			t.Errorf("allow-listed %s no longer exists", name)
		}
	}

	// Reflection agrees with the source: no method on *Client returns
	// anything but error, bool or int.
	ct := reflect.TypeFor[*secrets.Client]()
	okOut := map[reflect.Type]bool{
		reflect.TypeFor[error](): true, reflect.TypeFor[bool](): true, reflect.TypeFor[int](): true,
	}
	for i := range ct.NumMethod() {
		m := ct.Method(i)
		for j := range m.Type.NumOut() {
			if !okOut[m.Type.Out(j)] {
				t.Errorf("method %s returns %v", m.Name, m.Type.Out(j))
			}
		}
	}
}
