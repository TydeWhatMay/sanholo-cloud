package server

import (
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lhaig/sanholo-cloud/internal/metrics"
	"github.com/lhaig/sanholo-cloud/internal/opalert"
)

const (
	platMetricsToken = "metrics-0123456789abcdefghijklmnopqrstuv"
	platAlertsToken  = "alerts-0123456789abcdefghijklmnopqrstuvwx"
	amBody           = `{"version":"4","status":"firing","alerts":[{"status":"firing","fingerprint":"f1","labels":{"alertname":"QMSealed","kind":"qm-sealed","severity":"critical"},"annotations":{"summary":"qm is sealed"}}]}`
)

type recNotifier struct {
	got  []string
	sev  []string
	fail bool
}

func (r *recNotifier) NotifyOperator(kind, severity, text string) error {
	if r.fail {
		return errors.New("queue full")
	}
	r.got = append(r.got, kind+": "+text)
	r.sev = append(r.sev, severity)
	return nil
}

func platformReq(t *testing.T, p Platform, method, path, auth, body string) (int, string, map[string][]string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := NewPlatform(p).Test(req, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func TestPlatformAuth(t *testing.T) {
	n := &recNotifier{}
	p := Platform{
		MetricsToken: platMetricsToken, Metrics: &metrics.Source{Version: "t"},
		AlertsToken: platAlertsToken, Alerts: opalert.New(n, nil),
	}
	cases := []struct {
		name, method, path, auth, body string
		code                           int
	}{
		{"metrics without a token", "GET", "/metrics", "", "", 401},
		{"metrics with a wrong token", "GET", "/metrics", "Bearer " + platMetricsToken + "x", "", 401},
		{"metrics with a prefix of the token", "GET", "/metrics", "Bearer " + platMetricsToken[:20], "", 401},
		{"metrics with basic auth", "GET", "/metrics", "Basic " + platMetricsToken, "", 401},
		{"metrics with the alerts token", "GET", "/metrics", "Bearer " + platAlertsToken, "", 401},
		{"metrics with its token", "GET", "/metrics", "Bearer " + platMetricsToken, "", 200},
		{"metrics lower-case scheme", "GET", "/metrics", "bearer " + platMetricsToken, "", 200},
		{"alerts without a token", "POST", "/internal/operator-alerts", "", amBody, 401},
		{"alerts with the metrics token", "POST", "/internal/operator-alerts", "Bearer " + platMetricsToken, amBody, 401},
		{"alerts with its token", "POST", "/internal/operator-alerts", "Bearer " + platAlertsToken, amBody, 200},
		{"alerts with a bad body", "POST", "/internal/operator-alerts", "Bearer " + platAlertsToken, `{"version":"3"}`, 400},
		{"app routes are not here", "GET", "/account", "Bearer " + platMetricsToken, "", 404},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body, hdr := platformReq(t, p, tc.method, tc.path, tc.auth, tc.body)
			if code != tc.code {
				t.Fatalf("status %d, want %d (%s)", code, tc.code, body)
			}
			if code == 401 && strings.Contains(body, "sanholo_cloud") {
				t.Fatal("metrics served on a 401")
			}
			if code == 200 && tc.path == "/metrics" {
				if !strings.Contains(body, `sanholo_cloud_build_info{version="t"} 1`) || !strings.HasPrefix(hdr["Content-Type"][0], "text/plain; version=0.0.4") {
					t.Fatalf("metrics body or type wrong: %v %q", hdr["Content-Type"], body)
				}
			}
		})
	}
	if len(n.got) != 1 || n.got[0] != "qm-sealed: qm is sealed" || n.sev[0] != "critical" {
		t.Fatalf("operator pushes = %v (severity %v), want exactly the authenticated one, critical", n.got, n.sev)
	}
}

// A route whose token is not configured does not exist.
func TestPlatformRoutesOffWithoutToken(t *testing.T) {
	p := Platform{Metrics: &metrics.Source{}, Alerts: opalert.New(&recNotifier{}, nil)}
	for _, r := range []struct{ method, path, body string }{{"GET", "/metrics", ""}, {"POST", "/internal/operator-alerts", amBody}} {
		if code, _, _ := platformReq(t, p, r.method, r.path, "Bearer ", r.body); code != 404 {
			t.Errorf("%s %s with no token configured: %d, want 404", r.method, r.path, code)
		}
	}
}

// A push that cannot be queued answers 503, so Alertmanager retries.
func TestPlatformAlertsRetryOnQueueFull(t *testing.T) {
	p := Platform{AlertsToken: platAlertsToken, Alerts: opalert.New(&recNotifier{fail: true}, nil)}
	if code, _, _ := platformReq(t, p, "POST", "/internal/operator-alerts", "Bearer "+platAlertsToken, amBody); code != 503 {
		t.Fatalf("status %d, want 503", code)
	}
}
