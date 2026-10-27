package nomad_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lhaig/sanholo-cloud/internal/nomad"
	"github.com/lhaig/sanholo-cloud/internal/nomad/nomadfake"
)

const (
	testToken = "nomad-acl-SECRET-token-7f3a91"
	testJob   = "sanholo-tenant-acme-42"
)

type env struct {
	fake *nomadfake.Fake
	c    *nomad.Client
	logs *bytes.Buffer
}

func newEnv(t *testing.T) *env {
	t.Helper()
	f := nomadfake.New(t, testToken, "sanholo")
	logs := &bytes.Buffer{}
	c, err := nomad.New(nomad.Config{
		Addr: f.URL(), Token: testToken, Namespace: "sanholo", Timeout: 2 * time.Second,
		Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	// The tenant's volume exists before its job is registered, as the
	// provisioner arranges: a job without one is never placed. Seeded without
	// requests so the tests below can count theirs.
	f.AddVolume("sanholo-tenant-acme-42")
	return &env{f, c, logs}
}

func (e *env) job(t *testing.T, version string) nomad.Job {
	t.Helper()
	j, err := nomad.BuildJob(nomadfake.Settings(), "acme-42", version)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestRegisterSendsTokenNamespaceAndIndex(t *testing.T) {
	e := newEnv(t)
	if err := e.c.Register(context.Background(), e.job(t, "0.7.1"), nomad.RegisterOptions{}); err != nil {
		t.Fatal(err)
	}
	reqs := e.fake.Requests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests", len(reqs))
	}
	r := reqs[0]
	if r.Method != "POST" || r.Path != "/v1/jobs" || r.Token != testToken || !strings.Contains(r.Query, "namespace=sanholo") {
		t.Errorf("request = %+v", r)
	}
	if !strings.Contains(r.Body, `"EnforceIndex":true`) || !strings.Contains(r.Body, `"JobModifyIndex":0`) {
		t.Errorf("a create must say the job may not exist yet: %s", r.Body)
	}
	if exists, _ := e.fake.Has(testJob); !exists {
		t.Error("the job was not registered")
	}
}

func TestRegisterConflicts(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.c.Register(ctx, e.job(t, "0.7.1"), nomad.RegisterOptions{}); err != nil {
		t.Fatal(err)
	}
	// Creating again with index 0: the job exists.
	if err := e.c.Register(ctx, e.job(t, "0.7.1"), nomad.RegisterOptions{}); !errors.Is(err, nomad.ErrConflict) {
		t.Errorf("second create: %v, want ErrConflict", err)
	}
	// Updating with a stale index.
	if err := e.c.Register(ctx, e.job(t, "0.7.2"), nomad.RegisterOptions{ModifyIndex: 999}); !errors.Is(err, nomad.ErrConflict) {
		t.Errorf("stale index: %v, want ErrConflict", err)
	}
	info, err := e.c.Job(ctx, testJob)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.c.Register(ctx, e.job(t, "0.7.2"), nomad.RegisterOptions{ModifyIndex: info.ModifyIndex}); err != nil {
		t.Errorf("update with the current index: %v", err)
	}
	if e.fake.Image(testJob) != "ghcr.io/tydewhatmay/sanholo:v0.7.2" {
		t.Errorf("image = %q", e.fake.Image(testJob))
	}
}

func TestRegisterRefusesForeignJobAndNamespace(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	foreign := e.job(t, "0.7.1")
	foreign.ID = "other-job"
	if err := e.c.Register(ctx, foreign, nomad.RegisterOptions{}); !errors.Is(err, nomad.ErrInvalidJob) {
		t.Errorf("foreign job id: %v", err)
	}
	wrongNS := e.job(t, "0.7.1")
	wrongNS.Namespace = "default"
	if err := e.c.Register(ctx, wrongNS, nomad.RegisterOptions{}); !errors.Is(err, nomad.ErrInvalidJob) {
		t.Errorf("wrong namespace: %v", err)
	}
	if n := len(e.fake.Requests()); n != 0 {
		t.Errorf("%d requests were sent for refused jobs", n)
	}
}

func TestClientOnlyTouchesTenantJobs(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, id := range []string{"other-job", "sanholo-tenant-", "sanholo-tenant-A", "sanholo-tenant-../x", "sanholo-tenant-a/b", "sanholo-tenant-a?purge=true", ""} {
		if err := e.c.Stop(ctx, id, true); !errors.Is(err, nomad.ErrInvalidJob) {
			t.Errorf("Stop(%q) = %v", id, err)
		}
		if _, err := e.c.Job(ctx, id); !errors.Is(err, nomad.ErrInvalidJob) {
			t.Errorf("Job(%q) = %v", id, err)
		}
		if _, err := e.c.Status(ctx, id); !errors.Is(err, nomad.ErrInvalidJob) {
			t.Errorf("Status(%q) = %v", id, err)
		}
	}
	if err := e.c.RestartAllocation(ctx, "../../jobs"); !errors.Is(err, nomad.ErrInvalidJob) {
		t.Errorf("RestartAllocation with a path: %v", err)
	}
	if n := len(e.fake.Requests()); n != 0 {
		t.Errorf("%d requests were sent for refused ids", n)
	}
}

func TestStatusPhases(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	st, err := e.c.Status(ctx, testJob)
	if err != nil || st.Phase != nomad.PhaseAbsent {
		t.Fatalf("absent job: %v %v", st.Phase, err)
	}
	if err := e.c.Register(ctx, e.job(t, "0.7.1"), nomad.RegisterOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		phase nomadfake.Phase
		want  nomad.Phase
	}{
		{nomadfake.Pending, nomad.PhasePending},
		{nomadfake.Healthy, nomad.PhaseHealthy},
		{nomadfake.Unhealthy, nomad.PhaseUnhealthy},
		{nomadfake.Failed, nomad.PhaseFailed},
	} {
		e.fake.ReplaceAlloc(testJob) // Nomad decides health once per allocation
		e.fake.SetPhase(testJob, tc.phase)
		st, err := e.c.Status(ctx, testJob)
		if err != nil || st.Phase != tc.want {
			t.Errorf("%s: phase %q err %v, want %q", tc.phase, st.Phase, err, tc.want)
		}
	}
	e.fake.ReplaceAlloc(testJob)
	e.fake.SetPhase(testJob, nomadfake.Healthy)
	st, _ = e.c.Status(ctx, testJob)
	if st.Address != "10.0.0.7:23456" || st.AllocID == "" || st.Job.Image != "ghcr.io/tydewhatmay/sanholo:v0.7.1" {
		t.Errorf("healthy status = %+v", st)
	}
	if err := e.c.Stop(ctx, testJob, false); err != nil {
		t.Fatal(err)
	}
	if st, _ = e.c.Status(ctx, testJob); st.Phase != nomad.PhaseStopped {
		t.Errorf("after stop: %q", st.Phase)
	}
}

// An allocation of an older version says nothing about the current one.
func TestStatusIgnoresOldVersions(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.c.Register(ctx, e.job(t, "0.7.1"), nomad.RegisterOptions{}); err != nil {
		t.Fatal(err)
	}
	e.fake.SetPhase(testJob, nomadfake.Healthy)
	info, _ := e.c.Job(ctx, testJob)
	if err := e.c.Register(ctx, e.job(t, "0.7.2"), nomad.RegisterOptions{ModifyIndex: info.ModifyIndex}); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.c.Status(ctx, testJob); st.Phase != nomad.PhasePending {
		t.Errorf("new version is %q, want pending until its own allocation is healthy", st.Phase)
	}
}

func TestStopIsIdempotentAndPurges(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.c.Stop(ctx, testJob, true); err != nil {
		t.Errorf("stopping an absent job: %v", err)
	}
	if err := e.c.Register(ctx, e.job(t, "0.7.1"), nomad.RegisterOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Stop(ctx, testJob, true); err != nil {
		t.Fatal(err)
	}
	if exists, _ := e.fake.Has(testJob); exists {
		t.Error("purge left the job behind")
	}
	last := e.fake.Requests()[len(e.fake.Requests())-1]
	if last.Method != "DELETE" || !strings.Contains(last.Query, "purge=true") {
		t.Errorf("last request = %+v", last)
	}
}

func TestRestartAllocation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.c.Register(ctx, e.job(t, "0.7.1"), nomad.RegisterOptions{}); err != nil {
		t.Fatal(err)
	}
	e.fake.SetPhase(testJob, nomadfake.Healthy)
	st, _ := e.c.Status(ctx, testJob)
	if err := e.c.RestartAllocation(ctx, st.AllocID); err != nil {
		t.Fatal(err)
	}
	if e.fake.Restarts(testJob) != 1 {
		t.Error("the allocation was not restarted")
	}
}

func TestErrorMapping(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, tc := range []struct {
		status int
		body   string
		want   error
	}{
		{403, "Permission denied", nomad.ErrDenied},
		{400, "job validation failed", nomad.ErrRejected},
		{429, "slow down", nomad.ErrUnavailable},
		{500, "boom", nomad.ErrUnavailable},
		{503, "no leader", nomad.ErrUnavailable},
		{418, "teapot", nomad.ErrProtocol},
	} {
		e.fake.Override(func(nomadfake.Request) (int, string, bool) { return tc.status, tc.body, true })
		_, err := e.c.Job(ctx, testJob)
		if !errors.Is(err, tc.want) {
			t.Errorf("status %d: %v, want %v", tc.status, err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), tc.body) {
			t.Errorf("status %d: the error quotes the response body: %v", tc.status, err)
		}
	}
	e.fake.Override(func(nomadfake.Request) (int, string, bool) { return 200, "not json", true })
	if _, err := e.c.Job(ctx, testJob); !errors.Is(err, nomad.ErrProtocol) {
		t.Errorf("garbage answer: %v", err)
	}
	// A job answer for another id is a protocol error, not a status.
	e.fake.Override(func(nomadfake.Request) (int, string, bool) { return 200, `{"ID":"sanholo-tenant-other"}`, true })
	if _, err := e.c.Job(ctx, testJob); !errors.Is(err, nomad.ErrProtocol) {
		t.Errorf("answer for another job: %v", err)
	}
}

func TestWrongTokenIsDeniedAndNeverLogged(t *testing.T) {
	f := nomadfake.New(t, testToken, "sanholo")
	const wrong = "wrong-acl-token-SECRET-00112233"
	logs := &bytes.Buffer{}
	c, err := nomad.New(nomad.Config{Addr: f.URL(), Token: wrong, Logger: slog.New(slog.NewTextHandler(logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Job(context.Background(), testJob)
	if !errors.Is(err, nomad.ErrDenied) {
		t.Fatalf("err = %v", err)
	}
	for _, s := range []string{err.Error(), logs.String()} {
		if strings.Contains(s, wrong) {
			t.Error("the token appears in an error or the log")
		}
	}
}

// The token reaches only the configured origin: a redirect is not followed.
func TestRedirectIsNotFollowed(t *testing.T) {
	var hits atomic.Int32
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer evil.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()
	c, err := nomad.New(nomad.Config{Addr: redirector.URL, Token: testToken})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Job(context.Background(), testJob); err == nil {
		t.Error("a redirect answered as success")
	}
	if hits.Load() != 0 {
		t.Error("the client followed a redirect (and sent the token with it)")
	}
}

func TestResponseIsBounded(t *testing.T) {
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ID":"` + testJob + `","pad":"` + strings.Repeat("x", 5<<20) + `"}`))
	}))
	defer big.Close()
	c, _ := nomad.New(nomad.Config{Addr: big.URL, Token: testToken})
	if _, err := c.Job(context.Background(), testJob); !errors.Is(err, nomad.ErrProtocol) {
		t.Errorf("oversized answer: %v", err)
	}
}

func TestTimeout(t *testing.T) {
	block := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	defer slow.Close()
	defer close(block)
	c, _ := nomad.New(nomad.Config{Addr: slow.URL, Token: testToken, Timeout: 100 * time.Millisecond})
	start := time.Now()
	if _, err := c.Job(context.Background(), testJob); !errors.Is(err, nomad.ErrUnavailable) {
		t.Errorf("err = %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Error("the timeout did not apply")
	}
}

func TestNewValidates(t *testing.T) {
	tests := []struct {
		name string
		cfg  nomad.Config
		ok   bool
	}{
		{"disabled", nomad.Config{}, true},
		{"https", nomad.Config{Addr: "https://nomad.example.org:4646", Token: "t"}, true},
		{"loopback http", nomad.Config{Addr: "http://127.0.0.1:4646", Token: "t"}, true},
		{"remote http", nomad.Config{Addr: "http://nomad.example.org:4646", Token: "t"}, false},
		{"path in address", nomad.Config{Addr: "https://nomad.example.org/x", Token: "t"}, false},
		{"userinfo", nomad.Config{Addr: "https://u:p@nomad.example.org", Token: "t"}, false},
		{"no token", nomad.Config{Addr: "https://nomad.example.org"}, false},
		{"both tokens", nomad.Config{Addr: "https://nomad.example.org", Token: "t", TokenFile: "/x"}, false},
		{"token without address", nomad.Config{Token: "t"}, false},
		{"newline in token", nomad.Config{Addr: "https://nomad.example.org", Token: "t\nX-Evil: 1"}, false},
		{"bad namespace", nomad.Config{Addr: "https://nomad.example.org", Token: "t", Namespace: "a/b"}, false},
		{"unreadable token file", nomad.Config{Addr: "https://nomad.example.org", TokenFile: "/nonexistent/token"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := nomad.New(tc.cfg)
			if (err == nil) != tc.ok {
				t.Errorf("New() = %v, want ok=%v", err, tc.ok)
			}
			if err != nil && tc.cfg.Token != "" && strings.Contains(err.Error(), tc.cfg.Token) && len(tc.cfg.Token) > 4 {
				t.Error("the error quotes the token")
			}
		})
	}
	if c, _ := nomad.New(nomad.Config{}); c.Enabled() {
		t.Error("an empty config is enabled")
	}
	if err := (*nomad.Client)(nil).Stop(context.Background(), testJob, true); !errors.Is(err, nomad.ErrDisabled) {
		t.Errorf("nil client: %v", err)
	}
}

// The token file is re-read on every request, so a rotated token applies
// without a restart.
func TestTokenFileIsReread(t *testing.T) {
	f := nomadfake.New(t, testToken, "sanholo")
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("stale-token-aaaa\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := nomad.New(nomad.Config{Addr: f.URL(), TokenFile: path})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Job(context.Background(), testJob); !errors.Is(err, nomad.ErrDenied) {
		t.Fatalf("stale token: %v", err)
	}
	if err := os.WriteFile(path, []byte(testToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Job(context.Background(), testJob); !errors.Is(err, nomad.ErrNotFound) {
		t.Errorf("rotated token: %v, want the request to be authorised (job not found)", err)
	}
}

// A transport failure is logged (kind only): the log is not empty, and it
// does not hold the token or the URL.
func TestTokenNeverLoggedOnTransportFailure(t *testing.T) {
	// Close the server: connection refused.
	srv := httptest.NewServer(http.NotFoundHandler())
	dead := srv.URL
	srv.Close()
	logs := &bytes.Buffer{}
	c, err := nomad.New(nomad.Config{Addr: dead, Token: testToken, Timeout: time.Second, Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Job(context.Background(), testJob)
	if !errors.Is(err, nomad.ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if logs.Len() == 0 {
		t.Fatal("nothing was logged for a failed request; the leak check below would prove nothing")
	}
	hostOnly := strings.TrimPrefix(dead, "http://")
	for _, s := range []string{err.Error(), logs.String()} {
		if strings.Contains(s, testToken) || strings.Contains(s, hostOnly) || strings.Contains(s, testJob) {
			t.Errorf("the token, the address or the job id is in %q", s)
		}
	}
}

// Nomad decides the deployment's health verdict once. A check that fails
// later, a restart and a replaced allocation show only in the live data.
func TestStatusLiveSeesWhatTheDeploymentVerdictCannot(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.c.Register(ctx, e.job(t, "0.7.1"), nomad.RegisterOptions{}); err != nil {
		t.Fatal(err)
	}
	e.fake.SetPhase(testJob, nomadfake.Healthy)
	st, err := e.c.StatusLive(ctx, testJob)
	if err != nil || st.Phase != nomad.PhaseHealthy || !st.ChecksRead || len(st.Checks) != 1 || st.Checks[0].Status != nomad.CheckSuccess {
		t.Fatalf("healthy: %+v %v", st, err)
	}
	e.fake.SetCheck(testJob, "failure")
	if st, _ := e.c.Status(ctx, testJob); st.Phase != nomad.PhaseHealthy {
		t.Errorf("the deployment verdict changed to %q; it must not", st.Phase)
	}
	if st, _ := e.c.StatusLive(ctx, testJob); st.Phase != nomad.PhaseUnhealthy {
		t.Errorf("a failing live check gives %q, want unhealthy", st.Phase)
	}
	e.fake.SetCheck(testJob, "pending")
	if st, _ := e.c.StatusLive(ctx, testJob); st.Phase != nomad.PhaseHealthy {
		t.Errorf("a pending check gives %q, want the deployment's healthy", st.Phase)
	}
}

func TestStatusReportsRestartsAndTheAllocation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.c.Register(ctx, e.job(t, "0.7.1"), nomad.RegisterOptions{}); err != nil {
		t.Fatal(err)
	}
	e.fake.SetPhase(testJob, nomadfake.Healthy)
	first, _ := e.c.Status(ctx, testJob)
	e.fake.BumpRestarts(testJob)
	e.fake.BumpRestarts(testJob)
	again, _ := e.c.Status(ctx, testJob)
	if first.Restarts != 0 || again.Restarts != 2 || again.AllocID != first.AllocID {
		t.Errorf("restarts %d then %d, alloc %q then %q", first.Restarts, again.Restarts, first.AllocID, again.AllocID)
	}
	e.fake.ReplaceAlloc(testJob)
	e.fake.SetPhase(testJob, nomadfake.Healthy)
	third, _ := e.c.Status(ctx, testJob)
	if third.AllocID == first.AllocID || third.Restarts != 0 {
		t.Errorf("replacement: alloc %q restarts %d", third.AllocID, third.Restarts)
	}
}

func TestChecksRefusesABadAllocationID(t *testing.T) {
	e := newEnv(t)
	if _, err := e.c.Checks(context.Background(), "../x"); err == nil {
		t.Error("a path-like allocation id was sent")
	}
	if len(e.fake.Requests()) != 0 {
		t.Error("a request was sent")
	}
}
