// Package secrets is the control plane's only route to tenant secrets in
// qm. The control plane's qm principal is write-only, and this package is
// shaped to match: it can store a tenant's game credentials, ask whether
// they exist (a version number, never a value) and purge a tenant; it has
// no method that returns a secret value and no way to list or enumerate.
// TestAPISurface fails if one is added.
//
// Deletion has exactly one door, PurgeTenant. A token rotation is a plain
// new write (Put): qm keeps 16 versions and prunes the oldest. All wire
// detail is in wire.go.
package secrets

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lhaig/sanholo-cloud/internal/tenant"
)

var (
	// ErrSecretsUnavailable: no qm is configured (development). Nothing was
	// sent.
	ErrSecretsUnavailable = errors.New("secrets unavailable: qm is not configured")
	// ErrInvalidTenant: the tenant id fails the tenant rule. Nothing was sent.
	ErrInvalidTenant = errors.New("invalid tenant id")
	// ErrInvalidCredentials: the credentials to store are empty or malformed.
	ErrInvalidCredentials = errors.New("invalid credentials")
	// ErrDenied: qm refused the control plane's identity or policy (403,
	// also after one fresh login).
	ErrDenied = errors.New("qm denied the request")
	// ErrUnavailable: qm is unreachable, rate limiting or failing (network
	// error, timeout, 429, 5xx). Retrying later may work.
	ErrUnavailable = errors.New("qm unavailable")
	// ErrSealed: qm is sealed (503 vault_sealed). It is also an
	// ErrUnavailable: an operator must unseal it.
	ErrSealed = fmt.Errorf("%w: sealed", ErrUnavailable)
	// ErrProtocol: qm answered in a way this package does not understand.
	ErrProtocol = errors.New("unexpected qm response")
)

// Error carries the failed operation and the HTTP status (0 when no response
// arrived). It never holds a request or response body.
type Error struct {
	Op     string
	Status int
	Err    error
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("secrets: %s: %v (status %d)", e.Op, e.Err, e.Status)
	}
	return fmt.Sprintf("secrets: %s: %v", e.Op, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// Credentials is what a tenant's instance reads from qm: the same two
// fields the client's qmcreds package decodes.
type Credentials struct {
	APIToken      string
	ReplicantCode string
}

func (c Credentials) valid() bool {
	ok := func(s string) bool { return s != "" && len(s) <= 4096 && !strings.ContainsAny(s, "\x00\r\n") }
	return ok(c.APIToken) && ok(c.ReplicantCode)
}

// Config configures the client. With no Addr the client is disabled. With
// an Addr, set either the AppRole identity (RoleID with exactly one of
// SecretID and SecretIDFile), which is the production mode, or a static
// client token (exactly one of Token and TokenFile), for development and
// tests. Never both.
type Config struct {
	// Addr is the qm origin (https, or http on loopback).
	Addr string

	// RoleID and SecretID or SecretIDFile are the control plane's AppRole.
	// The file is re-read on every login.
	RoleID       string
	SecretID     string
	SecretIDFile string

	// Token is a static qm client token; TokenFile is a file holding it,
	// re-read on every request. Development and tests only.
	Token     string
	TokenFile string

	// Timeout bounds each request. Default 10 seconds.
	Timeout time.Duration
	// Logger receives operation records (tenant, counts, status); never a
	// secret value. Default: discard.
	Logger *slog.Logger
	// Now is the clock for token renewal. Default time.Now; tests set it.
	Now func() time.Time
}

// Client talks to qm. The zero value and a nil *Client are disabled.
type Client struct {
	base      string
	token     string
	tokenFile string
	roleID    string
	secretID  string
	secretFil string
	timeout   time.Duration
	http      *http.Client
	log       *slog.Logger
	now       func() time.Time

	// mu serialises login and renewal, and guards tok and renewAt.
	mu      sync.Mutex
	tok     string
	renewAt time.Time
}

// New validates cfg. An empty Addr (and no credentials) gives a disabled
// client whose operations return ErrSecretsUnavailable.
func New(cfg Config) (*Client, error) {
	hasToken := cfg.Token != "" || cfg.TokenFile != ""
	hasRole := cfg.RoleID != "" || cfg.SecretID != "" || cfg.SecretIDFile != ""
	if cfg.Addr == "" {
		if hasToken || hasRole {
			return nil, errors.New("secrets: qm credentials are set but no qm address")
		}
		return &Client{}, nil
	}
	base, err := ParseAddr(cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("secrets: qm address: %w", err)
	}
	switch {
	case hasToken && hasRole:
		return nil, errors.New("secrets: set either the qm AppRole or a static qm token, not both")
	case hasToken:
		if (cfg.Token == "") == (cfg.TokenFile == "") {
			return nil, errors.New("secrets: set exactly one of the qm token and the qm token file")
		}
	case hasRole:
		if cfg.RoleID == "" {
			return nil, errors.New("secrets: the qm role id is required with a secret id")
		}
		if (cfg.SecretID == "") == (cfg.SecretIDFile == "") {
			return nil, errors.New("secrets: set exactly one of the qm secret id and the qm secret id file")
		}
	default:
		return nil, errors.New("secrets: set the qm role id and secret id (or, in development, a static qm token)")
	}
	c := &Client{
		base: base, token: cfg.Token, tokenFile: cfg.TokenFile,
		roleID: cfg.RoleID, secretID: cfg.SecretID, secretFil: cfg.SecretIDFile,
		timeout: cfg.Timeout, log: cfg.Logger, now: cfg.Now,
	}
	if c.timeout <= 0 {
		c.timeout = 10 * time.Second
	}
	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.appRole() {
		if _, err := c.secretIDValue(); err != nil {
			return nil, err
		}
		if err := headerSafe("role id", cfg.RoleID); err != nil {
			return nil, err
		}
	} else if _, err := c.credential(); err != nil {
		return nil, err
	}
	c.http = &http.Client{
		Timeout: c.timeout,
		// A redirect would carry the token header to wherever it points.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return c, nil
}

// Enabled reports whether qm is configured.
func (c *Client) Enabled() bool { return c != nil && c.base != "" }

func (c *Client) appRole() bool { return c.roleID != "" }

// Ready probes qm's health endpoint (no auth, always 200). It returns nil
// only when qm is initialized and unsealed.
func (c *Client) Ready(ctx context.Context) error {
	if !c.Enabled() {
		return ErrSecretsUnavailable
	}
	_, body, err := c.do(ctx, "ready", healthCall())
	if err != nil {
		return err
	}
	initialized, sealed, err := decodeHealth(body)
	switch {
	case err != nil:
		return &Error{Op: "ready", Err: ErrProtocol}
	case sealed:
		return &Error{Op: "ready", Err: ErrSealed}
	case !initialized:
		return &Error{Op: "ready", Err: fmt.Errorf("%w: not initialized", ErrUnavailable)}
	}
	return nil
}

// Put stores a tenant's game credentials as a new version at the tenant's
// game path. Storing again is how a token is rotated: qm keeps 16 versions
// and prunes the oldest.
func (c *Client) Put(ctx context.Context, tenantID string, cr Credentials) error {
	if err := c.check(tenantID); err != nil {
		return err
	}
	if !cr.valid() {
		return &Error{Op: "put", Err: ErrInvalidCredentials}
	}
	pc, err := putCall(gamePath(tenantID), cr)
	if err != nil {
		return &Error{Op: "put", Err: ErrProtocol}
	}
	_, body, err := c.do(ctx, "put", pc)
	if err != nil {
		return err
	}
	v, err := decodePutVersion(body)
	if err != nil {
		return &Error{Op: "put", Err: ErrProtocol}
	}
	c.log.Info("secrets: stored", "tenant", tenantID, "version", v)
	return nil
}

// Version returns the current version number of the tenant's game secret, or
// 0 when there is none (never written, purged, or destroyed). It reads
// metadata only, never a value.
func (c *Client) Version(ctx context.Context, tenantID string) (int, error) {
	if err := c.check(tenantID); err != nil {
		return 0, err
	}
	status, body, err := c.do(ctx, "version", metaCall(gamePath(tenantID)))
	if err != nil {
		return 0, err
	}
	if absent(status) {
		return 0, nil
	}
	v, err := decodeCurrentVersion(body)
	if err != nil {
		return 0, &Error{Op: "version", Err: ErrProtocol}
	}
	return v, nil
}

// PurgeTenant removes everything qm holds for the tenant in one audited
// operation, then checks that the game path is gone. It is the only
// deletion path. Safe to repeat.
//
// The purge contract is final (qm Part E, ADR 0024), but the route is not
// yet deployed on the production qm. Until it is, qm refuses the purge (403,
// ErrDenied), nothing is removed and the cleanup stays pending.
func (c *Client) PurgeTenant(ctx context.Context, tenantID string) error {
	if err := c.check(tenantID); err != nil {
		return err
	}
	if err := c.purgeOnce(ctx, tenantID); err != nil {
		return err
	}
	status, _, err := c.do(ctx, "purge", metaCall(gamePath(tenantID)))
	if err != nil {
		return err
	}
	if !absent(status) {
		return &Error{Op: "purge", Err: fmt.Errorf("%w: the secret is still present after the purge", ErrProtocol)}
	}
	c.log.Info("secrets: purged", "tenant", tenantID)
	return nil
}

// absent reports a metadata read that found no path: qm answers 204 with an
// empty body. A 404 means an unknown mount or route (a misconfigured prefix,
// a qm without the route); it is ErrProtocol, never "absent", so a purge and
// its verification cannot pass against the wrong path.
func absent(status int) bool {
	return status == http.StatusNoContent
}

// purgeAttempts bounds the re-runs of a purge that ran out of qm's time
// budget; past it the pending cleanup retries later.
const purgeAttempts = 3

// purgeOnce sends the purge, re-sending it while qm reports that its time
// budget ran out part way (each re-run carries on where the last stopped).
func (c *Client) purgeOnce(ctx context.Context, tenantID string) error {
	var err error
	for range purgeAttempts {
		if _, _, err = c.do(ctx, "purge", purgeCall(tenantPrefix(tenantID))); !errors.Is(err, errPurgeIncomplete) {
			return err
		}
	}
	return err
}

func (c *Client) check(tenantID string) error {
	if !c.Enabled() {
		return ErrSecretsUnavailable
	}
	if !tenant.Valid(tenantID) {
		return &Error{Op: "validate", Err: ErrInvalidTenant}
	}
	return nil
}

// do sends one call, handling the client token. A 403 on an AppRole client
// triggers one fresh login and one retry; a second 403 is ErrDenied. Every
// error is an *Error wrapping a sentinel; none includes a request or
// response body.
func (c *Client) do(ctx context.Context, op string, k call) (int, []byte, error) {
	tok := ""
	if !k.noAuth {
		var err error
		if tok, err = c.clientToken(ctx, op); err != nil {
			return 0, nil, err
		}
	}
	status, body, err := c.send(ctx, op, k, tok)
	if err != nil {
		return status, nil, err
	}
	if status == http.StatusForbidden && !k.noAuth && c.appRole() {
		if tok, err = c.relogin(ctx, op, tok); err != nil {
			return 0, nil, err
		}
		if status, body, err = c.send(ctx, op, k, tok); err != nil {
			return status, nil, err
		}
	}
	return c.result(op, status, body)
}

func (c *Client) result(op string, status int, body []byte) (int, []byte, error) {
	if status >= 200 && status < 300 {
		return status, body, nil
	}
	c.log.Warn("secrets: qm refused", "op", op, "status", status)
	return status, nil, &Error{Op: op, Status: status, Err: statusError(status, body)}
}

// send performs one HTTP exchange and returns the status and the bounded
// body, whatever the status. Only transport failures are errors.
func (c *Client) send(ctx context.Context, op string, k call, token string) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	var rd io.Reader
	if k.body != nil {
		rd = strings.NewReader(string(k.body))
	}
	req, err := http.NewRequestWithContext(ctx, k.method, c.base+k.path, rd)
	if err != nil {
		return 0, nil, &Error{Op: op, Err: ErrProtocol}
	}
	if token != "" {
		req.Header.Set(authHeaderName, token)
	}
	if k.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		// The transport error quotes the URL; report only the kind.
		c.log.Warn("secrets: request failed", "op", op, "error", transportKind(ctx, err))
		return 0, nil, &Error{Op: op, Err: ErrUnavailable}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return resp.StatusCode, nil, &Error{Op: op, Status: resp.StatusCode, Err: ErrUnavailable}
	}
	if len(body) > maxBody {
		return resp.StatusCode, nil, &Error{Op: op, Status: resp.StatusCode, Err: fmt.Errorf("%w: response too large", ErrProtocol)}
	}
	return resp.StatusCode, body, nil
}

func transportKind(ctx context.Context, err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil:
		return "timeout"
	default:
		return "network"
	}
}

// clientToken returns the token to send. A static client reads it from its
// configuration; an AppRole client logs in on first use and renews once past
// two thirds of the lease (logging in again if the renewal fails).
func (c *Client) clientToken(ctx context.Context, op string) (string, error) {
	if !c.appRole() {
		tok, err := c.credential()
		if err != nil {
			return "", &Error{Op: op, Err: err}
		}
		return tok, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tok == "" {
		return c.loginLocked(ctx, op)
	}
	if !c.now().Before(c.renewAt) {
		if err := c.renewLocked(ctx, op); err != nil {
			c.log.Warn("secrets: qm token renewal failed; logging in again", "op", op)
			return c.loginLocked(ctx, op)
		}
	}
	return c.tok, nil
}

// relogin replaces a token qm rejected. If another goroutine has already
// replaced it, that token is used instead of logging in a second time.
func (c *Client) relogin(ctx context.Context, op, rejected string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tok != "" && c.tok != rejected {
		return c.tok, nil
	}
	c.log.Info("secrets: qm rejected the client token; logging in again", "op", op)
	return c.loginLocked(ctx, op)
}

func (c *Client) loginLocked(ctx context.Context, op string) (string, error) {
	c.tok = ""
	sid, err := c.secretIDValue()
	if err != nil {
		return "", &Error{Op: op, Err: err}
	}
	lc, err := loginCall(c.roleID, sid)
	if err != nil {
		return "", &Error{Op: op, Err: ErrProtocol}
	}
	status, body, err := c.send(ctx, op, lc, "")
	if err != nil {
		return "", err
	}
	if status < 200 || status >= 300 {
		c.log.Warn("secrets: qm login refused", "op", op, "status", status)
		return "", &Error{Op: op, Status: status, Err: statusError(status, body)}
	}
	tok, lease, err := decodeAuth(body)
	if err != nil || tok == "" || headerSafe("client token", tok) != nil {
		return "", &Error{Op: op, Err: ErrProtocol}
	}
	c.tok = tok
	c.renewAt = c.now().Add(lease * 2 / 3)
	c.log.Info("secrets: qm login", "lease_seconds", int(lease.Seconds()))
	return tok, nil
}

func (c *Client) renewLocked(ctx context.Context, op string) error {
	status, body, err := c.send(ctx, op, renewCall(), c.tok)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return &Error{Op: op, Status: status, Err: statusError(status, body)}
	}
	_, lease, err := decodeAuth(body)
	if err != nil {
		return &Error{Op: op, Err: ErrProtocol}
	}
	c.renewAt = c.now().Add(lease * 2 / 3)
	return nil
}

// credential returns the static client token, from the file when one is
// configured. Errors never include the token or the file content.
func (c *Client) credential() (string, error) {
	tok := c.token
	if c.tokenFile != "" {
		var err error
		if tok, err = readSecretFile(c.tokenFile, "qm token"); err != nil {
			return "", err
		}
	}
	if tok == "" {
		return "", errors.New("secrets: the qm token is empty")
	}
	if err := headerSafe("token", tok); err != nil {
		return "", err
	}
	return tok, nil
}

// secretIDValue returns the AppRole secret id, re-reading its file.
func (c *Client) secretIDValue() (string, error) {
	sid := c.secretID
	if c.secretFil != "" {
		var err error
		if sid, err = readSecretFile(c.secretFil, "qm secret id"); err != nil {
			return "", err
		}
	}
	if sid == "" {
		return "", errors.New("secrets: the qm secret id is empty")
	}
	return sid, nil
}

func readSecretFile(path, what string) (string, error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("secrets: cannot open the %s file", what)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(b) > 4096 {
		return "", fmt.Errorf("secrets: cannot read the %s file", what)
	}
	return strings.TrimSpace(string(b)), nil
}

// headerSafe rejects characters that cannot go in a header or JSON field
// unescaped. The error never includes the value.
func headerSafe(what, v string) error {
	for _, r := range v {
		if r < 0x21 || r > 0x7e {
			return fmt.Errorf("secrets: the qm %s has characters not allowed in a header", what)
		}
	}
	return nil
}

func loopback(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}
