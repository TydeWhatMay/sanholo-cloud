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
			name: "game api url defaults to the real game",
			env:  map[string]string{"DATABASE_URL": "postgres://u:p@h/d"},
			check: func(t *testing.T, c Config) {
				if c.GameURL != "https://api.replicant.space" {
					t.Errorf("GameURL = %q", c.GameURL)
				}
			},
		},
		{
			name: "game api url set",
			env:  map[string]string{"DATABASE_URL": "postgres://u:p@h/d", "GAME_API_URL": "http://127.0.0.1:9000/"},
			check: func(t *testing.T, c Config) {
				if c.GameURL != "http://127.0.0.1:9000" {
					t.Errorf("GameURL = %q", c.GameURL)
				}
			},
		},
		{
			name:    "game api url refuses plain http to a real host",
			env:     map[string]string{"DATABASE_URL": "postgres://u:p@h/d", "GAME_API_URL": "http://api.replicant.space"},
			wantErr: "GAME_API_URL",
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
				"BETA_INVITE_ONLY": "true", "BETA_MAX_TENANTS": "5", "BETA_MAX_GAME_ACCOUNTS_PER_TENANT": "1",
			},
			check: func(t *testing.T, c Config) {
				if c.ListenAddr != "0.0.0.0:9000" || !c.Beta.InviteOnly || c.Beta.MaxTenants != 5 || c.Beta.MaxGameAccountsPerTenant != 1 {
					t.Errorf("config = %+v", c)
				}
			},
		},
		{name: "bad bool", env: map[string]string{"DATABASE_URL": "postgres://h/d", "BETA_INVITE_ONLY": "maybe"}, wantErr: "BETA_INVITE_ONLY"},
		{name: "bad int", env: map[string]string{"DATABASE_URL": "postgres://h/d", "BETA_MAX_TENANTS": "many"}, wantErr: "BETA_MAX_TENANTS"},
		{name: "zero tenants", env: map[string]string{"DATABASE_URL": "postgres://h/d", "BETA_MAX_TENANTS": "0"}, wantErr: "BETA_MAX_TENANTS"},
		{name: "more than one game account", env: map[string]string{"DATABASE_URL": "postgres://h/d", "BETA_MAX_GAME_ACCOUNTS_PER_TENANT": "2"}, wantErr: "only 1 is supported"},
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
	// withKeys returns m with the key/value pairs applied (an empty value
	// removes the key).
	withKeys := func(m map[string]string, kv ...string) map[string]string {
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	// prodEnv is a complete production environment with one key replaced
	// (an empty value removes it).
	vPub, vPriv := vapidPair(t)
	prodEnv := func(key, val string) map[string]string {
		m := map[string]string{
			"VAPID_PUBLIC_KEY": vPub, "VAPID_PRIVATE_KEY": vPriv, "VAPID_SUBJECT": "mailto:ops@tydewhatmay.eu",
			"APP_ENV": "production", "SMTP_HOST": "send.ahasend.com", "SMTP_USERNAME": "u", "SMTP_PASSWORD": "pw",
			"APP_BASE_URL": "https://app.sanholo.space", "TRUSTED_PROXIES": "10.0.0.0/8",
			"QM_ADDR": "https://qm.sanholo.space", "QM_ROLE_ID": "role-1", "QM_SECRET_ID_FILE": "/run/secrets/qm-secret-id",
			"NOMAD_ADDR": "https://nomad.sanholo.space:4646", "NOMAD_TOKEN": "tok", "SANHOLO_IMAGE_VERSION": "0.7.1",
			"PROXY_SIGNING_KEY": "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI=",
		}
		if key != "" {
			m[key] = val
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
			"APP_BASE_URL": "https://app.sanholo.space", "WEBAUTHN_RP_ID": "app.sanholo.space",
			"TRUSTED_PROXIES": "10.0.0.0/8", "QM_ADDR": "https://qm.sanholo.space", "QM_ROLE_ID": "r", "QM_SECRET_ID": "s",
			"NOMAD_ADDR": "https://nomad.sanholo.space:4646", "NOMAD_TOKEN": "t", "SANHOLO_IMAGE_VERSION": "0.7.1",
			"PROXY_SIGNING_KEY": "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI=",
			"VAPID_PUBLIC_KEY":  vPub, "VAPID_PRIVATE_KEY": vPriv, "VAPID_SUBJECT": "mailto:ops@tydewhatmay.eu",
		}), "", func(t *testing.T, c Config) {
			if !c.Production || c.SMTP.Port != "465" || c.SMTP.Username != "u" || c.SMTP.Password != "pw" {
				t.Errorf("SMTP=%+v", c.SMTP)
			}
		}},
		{"production needs trusted proxies", with(prodEnv("TRUSTED_PROXIES", "")), "TRUSTED_PROXIES", nil},
		{"production needs explicit base url", with(prodEnv("APP_BASE_URL", "")), "APP_BASE_URL", nil},
		{"production refuses http base url", with(prodEnv("APP_BASE_URL", "http://localhost:8090")), "APP_BASE_URL", nil},
		{"production needs smtp username", with(prodEnv("SMTP_USERNAME", "")), "SMTP_USERNAME", nil},
		{"production complete", with(prodEnv("", "")), "", func(t *testing.T, c Config) {
			if c.QM != (QM{Addr: "https://qm.sanholo.space", RoleID: "role-1", SecretIDFile: "/run/secrets/qm-secret-id"}) {
				t.Errorf("QM = %+v", c.QM)
			}
		}},
		{"production needs qm", with(prodEnv("QM_ADDR", "")), "QM_ADDR", nil},
		{"production refuses http qm", with(prodEnv("QM_ADDR", "http://qm.sanholo.space")), "QM_ADDR", nil},
		{"qm disabled in development", with(nil), "", func(t *testing.T, c Config) {
			if c.QM != (QM{}) {
				t.Errorf("QM = %+v", c.QM)
			}
		}},
		{"qm http loopback in development", with(map[string]string{"QM_ADDR": "http://127.0.0.1:8222/", "QM_TOKEN": "t"}), "", func(t *testing.T, c Config) {
			if c.QM.Addr != "http://127.0.0.1:8222" || c.QM.Token != "t" {
				t.Errorf("QM = %+v", c.QM)
			}
		}},
		{"qm http real host refused", with(map[string]string{"QM_ADDR": "http://qm.example.org", "QM_TOKEN": "t"}), "QM_ADDR", nil},
		{"qm addr needs a token", with(map[string]string{"QM_ADDR": "https://qm.example.org"}), "QM_TOKEN", nil},
		{"qm token and file both", with(map[string]string{"QM_ADDR": "https://qm.example.org", "QM_TOKEN": "t", "QM_TOKEN_FILE": "/x"}), "exactly one", nil},
		{"qm token without addr", with(map[string]string{"QM_TOKEN": "t"}), "QM_ADDR", nil},
		{"qm token file without addr", with(map[string]string{"QM_TOKEN_FILE": "/x"}), "QM_ADDR", nil},
		{"qm approle in development", with(map[string]string{"QM_ADDR": "https://qm.example.org", "QM_ROLE_ID": "r", "QM_SECRET_ID": "s"}), "", func(t *testing.T, c Config) {
			if c.QM != (QM{Addr: "https://qm.example.org", RoleID: "r", SecretID: "s"}) {
				t.Errorf("QM = %+v", c.QM)
			}
		}},
		{"qm approle secret id file", with(map[string]string{"QM_ADDR": "https://qm.example.org", "QM_ROLE_ID": "r", "QM_SECRET_ID_FILE": "/x"}), "", nil},
		{"qm role without secret id", with(map[string]string{"QM_ADDR": "https://qm.example.org", "QM_ROLE_ID": "r"}), "QM_SECRET_ID", nil},
		{"qm secret id without role", with(map[string]string{"QM_ADDR": "https://qm.example.org", "QM_SECRET_ID": "s"}), "QM_ROLE_ID", nil},
		{"qm secret id and file both", with(map[string]string{"QM_ADDR": "https://qm.example.org", "QM_ROLE_ID": "r", "QM_SECRET_ID": "s", "QM_SECRET_ID_FILE": "/x"}), "exactly one", nil},
		{"qm approle and token both", with(map[string]string{"QM_ADDR": "https://qm.example.org", "QM_ROLE_ID": "r", "QM_SECRET_ID": "s", "QM_TOKEN": "t"}), "not both", nil},
		{"qm role id without addr", with(map[string]string{"QM_ROLE_ID": "r", "QM_SECRET_ID": "s"}), "QM_ADDR", nil},
		{"production needs a role id", with(prodEnv("QM_ROLE_ID", "")), "QM_ROLE_ID", nil},
		{"production refuses static token", with(withKeys(prodEnv("", ""), "QM_ROLE_ID", "", "QM_SECRET_ID_FILE", "", "QM_TOKEN", "t")), "refused", nil},
		{"production refuses static token file", with(withKeys(prodEnv("", ""), "QM_TOKEN_FILE", "/x")), "refused", nil},
		{"production needs a secret id", with(prodEnv("QM_SECRET_ID_FILE", "")), "QM_SECRET_ID", nil},
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

func TestWebAuthnConfig(t *testing.T) {
	base := map[string]string{"DATABASE_URL": "postgres://u@h/d"}
	with := func(kv map[string]string) func(string) string {
		return func(k string) string {
			if v, ok := kv[k]; ok {
				return v
			}
			return base[k]
		}
	}
	cases := []struct {
		name    string
		env     map[string]string
		wantID  string
		wantErr bool
	}{
		{"bare IP default is off", map[string]string{}, "", false},
		{"derived from base url", map[string]string{"APP_BASE_URL": "https://app.sanholo.space"}, "app.sanholo.space", false},
		{"explicit parent domain", map[string]string{"APP_BASE_URL": "https://app.sanholo.space", "WEBAUTHN_RP_ID": "sanholo.space"}, "sanholo.space", false},
		{"localhost over http", map[string]string{"APP_BASE_URL": "http://localhost:8090"}, "localhost", false},
		{"http on a real host", map[string]string{"WEBAUTHN_RP_ID": "example.org", "WEBAUTHN_RP_ORIGIN": "http://example.org"}, "", true},
		{"origin outside the rp id", map[string]string{"WEBAUTHN_RP_ID": "example.org", "WEBAUTHN_RP_ORIGIN": "https://example.com"}, "", true},
		{"suffix is not a subdomain", map[string]string{"WEBAUTHN_RP_ID": "example.org", "WEBAUTHN_RP_ORIGIN": "https://badexample.org"}, "", true},
		{"origin with a path", map[string]string{"WEBAUTHN_RP_ID": "example.org", "WEBAUTHN_RP_ORIGIN": "https://example.org/x"}, "", true},
		{"production without rp id", map[string]string{"APP_ENV": "production", "SMTP_HOST": "m.example.org"}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load(with(tc.env))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			if !tc.wantErr && c.WebAuthn.RPID != tc.wantID {
				t.Fatalf("RPID = %q, want %q", c.WebAuthn.RPID, tc.wantID)
			}
		})
	}
}
