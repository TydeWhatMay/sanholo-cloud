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
	"sync/atomic"
	"testing"
	"time"

	"github.com/lhaig/sanholo-cloud/internal/secrets"
	"github.com/lhaig/sanholo-cloud/internal/secrets/qmfake"
)

const (
	cpToken   = "cp-client-token-7f3a"
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

func TestNewValidation(t *testing.T) {
	cases := []struct {
		name    string
		cfg     secrets.Config
		wantErr string
	}{
		{"disabled", secrets.Config{}, ""},
		{"https", secrets.Config{Addr: "https://qm.example.org", Token: "t"}, ""},
		{"http loopback", secrets.Config{Addr: "http://127.0.0.1:8222", Token: "t"}, ""},
		{"http localhost", secrets.Config{Addr: "http://localhost:8222", Token: "t"}, ""},
		{"http ipv6 loopback", secrets.Config{Addr: "http://[::1]:8222", Token: "t"}, ""},
		{"http real host", secrets.Config{Addr: "http://qm.example.org", Token: "t"}, "loopback"},
		{"http lookalike", secrets.Config{Addr: "http://localhost.example.org", Token: "t"}, "loopback"},
		{"ftp", secrets.Config{Addr: "ftp://qm.example.org", Token: "t"}, "https"},
		{"path", secrets.Config{Addr: "https://qm.example.org/x", Token: "t"}, "origin"},
		{"userinfo", secrets.Config{Addr: "https://u:p@qm.example.org", Token: "t"}, "origin"},
		{"no token", secrets.Config{Addr: "https://qm.example.org"}, "exactly one"},
		{"both tokens", secrets.Config{Addr: "https://qm.example.org", Token: "t", TokenFile: "/x"}, "exactly one"},
		{"token without addr", secrets.Config{Token: "t"}, "no qm address"},
		{"newline in token", secrets.Config{Addr: "https://qm.example.org", Token: "a\nb"}, "header"},
		{"missing token file", secrets.Config{Addr: "https://qm.example.org", TokenFile: "/nonexistent/qm-token"}, "token file"},
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
			if tc.cfg.Token != "" && strings.Contains(err.Error(), tc.cfg.Token) && len(tc.cfg.Token) > 1 {
				t.Fatalf("error echoes the token: %v", err)
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
			"put":    cl.Put(ctx, "abc", creds),
			"purge":  cl.PurgeTenant(ctx, "abc"),
			"rotate": cl.Rotate(ctx, "abc", creds),
			"ready":  cl.Ready(ctx),
		}
		_, checks["list"] = cl.List(ctx, "abc")
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
		_, listErr := c.List(ctx, id)
		for op, err := range map[string]error{
			"put": c.Put(ctx, id, creds), "list": listErr,
			"purge": c.PurgeTenant(ctx, id), "rotate": c.Rotate(ctx, id, creds),
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
	if r.Method != http.MethodPost || r.URL != "/qm/v1/secret/data/sanholo/tenants/t1/game" || r.Header.Get("X-Qm-Token") != cpToken {
		t.Fatalf("request = %+v", r)
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

func TestListReturnsNamesOnly(t *testing.T) {
	c, f, _ := setup(t)
	ctx := context.Background()
	if names, err := c.List(ctx, "t1"); err != nil || len(names) != 0 {
		t.Fatalf("empty: %v %v", names, err)
	}
	f.Seed("sanholo/tenants/t1/extra/deep", map[string]string{"k": "v"})
	if err := c.Put(ctx, "t1", creds); err != nil {
		t.Fatal(err)
	}
	names, err := c.List(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(names) != "[extra/deep game]" {
		t.Fatalf("names = %v", names)
	}
	for _, n := range names {
		if strings.Contains(n, gameToken) {
			t.Fatal("name carries a value")
		}
	}
	// A GET of a data path never happens.
	for _, r := range f.Requests() {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL, "/qm/v1/secret/data/") {
			t.Fatalf("client read a value: %+v", r)
		}
	}
}

func TestPurgeTenantLeavesNothing(t *testing.T) {
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
	if err := c.PurgeTenant(ctx, "abc"); err != nil {
		t.Fatal(err)
	}
	if n := f.Live("sanholo/tenants/abc/"); n != 0 {
		t.Fatalf("%d live versions remain under the prefix", n)
	}
	if names, err := c.List(ctx, "abc"); err != nil || len(names) != 0 {
		t.Fatalf("list after purge = %v, %v", names, err)
	}
	for _, id := range []string{"abc-d", "ab"} {
		if n := f.Live("sanholo/tenants/" + id + "/"); n != 1 {
			t.Errorf("%s has %d live versions after purging abc, want 1", id, n)
		}
	}
	// Repeatable, and fine for a tenant with nothing stored.
	if err := c.PurgeTenant(ctx, "abc"); err != nil {
		t.Fatalf("second purge: %v", err)
	}
	if err := c.PurgeTenant(ctx, "never-existed"); err != nil {
		t.Fatalf("purge of unknown tenant: %v", err)
	}
}

func TestPurgeFailsLoudlyIfVersionsRemain(t *testing.T) {
	c, f, _ := setup(t)
	ctx := context.Background()
	if err := c.Put(ctx, "t1", creds); err != nil {
		t.Fatal(err)
	}
	// A qm that acknowledges destroy but keeps the data.
	f.SetOverride(func(r qmfake.Request) (int, string, bool) {
		if strings.HasPrefix(r.URL, "/qm/v1/secret/destroy/") {
			return http.StatusNoContent, "", true
		}
		return 0, "", false
	})
	if err := c.PurgeTenant(ctx, "t1"); err == nil || !errors.Is(err, secrets.ErrProtocol) {
		t.Fatalf("err = %v, want ErrProtocol", err)
	}
}

func TestRotateLeavesExactlyOneVersion(t *testing.T) {
	c, f, _ := setup(t)
	ctx := context.Background()
	path := "sanholo/tenants/t1/game"
	if err := c.Put(ctx, "t1", creds); err != nil {
		t.Fatal(err)
	}
	if err := c.Put(ctx, "t1", creds); err != nil {
		t.Fatal(err)
	}
	if n := f.Live(path); n != 2 {
		t.Fatalf("Put should keep older versions, live = %d", n)
	}
	next := secrets.Credentials{APIToken: "NEWTOKEN-2", ReplicantCode: gameCode}
	if err := c.Rotate(ctx, "t1", next); err != nil {
		t.Fatal(err)
	}
	if n := f.Live(path); n != 1 {
		t.Fatalf("live after rotate = %d, want 1", n)
	}
	if got, _ := f.Stored(path); got["api_token"] != "NEWTOKEN-2" {
		t.Fatalf("stored = %v", got)
	}
	if err := c.Rotate(ctx, "t1", creds); err != nil {
		t.Fatal(err)
	}
	if n := f.Live(path); n != 1 {
		t.Fatalf("live after second rotate = %d", n)
	}
	// Write first, destroy after: the last two requests of a rotate.
	reqs := f.Requests()
	var posts []string
	for _, r := range reqs[len(reqs)-3:] {
		if r.Method == http.MethodPost {
			posts = append(posts, strings.Split(r.URL, "/")[4])
		}
	}
	if fmt.Sprint(posts) != "[data destroy]" {
		t.Fatalf("order of writes = %v", posts)
	}
}

func TestRotateWithFailedDestroyKeepsNewValueAndSaysSo(t *testing.T) {
	c, f, _ := setup(t)
	ctx := context.Background()
	path := "sanholo/tenants/t1/game"
	if err := c.Put(ctx, "t1", creds); err != nil {
		t.Fatal(err)
	}
	f.SetOverride(func(r qmfake.Request) (int, string, bool) {
		if strings.HasPrefix(r.URL, "/qm/v1/secret/destroy/") {
			return http.StatusInternalServerError, "", true
		}
		return 0, "", false
	})
	err := c.Rotate(ctx, "t1", secrets.Credentials{APIToken: "NEW", ReplicantCode: gameCode})
	if !errors.Is(err, secrets.ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if got, _ := f.Stored(path); got["api_token"] != "NEW" {
		t.Fatalf("new value not stored: %v", got)
	}
	f.SetOverride(nil)
	if err := c.Rotate(ctx, "t1", secrets.Credentials{APIToken: "NEW", ReplicantCode: gameCode}); err != nil {
		t.Fatal(err)
	}
	if n := f.Live(path); n != 1 {
		t.Fatalf("live after retry = %d", n)
	}
}

func TestTokenOnlyInPutBodyAndNeverLogged(t *testing.T) {
	c, f, logs := setup(t)
	ctx := context.Background()
	// Every operation, including failing ones.
	_ = c.Put(ctx, "t1", creds)
	_, _ = c.List(ctx, "t1")
	_ = c.Rotate(ctx, "t1", creds)
	_ = c.PurgeTenant(ctx, "t1")
	_ = c.Put(ctx, "../x", creds)
	f.SetOverride(func(r qmfake.Request) (int, string, bool) {
		return 500, `{"echo":"` + r.Body + `"}`, true
	})
	errs := []error{c.Put(ctx, "t2", creds), c.Rotate(ctx, "t2", creds)}
	f.SetOverride(nil)

	for _, r := range f.Requests() {
		inBody := strings.Contains(r.Body, gameToken)
		outside := strings.Contains(r.URL, gameToken) || strings.Contains(fmt.Sprint(r.Header), gameToken)
		if outside {
			t.Errorf("token outside the body of %s %s", r.Method, r.URL)
		}
		isData := r.Method == http.MethodPost && strings.HasPrefix(r.URL, "/qm/v1/secret/data/")
		if inBody && !isData {
			t.Errorf("token in the body of %s %s", r.Method, r.URL)
		}
	}
	if s := logs.String(); strings.Contains(s, gameToken) || strings.Contains(s, cpToken) {
		t.Fatalf("log contains a secret:\n%s", s)
	}
	if logs.String() == "" {
		t.Fatal("expected some log output to scan")
	}
	for _, err := range errs {
		if err == nil || strings.Contains(err.Error(), gameToken) || strings.Contains(err.Error(), cpToken) {
			t.Fatalf("error echoes a secret or is nil: %v", err)
		}
	}
}

func TestWriteOnlyPolicy(t *testing.T) {
	c, f, _ := setup(t)
	if err := c.Put(context.Background(), "t1", creds); err != nil {
		t.Fatal(err)
	}
	// The fake enforces the principal's policy: a read of a value is
	// refused even with the right token. The client has no way to ask.
	req, _ := http.NewRequest(http.MethodGet, f.URL()+"/qm/v1/secret/data/sanholo/tenants/t1/game", nil)
	req.Header.Set("X-Qm-Token", cpToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusForbidden || strings.Contains(string(b), gameToken) {
		t.Fatalf("read of a value: %d %s", resp.StatusCode, b)
	}
}

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   error
	}{
		{"401", 401, secrets.ErrDenied},
		{"403", 403, secrets.ErrDenied},
		{"429", 429, secrets.ErrUnavailable},
		{"500", 500, secrets.ErrUnavailable},
		{"503", 503, secrets.ErrUnavailable},
		{"400", 400, secrets.ErrProtocol},
		{"409", 409, secrets.ErrProtocol},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, f, _ := setup(t)
			f.SetOverride(func(r qmfake.Request) (int, string, bool) {
				return tc.status, `{"errors":["` + gameToken + `"]}`, true
			})
			err := c.Put(context.Background(), "t1", creds)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			var e *secrets.Error
			if !errors.As(err, &e) || e.Status != tc.status || e.Op != "put" {
				t.Fatalf("error = %#v", err)
			}
			if strings.Contains(err.Error(), gameToken) {
				t.Fatal("error echoes the response body")
			}
		})
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
	for name, body := range map[string]string{
		"dotdot":     `{"data":{"keys":["../other"]}}`,
		"slash":      `{"data":{"keys":["a/b"]}}`,
		"empty":      `{"data":{"keys":[""]}}`,
		"dir dotdot": `{"data":{"keys":["../"]}}`,
		"query":      `{"data":{"keys":["x?y=1"]}}`,
		"junk":       `not json`,
	} {
		t.Run(name, func(t *testing.T) {
			c, f, _ := setup(t)
			f.SetOverride(func(r qmfake.Request) (int, string, bool) {
				if strings.Contains(r.URL, "list=true") {
					return 200, body, true
				}
				return 0, "", false
			})
			if err := c.PurgeTenant(ctx, "t1"); !errors.Is(err, secrets.ErrProtocol) {
				t.Fatalf("err = %v, want ErrProtocol", err)
			}
			for _, r := range f.Requests() {
				if strings.Contains(r.URL, "..") || strings.Contains(r.URL, "destroy") {
					t.Fatalf("a hostile key became a request: %s", r.URL)
				}
			}
		})
	}

	t.Run("oversize", func(t *testing.T) {
		c, f, _ := setup(t)
		f.SetOverride(func(qmfake.Request) (int, string, bool) {
			return 200, `{"data":{"version":1,"pad":"` + strings.Repeat("x", 2<<20) + `"}}`, true
		})
		if err := c.Put(ctx, "t1", creds); !errors.Is(err, secrets.ErrProtocol) {
			t.Fatalf("err = %v, want ErrProtocol", err)
		}
	})
	t.Run("endless tree", func(t *testing.T) {
		c, f, _ := setup(t)
		var n atomic.Int32
		f.SetOverride(func(qmfake.Request) (int, string, bool) {
			if n.Add(1) > 50 { // a client with no depth limit stops here instead of running away
				return 500, "", true
			}
			return 200, `{"data":{"keys":["d/"]}}`, true
		})
		if err := c.PurgeTenant(ctx, "t1"); !errors.Is(err, secrets.ErrProtocol) {
			t.Fatalf("err = %v, want ErrProtocol", err)
		}
		if n := len(f.Requests()); n > 20 {
			t.Fatalf("%d requests for an endless tree", n)
		}
	})
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
}

func TestReady(t *testing.T) {
	c, f, _ := setup(t)
	ctx := context.Background()
	if err := c.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	if r := f.Requests()[0]; r.URL != "/qm/v1/sys/ready" || r.Header.Get("X-Qm-Token") != "" {
		t.Fatalf("ready request = %+v", r)
	}
	f.SetSealed(true)
	if err := c.Ready(ctx); !errors.Is(err, secrets.ErrUnavailable) {
		t.Fatalf("sealed: %v", err)
	}
}

func TestWrongControlPlaneToken(t *testing.T) {
	f := qmfake.New(t, cpToken)
	c, _ := secrets.New(secrets.Config{Addr: f.URL(), Token: "wrong"})
	if err := c.Put(context.Background(), "t1", creds); !errors.Is(err, secrets.ErrDenied) {
		t.Fatalf("err = %v", err)
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

// TestAPISurface is the structural guarantee that this package cannot hand a
// secret value back: every exported function and method is on an allow-list
// with its exact result types, which are only errors, booleans, strings that
// are names or messages, and name lists. Adding an exported function, or
// changing a result type, fails here until a human decides it is safe.
func TestAPISurface(t *testing.T) {
	want := map[string]string{
		"New":                "(*Client, error)",
		"ParseAddr":          "(string, error)",
		"Client.Enabled":     "bool",
		"Client.Ready":       "error",
		"Client.Put":         "error",
		"Client.List":        "([]string, error)",
		"Client.PurgeTenant": "error",
		"Client.Rotate":      "error",
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
	// anything but error, bool, []string or (T, error).
	ct := reflect.TypeFor[*secrets.Client]()
	okOut := map[reflect.Type]bool{
		reflect.TypeFor[error](): true, reflect.TypeFor[bool](): true, reflect.TypeFor[[]string](): true,
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
