// Package config reads the control plane's settings from the environment.
// There are no flags for them: the operator runs the service under
// `qm db creds -role <role> -env -- sanholo-cloud`, which injects the
// Postgres credentials as environment, and secrets must not appear in a
// process listing.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// Config is the validated process configuration.
type Config struct {
	// DatabaseURL is a postgres:// URL, from DATABASE_URL or built from the
	// standard PG* variables.
	DatabaseURL string
	ListenAddr  string
	Beta        Beta
}

// Beta holds the closed-beta limits.
type Beta struct {
	InviteOnly               bool
	MaxTenants               int
	MaxGameAccountsPerTenant int
}

const defaultListenAddr = "127.0.0.1:8090"

// Load reads and validates the configuration. getenv is os.Getenv in
// production; tests pass a map lookup.
func Load(getenv func(string) string) (Config, error) {
	c := Config{
		ListenAddr: defaultListenAddr,
		Beta:       Beta{MaxTenants: 20, MaxGameAccountsPerTenant: 1},
	}
	var errs []error

	dsn, err := databaseURL(getenv)
	if err != nil {
		errs = append(errs, err)
	}
	c.DatabaseURL = dsn

	if v := getenv("LISTEN_ADDR"); v != "" {
		c.ListenAddr = v
	}
	if _, _, err := net.SplitHostPort(c.ListenAddr); err != nil {
		errs = append(errs, fmt.Errorf("LISTEN_ADDR %q: %w", c.ListenAddr, err))
	}

	if v := getenv("BETA_INVITE_ONLY"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("BETA_INVITE_ONLY %q: not a boolean", v))
		}
		c.Beta.InviteOnly = b
	}
	if err := positiveInt(getenv, "BETA_MAX_TENANTS", &c.Beta.MaxTenants); err != nil {
		errs = append(errs, err)
	}
	if err := positiveInt(getenv, "BETA_MAX_GAME_ACCOUNTS_PER_TENANT", &c.Beta.MaxGameAccountsPerTenant); err != nil {
		errs = append(errs, err)
	}
	return c, errors.Join(errs...)
}

func positiveInt(getenv func(string) string, name string, dst *int) error {
	v := getenv(name)
	if v == "" {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return fmt.Errorf("%s %q: want a positive integer", name, v)
	}
	*dst = n
	return nil
}

// databaseURL prefers DATABASE_URL and falls back to PGHOST, PGPORT, PGUSER,
// PGPASSWORD, PGDATABASE and PGSSLMODE. Errors never include the URL, which
// may carry a password.
func databaseURL(getenv func(string) string) (string, error) {
	if v := getenv("DATABASE_URL"); v != "" {
		u, err := url.Parse(v)
		if err != nil {
			// url.Error quotes the input; report only that it is malformed.
			return "", errors.New("DATABASE_URL: not a valid URL")
		}
		if u.Scheme != "postgres" && u.Scheme != "postgresql" {
			return "", errors.New("DATABASE_URL: scheme must be postgres or postgresql")
		}
		return v, nil
	}
	host, user, name := getenv("PGHOST"), getenv("PGUSER"), getenv("PGDATABASE")
	if host == "" && user == "" && name == "" {
		return "", errors.New("DATABASE_URL (or PGHOST, PGUSER and PGDATABASE) is required")
	}
	var missing []string
	for _, p := range [][2]string{{"PGHOST", host}, {"PGUSER", user}, {"PGDATABASE", name}} {
		if p[1] == "" {
			missing = append(missing, p[0])
		}
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("database: %s not set", strings.Join(missing, ", "))
	}
	u := url.URL{Scheme: "postgres", Host: host, Path: "/" + name}
	if pw := getenv("PGPASSWORD"); pw != "" {
		u.User = url.UserPassword(user, pw)
	} else {
		u.User = url.User(user)
	}
	if port := getenv("PGPORT"); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return "", fmt.Errorf("PGPORT %q: not a port", port)
		}
		u.Host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	}
	if mode := getenv("PGSSLMODE"); mode != "" {
		u.RawQuery = url.Values{"sslmode": {mode}}.Encode()
	}
	return u.String(), nil
}
