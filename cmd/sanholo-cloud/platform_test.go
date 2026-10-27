package main

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lhaig/sanholo-cloud/internal/config"
	"github.com/lhaig/sanholo-cloud/internal/dbtest"
	"github.com/lhaig/sanholo-cloud/internal/push"
	"github.com/lhaig/sanholo-cloud/internal/secrets"
	"github.com/lhaig/sanholo-cloud/internal/secrets/qmfake"
)

const (
	tMetrics = "metrics-0123456789abcdefghijklmnopqrstuv"
	tAlerts  = "alerts-0123456789abcdefghijklmnopqrstuvwx"
	tAMBody  = `{"version":"4","alerts":[{"status":"firing","fingerprint":"f","labels":{"kind":"qm-sealed"},"annotations":{"summary":"s"}}]}`
)

func platformCall(t *testing.T, cfg config.Config, sec *secrets.Client, svc *push.Service, method, path, token, body string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	app := buildPlatform(ctx, cfg, dbtest.Migrated(t), sec, svc)
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestBuildPlatform(t *testing.T) {
	cfg := config.Config{Internal: config.Internal{Addr: "127.0.0.1:9464", MetricsToken: tMetrics, AlertsToken: tAlerts}}
	qm := qmfake.New(t, "cp-token")
	sec, err := secrets.New(secrets.Config{Addr: qm.URL(), Token: "cp-token", Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	svc := push.New(push.Config{TenantOrigin: func(string) string { return "https://x" }, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})

	t.Run("metrics read the database, qm and push", func(t *testing.T) {
		var code int
		var body string
		// The qm probe runs in the background; wait for its first result.
		for range 100 {
			code, body = platformCall(t, cfg, sec, svc, "GET", "/metrics", tMetrics, "")
			if strings.Contains(body, "sanholo_cloud_qm_up") {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		for _, want := range []string{"sanholo_cloud_database_up 1", "sanholo_cloud_qm_up 1", `sanholo_cloud_push_messages_total{result="delivered"} 0`, `sanholo_cloud_operator_alerts_total{result="pushed"} 0`} {
			if code != 200 || !strings.Contains(body, want) {
				t.Fatalf("status %d, body lacks %q:\n%s", code, want, body)
			}
		}
	})
	t.Run("alert intake on with push", func(t *testing.T) {
		if code, _ := platformCall(t, cfg, sec, svc, "POST", "/internal/operator-alerts", tAlerts, tAMBody); code != 200 {
			t.Fatalf("status %d, want 200", code)
		}
	})
	t.Run("alert intake off without push", func(t *testing.T) {
		if code, _ := platformCall(t, cfg, sec, nil, "POST", "/internal/operator-alerts", tAlerts, tAMBody); code != 404 {
			t.Fatalf("status %d, want 404", code)
		}
	})
	t.Run("a sealed qm is reported as sealed", func(t *testing.T) {
		qm.SetSealed(true)
		defer qm.SetSealed(false)
		var body string
		for range 100 {
			_, body = platformCall(t, cfg, sec, nil, "GET", "/metrics", tMetrics, "")
			if strings.Contains(body, "sanholo_cloud_qm_sealed") {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if !strings.Contains(body, "sanholo_cloud_qm_sealed 1") || !strings.Contains(body, "sanholo_cloud_qm_up 0") {
			t.Fatalf("body:\n%s", body)
		}
	})
	t.Run("no qm metrics without qm", func(t *testing.T) {
		_, body := platformCall(t, cfg, nil, nil, "GET", "/metrics", tMetrics, "")
		if strings.Contains(body, "sanholo_cloud_qm_up") || !strings.Contains(body, "sanholo_cloud_database_up 1") {
			t.Fatalf("body:\n%s", body)
		}
	})
}
