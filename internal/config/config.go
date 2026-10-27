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
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"github.com/lhaig/sanholo-cloud/internal/game"
	"github.com/lhaig/sanholo-cloud/internal/secrets"
)

// Config is the validated process configuration.
type Config struct {
	// DatabaseURL is a postgres:// URL, from DATABASE_URL or built from the
	// standard PG* variables.
	DatabaseURL string
	ListenAddr  string
	Beta        Beta
	// AppBaseURL is the public origin of the control plane, without a
	// trailing slash. Sign-in links are built from it.
	AppBaseURL string
	// TrustedProxies are the peers whose X-Forwarded-For is believed.
	TrustedProxies []netip.Prefix
	SMTP           SMTP
	// QM is the control plane's route to tenant secrets. Empty Addr means
	// secrets are disabled (development only; production requires it).
	QM QM
	// GameURL is the Replicant Space API origin (GAME_API_URL), without a
	// trailing slash.
	GameURL string
	// WebAuthn holds the passkey relying-party settings. Enabled is false
	// when no usable relying party exists (development on a bare IP).
	WebAuthn WebAuthn
	// Production is APP_ENV=production: mail, an https base URL and trusted
	// proxies must be configured (the default base URL is http, so an unset
	// APP_BASE_URL is refused too).
	Production bool
}

// SMTP holds the mail settings. Password is a secret: never log or print a
// Config. An empty Host means log-only (nothing is sent).
type SMTP struct {
	Host     string
	Port     string
	Username string
	Password string
	From     string
}

// QM holds the qm connection. Token is a secret: never log or print a
// Config. At most one of Token and TokenFile is set; TokenFile is re-read
// on every request.
type QM struct {
	Addr      string
	Token     string
	TokenFile string
}

// WebAuthn is the relying party for passkeys. RPID is the registrable domain
// the credentials are bound to; RPOrigin is the exact origin the browser
// reports. Credentials made under one RPID never work under another.
type WebAuthn struct {
	RPID     string
	RPOrigin string
	RPName   string
}

// Enabled reports whether passkeys can be offered.
func (w WebAuthn) Enabled() bool { return w.RPID != "" }

// Enabled reports whether mail will really be sent.
func (s SMTP) Enabled() bool { return s.Host != "" }

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
		AppBaseURL: "http://127.0.0.1:8090",
		Beta:       Beta{InviteOnly: true, MaxTenants: 20, MaxGameAccountsPerTenant: 1},
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
		if err == nil && !b {
			// Open sign-up would create an account on an address nobody
			// has proved; verified sign-up is not built yet.
			errs = append(errs, errors.New("BETA_INVITE_ONLY=false is not supported: open sign-up needs email verification, which does not exist yet"))
		}
	}
	if err := positiveInt(getenv, "BETA_MAX_TENANTS", &c.Beta.MaxTenants); err != nil {
		errs = append(errs, err)
	}
	if err := positiveInt(getenv, "BETA_MAX_GAME_ACCOUNTS_PER_TENANT", &c.Beta.MaxGameAccountsPerTenant); err != nil {
		errs = append(errs, err)
	} else if c.Beta.MaxGameAccountsPerTenant != 1 {
		// qm holds one game credential per tenant (secrets layout).
		errs = append(errs, errors.New("BETA_MAX_GAME_ACCOUNTS_PER_TENANT: only 1 is supported"))
	}
	if v := getenv("APP_BASE_URL"); v != "" {
		base, err := baseURL(v)
		if err != nil {
			errs = append(errs, err)
		}
		c.AppBaseURL = base
	}
	proxies, err := prefixes("TRUSTED_PROXIES", getenv("TRUSTED_PROXIES"))
	if err != nil {
		errs = append(errs, err)
	}
	c.TrustedProxies = proxies

	switch getenv("APP_ENV") {
	case "", "development":
	case "production":
		c.Production = true
	default:
		errs = append(errs, fmt.Errorf("APP_ENV %q: want production or development", getenv("APP_ENV")))
	}
	c.SMTP = SMTP{
		Host: getenv("SMTP_HOST"), Port: getenv("SMTP_PORT"), Username: getenv("SMTP_USERNAME"),
		Password: getenv("SMTP_PASSWORD"), From: getenv("SMTP_FROM"),
	}
	switch {
	case c.SMTP.Host == "" && c.Production:
		errs = append(errs, errors.New("SMTP_HOST is required when APP_ENV=production"))
	case c.SMTP.Host != "":
		if c.SMTP.Port == "" {
			c.SMTP.Port = "587"
		}
		if n, err := strconv.Atoi(c.SMTP.Port); err != nil || n < 1 || n > 65535 {
			errs = append(errs, fmt.Errorf("SMTP_PORT %q: not a port", c.SMTP.Port))
		}
		if c.SMTP.From == "" {
			c.SMTP.From = "no-reply@" + c.SMTP.Host
		}
		if strings.ContainsAny(c.SMTP.From, "\r\n") {
			errs = append(errs, errors.New("SMTP_FROM: contains a line break"))
		}
		if c.SMTP.Username != "" && c.SMTP.Password == "" {
			errs = append(errs, errors.New("SMTP_USERNAME is set but SMTP_PASSWORD is not"))
		}
	}
	qm, err := qmSettings(getenv)
	if err != nil {
		errs = append(errs, err)
	}
	c.QM = qm
	c.GameURL = game.DefaultURL
	if v := getenv("GAME_API_URL"); v != "" {
		u, err := game.ParseURL(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("GAME_API_URL: %w", err))
		} else {
			c.GameURL = u
		}
	}
	wa, err := webAuthn(getenv, c.AppBaseURL)
	if err != nil {
		errs = append(errs, err)
	}
	c.WebAuthn = wa
	if c.Production {
		if len(c.TrustedProxies) == 0 {
			errs = append(errs, errors.New("TRUSTED_PROXIES is required when APP_ENV=production: without it every client shares the edge's rate-limit key"))
		}
		if !strings.HasPrefix(c.AppBaseURL, "https://") {
			errs = append(errs, errors.New("APP_BASE_URL must be set to an https origin when APP_ENV=production"))
		}
		if c.SMTP.Host != "" && c.SMTP.Username == "" {
			errs = append(errs, errors.New("SMTP_USERNAME is required when APP_ENV=production: mail must be authenticated"))
		}
	}
	if c.Production && c.QM.Addr == "" {
		errs = append(errs, errors.New("QM_ADDR is required when APP_ENV=production: customer game tokens are stored in qm"))
	}
	if c.Production && !wa.Enabled() {
		errs = append(errs, errors.New("WEBAUTHN_RP_ID is required when APP_ENV=production: operators sign in with a passkey"))
	}
	return c, errors.Join(errs...)
}

// qmSettings reads QM_ADDR, QM_TOKEN and QM_TOKEN_FILE. With none set,
// secrets are disabled. Errors never include the token.
func qmSettings(getenv func(string) string) (QM, error) {
	q := QM{Addr: getenv("QM_ADDR"), Token: getenv("QM_TOKEN"), TokenFile: getenv("QM_TOKEN_FILE")}
	if q.Addr == "" {
		if q.Token != "" || q.TokenFile != "" {
			return QM{}, errors.New("QM_TOKEN or QM_TOKEN_FILE is set but QM_ADDR is not")
		}
		return q, nil
	}
	addr, err := secrets.ParseAddr(q.Addr)
	if err != nil {
		return QM{}, fmt.Errorf("QM_ADDR: %w", err)
	}
	q.Addr = addr
	if (q.Token == "") == (q.TokenFile == "") {
		return QM{}, errors.New("QM_ADDR is set: set exactly one of QM_TOKEN and QM_TOKEN_FILE")
	}
	return q, nil
}

// webAuthn reads WEBAUTHN_RP_ID, WEBAUTHN_RP_ORIGIN and WEBAUTHN_RP_NAME.
// Unset, the RP ID is the host of APP_BASE_URL and the origin is
// APP_BASE_URL; a bare IP host cannot be an RP ID, so passkeys stay off.
// The origin is https, or http on localhost only, and its host must be the
// RP ID or a subdomain of it (the WebAuthn rule).
func webAuthn(getenv func(string) string, appBase string) (WebAuthn, error) {
	w := WebAuthn{RPID: getenv("WEBAUTHN_RP_ID"), RPOrigin: getenv("WEBAUTHN_RP_ORIGIN"), RPName: getenv("WEBAUTHN_RP_NAME")}
	if w.RPName == "" {
		w.RPName = "SanHolo"
	}
	if w.RPOrigin == "" {
		w.RPOrigin = appBase
	}
	if w.RPID == "" {
		u, err := url.Parse(w.RPOrigin)
		if err != nil || u.Hostname() == "" {
			return WebAuthn{}, errors.New("WEBAUTHN_RP_ORIGIN: not an origin")
		}
		if _, err := netip.ParseAddr(u.Hostname()); err == nil {
			return WebAuthn{RPName: w.RPName}, nil
		}
		w.RPID = u.Hostname()
	}
	if strings.ContainsAny(w.RPID, "/:@ ") || w.RPID != strings.ToLower(w.RPID) {
		return WebAuthn{}, fmt.Errorf("WEBAUTHN_RP_ID %q: want a lower-case domain name", w.RPID)
	}
	u, err := url.Parse(w.RPOrigin)
	if err != nil || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
		return WebAuthn{}, errors.New("WEBAUTHN_RP_ORIGIN: want an origin such as https://app.example.org")
	}
	host := u.Hostname()
	switch u.Scheme {
	case "https":
	case "http":
		if host != "localhost" && !strings.HasSuffix(host, ".localhost") {
			return WebAuthn{}, errors.New("WEBAUTHN_RP_ORIGIN: http is allowed for localhost only")
		}
	default:
		return WebAuthn{}, errors.New("WEBAUTHN_RP_ORIGIN: scheme must be https")
	}
	if host != w.RPID && !strings.HasSuffix(host, "."+w.RPID) {
		return WebAuthn{}, fmt.Errorf("WEBAUTHN_RP_ID %q: the origin host %q is not that domain or a subdomain of it", w.RPID, host)
	}
	w.RPOrigin = u.Scheme + "://" + u.Host
	return w, nil
}

// baseURL accepts an origin only: https, or http on a loopback host (local
// development). Sign-in links carry a bearer token, so they never go out
// over plain http to a real host.
func baseURL(v string) (string, error) {
	u, err := url.Parse(v)
	if err != nil || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
		return "", errors.New("APP_BASE_URL: want an origin such as https://app.example.org")
	}
	switch u.Scheme {
	case "https":
	case "http":
		h := u.Hostname()
		if ip, err := netip.ParseAddr(h); h != "localhost" && (err != nil || !ip.IsLoopback()) {
			return "", errors.New("APP_BASE_URL: http is allowed for loopback hosts only")
		}
	default:
		return "", errors.New("APP_BASE_URL: scheme must be https")
	}
	return u.Scheme + "://" + u.Host, nil
}

// prefixes parses a comma-separated list of CIDRs or bare addresses.
func prefixes(name, v string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		p, err := netip.ParsePrefix(part)
		if err != nil {
			a, aerr := netip.ParseAddr(part)
			if aerr != nil {
				return nil, fmt.Errorf("%s %q: not a CIDR or address", name, part)
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		out = append(out, p.Masked())
	}
	return out, nil
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
