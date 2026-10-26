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
				if c.Beta.InviteOnly || c.Beta.MaxTenants != 20 || c.Beta.MaxGameAccountsPerTenant != 1 {
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
