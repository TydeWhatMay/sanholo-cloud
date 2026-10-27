package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// Internal is the platform listener (US-023): Prometheus scrapes /metrics and
// Alertmanager posts to /internal/operator-alerts on it. It is a second
// address so the edge never routes to it, and each endpoint also needs its
// own bearer token. The tokens are secrets: never log or print a Config.
type Internal struct {
	// Addr is INTERNAL_LISTEN_ADDR, a private address. Empty turns the
	// listener off.
	Addr string
	// MetricsToken is the bearer Prometheus sends (METRICS_TOKEN_FILE).
	MetricsToken string
	// AlertsToken is the bearer Alertmanager sends (ALERTMANAGER_TOKEN_FILE).
	AlertsToken string
}

// Enabled reports whether the platform listener runs.
func (i Internal) Enabled() bool { return i.Addr != "" }

// minTokenLen is the shortest bearer accepted: 32 characters is 192 bits of
// base64, which `openssl rand -base64 32` comfortably exceeds.
const minTokenLen = 32

// internalSettings reads INTERNAL_LISTEN_ADDR, METRICS_TOKEN_FILE and
// ALERTMANAGER_TOKEN_FILE. The tokens come from files only (no inline
// variable), so they never appear in a process environment listing. In
// production all three are required: operator alerting is a launch gate.
// Errors never include a token.
func internalSettings(getenv func(string) string, listenAddr string, production bool) (Internal, error) {
	addr := getenv("INTERNAL_LISTEN_ADDR")
	mFile, aFile := getenv("METRICS_TOKEN_FILE"), getenv("ALERTMANAGER_TOKEN_FILE")
	if addr == "" {
		if production {
			return Internal{}, errors.New("INTERNAL_LISTEN_ADDR is required when APP_ENV=production: Prometheus and Alertmanager reach the control plane there")
		}
		if mFile != "" || aFile != "" {
			return Internal{}, errors.New("METRICS_TOKEN_FILE or ALERTMANAGER_TOKEN_FILE is set but INTERNAL_LISTEN_ADDR is not")
		}
		return Internal{}, nil
	}
	var errs []error
	if _, _, err := net.SplitHostPort(addr); err != nil {
		errs = append(errs, fmt.Errorf("INTERNAL_LISTEN_ADDR %q: %w", addr, err))
	}
	if addr == listenAddr {
		errs = append(errs, errors.New("INTERNAL_LISTEN_ADDR must differ from LISTEN_ADDR: the edge must not reach the platform endpoints"))
	}
	in := Internal{Addr: addr}
	if mFile == "" && aFile == "" {
		errs = append(errs, errors.New("INTERNAL_LISTEN_ADDR is set: set METRICS_TOKEN_FILE, ALERTMANAGER_TOKEN_FILE or both"))
	}
	if production && (mFile == "" || aFile == "") {
		errs = append(errs, errors.New("METRICS_TOKEN_FILE and ALERTMANAGER_TOKEN_FILE are required when APP_ENV=production"))
	}
	var err error
	if mFile != "" {
		if in.MetricsToken, err = readTokenFile("METRICS_TOKEN_FILE", mFile); err != nil {
			errs = append(errs, err)
		}
	}
	if aFile != "" {
		if in.AlertsToken, err = readTokenFile("ALERTMANAGER_TOKEN_FILE", aFile); err != nil {
			errs = append(errs, err)
		}
	}
	if in.MetricsToken != "" && in.MetricsToken == in.AlertsToken {
		// One token per caller: the scrape token must not be able to push.
		errs = append(errs, errors.New("METRICS_TOKEN_FILE and ALERTMANAGER_TOKEN_FILE must hold different tokens"))
	}
	return in, errors.Join(errs...)
}

// readTokenFile reads a bearer token from a file. Surrounding white space is
// dropped; inner white space or a short token is refused.
func readTokenFile(name, path string) (string, error) {
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("%s: cannot read the file", name)
	}
	v := strings.TrimSpace(string(b))
	if strings.ContainsAny(v, " \t\r\n") {
		return "", fmt.Errorf("%s: the token contains white space", name)
	}
	if len(v) < minTokenLen {
		return "", fmt.Errorf("%s: want a token of at least %d characters", name, minTokenLen)
	}
	return v, nil
}
