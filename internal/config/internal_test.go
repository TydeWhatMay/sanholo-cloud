package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testMetricsToken = "metrics-token-0123456789abcdefghijklmnop"
	testAlertsToken  = "alertmanager-token-0123456789abcdefghijk"
)

// writeToken writes v to a fresh file and returns its path.
func writeToken(t *testing.T, name, v string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(v), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// internalEnv is a complete platform listener configuration.
func internalEnv(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"INTERNAL_LISTEN_ADDR":    "10.0.0.5:9464",
		"METRICS_TOKEN_FILE":      writeToken(t, "metrics", testMetricsToken+"\n"),
		"ALERTMANAGER_TOKEN_FILE": writeToken(t, "alerts", testAlertsToken+"\n"),
	}
}

func TestInternalSettings(t *testing.T) {
	full := internalEnv(t)
	cases := []struct {
		name       string
		env        map[string]string
		production bool
		wantErr    string
		check      func(t *testing.T, in Internal)
	}{
		{name: "off by default", env: nil, check: func(t *testing.T, in Internal) {
			if in.Enabled() {
				t.Error("enabled with nothing set")
			}
		}},
		{name: "complete", env: full, check: func(t *testing.T, in Internal) {
			if in.Addr != "10.0.0.5:9464" || in.MetricsToken != testMetricsToken || in.AlertsToken != testAlertsToken {
				t.Errorf("got addr %q, tokens trimmed=%v/%v", in.Addr, in.MetricsToken == testMetricsToken, in.AlertsToken == testAlertsToken)
			}
		}},
		{name: "metrics only in development", env: map[string]string{
			"INTERNAL_LISTEN_ADDR": "10.0.0.5:9464", "METRICS_TOKEN_FILE": full["METRICS_TOKEN_FILE"],
		}, check: func(t *testing.T, in Internal) {
			if in.MetricsToken == "" || in.AlertsToken != "" {
				t.Error("want only the metrics token")
			}
		}},
		{name: "production needs the listener", env: nil, production: true, wantErr: "INTERNAL_LISTEN_ADDR is required"},
		{name: "production needs both tokens", env: map[string]string{
			"INTERNAL_LISTEN_ADDR": "10.0.0.5:9464", "METRICS_TOKEN_FILE": full["METRICS_TOKEN_FILE"],
		}, production: true, wantErr: "required when APP_ENV=production"},
		{name: "production complete", env: full, production: true},
		{name: "token without listener", env: map[string]string{"METRICS_TOKEN_FILE": full["METRICS_TOKEN_FILE"]}, wantErr: "INTERNAL_LISTEN_ADDR is not"},
		{name: "listener without tokens", env: map[string]string{"INTERNAL_LISTEN_ADDR": "10.0.0.5:9464"}, wantErr: "set METRICS_TOKEN_FILE"},
		{name: "same address as the app", env: map[string]string{
			"INTERNAL_LISTEN_ADDR": "127.0.0.1:8090", "METRICS_TOKEN_FILE": full["METRICS_TOKEN_FILE"],
		}, wantErr: "must differ from LISTEN_ADDR"},
		{name: "bad address", env: map[string]string{
			"INTERNAL_LISTEN_ADDR": "10.0.0.5", "METRICS_TOKEN_FILE": full["METRICS_TOKEN_FILE"],
		}, wantErr: "INTERNAL_LISTEN_ADDR"},
		{name: "unreadable token", env: map[string]string{
			"INTERNAL_LISTEN_ADDR": "10.0.0.5:9464", "METRICS_TOKEN_FILE": "/nonexistent/token",
		}, wantErr: "METRICS_TOKEN_FILE: cannot read"},
		// 31 characters: one short of the minimum (not a round number).
		{name: "short token", env: map[string]string{
			"INTERNAL_LISTEN_ADDR": "10.0.0.5:9464", "METRICS_TOKEN_FILE": writeToken(t, "short", strings.Repeat("a", 31)),
		}, wantErr: "at least 32"},
		{name: "token with inner space", env: map[string]string{
			"INTERNAL_LISTEN_ADDR": "10.0.0.5:9464", "ALERTMANAGER_TOKEN_FILE": writeToken(t, "space", strings.Repeat("a", 20)+" "+strings.Repeat("b", 20)),
		}, wantErr: "white space"},
		{name: "same token twice", env: map[string]string{
			"INTERNAL_LISTEN_ADDR": "10.0.0.5:9464", "METRICS_TOKEN_FILE": full["METRICS_TOKEN_FILE"],
			"ALERTMANAGER_TOKEN_FILE": writeToken(t, "dup", testMetricsToken),
		}, wantErr: "different tokens"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in, err := internalSettings(env(tc.env), defaultListenAddr, tc.production)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if tc.check != nil {
					tc.check(t, in)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %v, want it to contain %q", err, tc.wantErr)
			}
			for _, tok := range []string{testMetricsToken, testAlertsToken} {
				if strings.Contains(err.Error(), tok) {
					t.Fatal("error message carries a token")
				}
			}
		})
	}
}

// Load wires the platform listener through and refuses it in production
// when missing (the full production fixture lives in config_test.go).
func TestLoadInternal(t *testing.T) {
	m := internalEnv(t)
	m["DATABASE_URL"] = "postgres://u:p@h/d"
	c, err := Load(env(m))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Internal.Enabled() || c.Internal.AlertsToken != testAlertsToken {
		t.Fatalf("Internal not loaded: enabled=%v", c.Internal.Enabled())
	}
}
