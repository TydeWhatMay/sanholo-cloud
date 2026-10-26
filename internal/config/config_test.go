package config

import (
	"strings"
	"testing"
)

func env(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func TestLoad(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		wantErr string // substring; empty means success
		check   func(t *testing.T, c Config)
	}{
		{
			name: "database url and defaults",
			env:  map[string]string{"DATABASE_URL": "postgres://u:p@h/d"},
			check: func(t *testing.T, c Config) {
				if c.DatabaseURL != "postgres://u:p@h/d" {
					t.Errorf("DatabaseURL = %q", c.DatabaseURL)
				}
				if c.ListenAddr != "127.0.0.1:8090" {
					t.Errorf("ListenAddr = %q", c.ListenAddr)
				}
				if !c.Beta.InviteOnly || c.Beta.MaxTenants != 20 || c.Beta.MaxGameAccountsPerTenant != 1 {
					t.Errorf("Beta = %+v", c.Beta)
				}
			},
		},
		{
			name: "database url wins over PG vars",
			env: map[string]string{
				"DATABASE_URL": "postgres://a/b", "PGHOST": "other", "PGUSER": "u", "PGDATABASE": "d",
			},
			check: func(t *testing.T, c Config) {
				if c.DatabaseURL != "postgres://a/b" {
					t.Errorf("DatabaseURL = %q", c.DatabaseURL)
				}
			},
		},
		{
			name: "PG vars build an escaped url",
			env: map[string]string{
				"PGHOST": "db.internal", "PGPORT": "5433", "PGUSER": "svc@x",
				"PGPASSWORD": "p@ss/word:1", "PGDATABASE": "sanholo", "PGSSLMODE": "require",
			},
			check: func(t *testing.T, c Config) {
				want := "postgres://svc%40x:p%40ss%2Fword%3A1@db.internal:5433/sanholo?sslmode=require"
				if c.DatabaseURL != want {
					t.Errorf("DatabaseURL = %q, want %q", c.DatabaseURL, want)
				}
			},
		},
		{
			name: "PG vars default port and no password",
			env:  map[string]string{"PGHOST": "h", "PGUSER": "u", "PGDATABASE": "d"},
			check: func(t *testing.T, c Config) {
				if c.DatabaseURL != "postgres://u@h/d" {
					t.Errorf("DatabaseURL = %q", c.DatabaseURL)
				}
			},
		},
		{
			name: "IPv6 PG host keeps its brackets",
			env:  map[string]string{"PGHOST": "::1", "PGPORT": "5432", "PGUSER": "u", "PGDATABASE": "d"},
			check: func(t *testing.T, c Config) {
				if c.DatabaseURL != "postgres://u@[::1]:5432/d" {
					t.Errorf("DatabaseURL = %q", c.DatabaseURL)
				}
			},
		},
		{name: "no database configured", env: map[string]string{}, wantErr: "DATABASE_URL"},
		{name: "partial PG vars", env: map[string]string{"PGHOST": "h"}, wantErr: "PGUSER"},
		{name: "bad PG port", env: map[string]string{"PGHOST": "h", "PGUSER": "u", "PGDATABASE": "d", "PGPORT": "x"}, wantErr: "PGPORT"},
		{
			name: "beta values",
			env: map[string]string{
				"DATABASE_URL": "postgres://h/d", "LISTEN_ADDR": "0.0.0.0:9000",
				"BETA_INVITE_ONLY": "true", "BETA_MAX_TENANTS": "5", "BETA_MAX_GAME_ACCOUNTS_PER_TENANT": "2",
			},
			check: func(t *testing.T, c Config) {
				if c.ListenAddr != "0.0.0.0:9000" || !c.Beta.InviteOnly || c.Beta.MaxTenants != 5 || c.Beta.MaxGameAccountsPerTenant != 2 {
					t.Errorf("config = %+v", c)
				}
			},
		},
		{name: "bad bool", env: map[string]string{"DATABASE_URL": "postgres://h/d", "BETA_INVITE_ONLY": "maybe"}, wantErr: "BETA_INVITE_ONLY"},
		{name: "bad int", env: map[string]string{"DATABASE_URL": "postgres://h/d", "BETA_MAX_TENANTS": "many"}, wantErr: "BETA_MAX_TENANTS"},
		{name: "zero tenants", env: map[string]string{"DATABASE_URL": "postgres://h/d", "BETA_MAX_TENANTS": "0"}, wantErr: "BETA_MAX_TENANTS"},
		{name: "negative accounts", env: map[string]string{"DATABASE_URL": "postgres://h/d", "BETA_MAX_GAME_ACCOUNTS_PER_TENANT": "-1"}, wantErr: "BETA_MAX_GAME_ACCOUNTS_PER_TENANT"},
		{name: "non-postgres url", env: map[string]string{"DATABASE_URL": "mysql://h/d"}, wantErr: "DATABASE_URL"},
		{name: "unparseable url", env: map[string]string{"DATABASE_URL": "postgres://h:notaport/d"}, wantErr: "DATABASE_URL"},
		{name: "bad listen addr", env: map[string]string{"DATABASE_URL": "postgres://h/d", "LISTEN_ADDR": "nocolon"}, wantErr: "LISTEN_ADDR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load(env(tc.env))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tc.check(t, c)
		})
	}
}

// The error must not echo the password back (logs are not a secret store).
func TestLoadErrorOmitsPassword(t *testing.T) {
	_, err := Load(env(map[string]string{"DATABASE_URL": "postgres://u:hunter2@h:notaport/d"}))
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("error leaks password: %v", err)
	}
}

func TestLoadHostedSettings(t *testing.T) {
	base := map[string]string{"DATABASE_URL": "postgres://u:p@h/d"}
	with := func(kv map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range kv {
			m[k] = v
		}
		return m
	}
	cases := []struct {
		name    string
		env     map[string]string
		wantErr string
		check   func(t *testing.T, c Config)
	}{
		{"defaults", with(nil), "", func(t *testing.T, c Config) {
			if c.AppBaseURL != "http://127.0.0.1:8090" || len(c.TrustedProxies) != 0 {
				t.Errorf("AppBaseURL=%q proxies=%v", c.AppBaseURL, c.TrustedProxies)
			}
		}},
		{"https base url", with(map[string]string{"APP_BASE_URL": "https://app.sanholo.space/"}), "", func(t *testing.T, c Config) {
			if c.AppBaseURL != "https://app.sanholo.space" {
				t.Errorf("AppBaseURL=%q", c.AppBaseURL)
			}
		}},
		{"http loopback ok", with(map[string]string{"APP_BASE_URL": "http://localhost:8090"}), "", nil},
		{"http non-loopback refused", with(map[string]string{"APP_BASE_URL": "http://app.sanholo.space"}), "APP_BASE_URL", nil},
		{"base url with path refused", with(map[string]string{"APP_BASE_URL": "https://app.sanholo.space/x"}), "APP_BASE_URL", nil},
		{"proxies", with(map[string]string{"TRUSTED_PROXIES": "10.0.0.0/8, 192.0.2.1,2001:db8::/32"}), "", func(t *testing.T, c Config) {
			if len(c.TrustedProxies) != 3 || c.TrustedProxies[1].String() != "192.0.2.1/32" {
				t.Errorf("proxies=%v", c.TrustedProxies)
			}
		}},
		{"bad proxy", with(map[string]string{"TRUSTED_PROXIES": "10.0.0.0/8,nope"}), "TRUSTED_PROXIES", nil},
		{"open sign-up refused", with(map[string]string{"BETA_INVITE_ONLY": "false"}), "BETA_INVITE_ONLY", nil},
		{"invite only explicit", with(map[string]string{"BETA_INVITE_ONLY": "true"}), "", nil},
		{"no smtp host in development", with(nil), "", func(t *testing.T, c Config) {
			if c.SMTP.Host != "" || c.Production {
				t.Errorf("SMTP=%+v production=%v", c.SMTP, c.Production)
			}
		}},
		{"production needs smtp host", with(map[string]string{"APP_ENV": "production"}), "SMTP_HOST", nil},
		{"unknown app env", with(map[string]string{"APP_ENV": "prod"}), "APP_ENV", nil},
		{"smtp defaults", with(map[string]string{"SMTP_HOST": "send.ahasend.com"}), "", func(t *testing.T, c Config) {
			if c.SMTP.Port != "587" || c.SMTP.From != "no-reply@send.ahasend.com" {
				t.Errorf("SMTP=%+v", c.SMTP)
			}
		}},
		{"smtp complete in production", with(map[string]string{
			"APP_ENV": "production", "SMTP_HOST": "send.ahasend.com", "SMTP_PORT": "465",
			"SMTP_USERNAME": "u", "SMTP_PASSWORD": "pw", "SMTP_FROM": "SanHolo <no-reply@sanholo.space>",
		}), "", func(t *testing.T, c Config) {
			if !c.Production || c.SMTP.Port != "465" || c.SMTP.Username != "u" || c.SMTP.Password != "pw" {
				t.Errorf("SMTP=%+v", c.SMTP)
			}
		}},
		{"username needs password", with(map[string]string{"SMTP_HOST": "h", "SMTP_USERNAME": "u"}), "SMTP_PASSWORD", nil},
		{"bad port", with(map[string]string{"SMTP_HOST": "h", "SMTP_PORT": "x"}), "SMTP_PORT", nil},
		{"line break in from", with(map[string]string{"SMTP_HOST": "h", "SMTP_FROM": "a@b.c\r\nBcc: x@y.z"}), "SMTP_FROM", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load(env(tc.env))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				if strings.Contains(err.Error(), "pw") {
					t.Fatalf("error leaks the password: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.check != nil {
				tc.check(t, c)
			}
		})
	}
}
