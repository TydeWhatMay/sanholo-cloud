// Package secrets is the control plane's only route to tenant secrets in
// qm. The control plane's qm principal is write-only (US-002), and this
// package is shaped to match: it can store a tenant's game credentials,
// list names, and destroy; it has no method that returns a secret value.
// TestAPISurface fails if one is added.
//
// Deletion has exactly two doors, PurgeTenant and Rotate, so that qm's own
// tenant purge can replace their internals later. All wire detail is in
// wire.go.
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
	// ErrDenied: qm refused the control plane's token or policy (401/403).
	ErrDenied = errors.New("qm denied the request")
	// ErrUnavailable: qm is unreachable, sealed, rate limiting or failing
	// (network error, timeout, 429, 5xx). Retrying later may work.
	ErrUnavailable = errors.New("qm unavailable")
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

// Config configures the client. With no Addr the client is disabled.
type Config struct {
	// Addr is the qm origin (https, or http on loopback).
	Addr string
	// Token is the control plane's qm client token; TokenFile is a file
	// holding it, re-read on every request so renewal needs no restart.
	// Exactly one of them when Addr is set.
	Token     string
	TokenFile string
	// Timeout bounds each request. Default 10 seconds.
	Timeout time.Duration
	// Logger receives operation records (tenant, counts, status); never a
	// secret value. Default: discard.
	Logger *slog.Logger
}

// Client talks to qm. The zero value and a nil *Client are disabled.
type Client struct {
	base      string
	token     string
	tokenFile string
	timeout   time.Duration
	http      *http.Client
	log       *slog.Logger
}

// New validates cfg. An empty Addr (and no token) gives a disabled client
// whose operations return ErrSecretsUnavailable.
func New(cfg Config) (*Client, error) {
	if cfg.Addr == "" {
		if cfg.Token != "" || cfg.TokenFile != "" {
			return nil, errors.New("secrets: a qm token is set but no qm address")
		}
		return &Client{}, nil
	}
	base, err := ParseAddr(cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("secrets: qm address: %w", err)
	}
	if (cfg.Token == "") == (cfg.TokenFile == "") {
		return nil, errors.New("secrets: set exactly one of the qm token and the qm token file")
	}
	c := &Client{base: base, token: cfg.Token, tokenFile: cfg.TokenFile, timeout: cfg.Timeout, log: cfg.Logger}
	if c.timeout <= 0 {
		c.timeout = 10 * time.Second
	}
	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}
	if _, err := c.credential(); err != nil {
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

// Ready probes qm's readiness endpoint (no auth; 503 when sealed).
func (c *Client) Ready(ctx context.Context) error {
	if !c.Enabled() {
		return ErrSecretsUnavailable
	}
	_, _, err := c.do(ctx, "ready", readyCall())
	return err
}

// Put stores a tenant's game credentials as a new version at the tenant's
// game path. It does not remove older versions: use Rotate to replace.
func (c *Client) Put(ctx context.Context, tenantID string, cr Credentials) error {
	if _, err := c.put(ctx, "put", tenantID, cr); err != nil {
		return err
	}
	c.log.Info("secrets: stored", "tenant", tenantID)
	return nil
}

func (c *Client) put(ctx context.Context, op, tenantID string, cr Credentials) (int, error) {
	if err := c.check(tenantID); err != nil {
		return 0, err
	}
	if !cr.valid() {
		return 0, &Error{Op: op, Err: ErrInvalidCredentials}
	}
	pc, err := putCall(gamePath(tenantID), cr)
	if err != nil {
		return 0, &Error{Op: op, Err: ErrProtocol}
	}
	_, body, err := c.do(ctx, op, pc)
	if err != nil {
		return 0, err
	}
	v, err := decodePutVersion(body)
	if err != nil {
		return 0, &Error{Op: op, Err: ErrProtocol}
	}
	return v, nil
}

// List returns the names (relative to the tenant prefix, such as "game")
// that still hold a live version. It never returns values.
func (c *Client) List(ctx context.Context, tenantID string) ([]string, error) {
	if err := c.check(tenantID); err != nil {
		return nil, err
	}
	paths, err := c.walk(ctx, "list", tenantPrefix(tenantID), 0)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, p := range paths {
		live, err := c.liveVersions(ctx, "list", p)
		if err != nil {
			return nil, err
		}
		if len(live) > 0 {
			names = append(names, strings.TrimPrefix(p, tenantPrefix(tenantID)))
		}
	}
	return names, nil
}

// PurgeTenant destroys every live version of every path under the tenant's
// prefix, then checks that none remain. It is one of the two deletion
// paths. Safe to repeat.
func (c *Client) PurgeTenant(ctx context.Context, tenantID string) error {
	if err := c.check(tenantID); err != nil {
		return err
	}
	paths, err := c.walk(ctx, "purge", tenantPrefix(tenantID), 0)
	if err != nil {
		return err
	}
	destroyed := 0
	for _, p := range paths {
		live, err := c.liveVersions(ctx, "purge", p)
		if err != nil {
			return err
		}
		if err := c.destroy(ctx, "purge", p, live); err != nil {
			return err
		}
		destroyed += len(live)
	}
	left, err := c.List(ctx, tenantID)
	if err != nil {
		return err
	}
	if len(left) > 0 {
		return &Error{Op: "purge", Err: fmt.Errorf("%w: %d paths still hold versions", ErrProtocol, len(left))}
	}
	c.log.Info("secrets: purged", "tenant", tenantID, "paths", len(paths), "versions", destroyed)
	return nil
}

// Rotate writes the new credentials, then destroys every older version of
// the game path. If the destroy fails the new value is already in place and
// the error says so; running Rotate again finishes the job. It is one of the
// two deletion paths.
func (c *Client) Rotate(ctx context.Context, tenantID string, cr Credentials) error {
	newest, err := c.put(ctx, "rotate", tenantID, cr)
	if err != nil {
		return err
	}
	p := gamePath(tenantID)
	live, err := c.liveVersions(ctx, "rotate", p)
	if err != nil {
		return err
	}
	var old []int
	for _, v := range live {
		if v < newest {
			old = append(old, v)
		}
	}
	if err := c.destroy(ctx, "rotate", p, old); err != nil {
		return err
	}
	c.log.Info("secrets: rotated", "tenant", tenantID, "destroyed", len(old))
	return nil
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

// walk lists prefix (ending in "/") and returns every leaf path beneath it.
func (c *Client) walk(ctx context.Context, op, prefix string, depth int) ([]string, error) {
	if depth > maxDepth {
		return nil, &Error{Op: op, Err: fmt.Errorf("%w: tree too deep", ErrProtocol)}
	}
	status, body, err := c.do(ctx, op, listCall(prefix))
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, nil
	}
	keys, err := decodeKeys(body)
	if err != nil {
		return nil, &Error{Op: op, Err: ErrProtocol}
	}
	var out []string
	for _, k := range keys {
		if dir, ok := strings.CutSuffix(k, "/"); ok {
			sub, err := c.walk(ctx, op, prefix+dir+"/", depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, sub...)
			continue
		}
		out = append(out, prefix+k)
	}
	return out, nil
}

func (c *Client) liveVersions(ctx context.Context, op, path string) ([]int, error) {
	status, body, err := c.do(ctx, op, metaCall(path))
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, nil
	}
	live, err := decodeLiveVersions(body)
	if err != nil {
		return nil, &Error{Op: op, Err: ErrProtocol}
	}
	return live, nil
}

func (c *Client) destroy(ctx context.Context, op, path string, versions []int) error {
	if len(versions) == 0 {
		return nil
	}
	_, _, err := c.do(ctx, op, destroyCall(path, versions))
	return err
}

// do sends one call. Every error is an *Error wrapping a sentinel; none
// includes a request or response body.
func (c *Client) do(ctx context.Context, op string, k call) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	u := c.base + k.path
	if len(k.query) > 0 {
		u += "?" + k.query.Encode()
	}
	var rd io.Reader
	if k.body != nil {
		rd = strings.NewReader(string(k.body))
	}
	req, err := http.NewRequestWithContext(ctx, k.method, u, rd)
	if err != nil {
		return 0, nil, &Error{Op: op, Err: ErrProtocol}
	}
	if !k.noAuth {
		tok, err := c.credential()
		if err != nil {
			return 0, nil, &Error{Op: op, Err: err}
		}
		req.Header.Set(authHeaderName, tok)
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
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return resp.StatusCode, body, nil
	case resp.StatusCode == http.StatusNotFound && k.okNotFound:
		return resp.StatusCode, nil, nil
	}
	c.log.Warn("secrets: qm refused", "op", op, "status", resp.StatusCode)
	return resp.StatusCode, nil, &Error{Op: op, Status: resp.StatusCode, Err: statusError(resp.StatusCode)}
}

func transportKind(ctx context.Context, err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil:
		return "timeout"
	default:
		return "network"
	}
}

// credential returns the control plane's qm token, from the file when one is
// configured. Errors never include the token or the file content.
func (c *Client) credential() (string, error) {
	tok := c.token
	if c.tokenFile != "" {
		f, err := os.Open(filepath.Clean(c.tokenFile))
		if err != nil {
			return "", errors.New("secrets: cannot open the qm token file")
		}
		defer func() { _ = f.Close() }()
		b, err := io.ReadAll(io.LimitReader(f, 4097))
		if err != nil || len(b) > 4096 {
			return "", errors.New("secrets: cannot read the qm token file")
		}
		tok = strings.TrimSpace(string(b))
	}
	if tok == "" {
		return "", errors.New("secrets: the qm token is empty")
	}
	for _, r := range tok {
		if r < 0x21 || r > 0x7e {
			return "", errors.New("secrets: the qm token has characters not allowed in a header")
		}
	}
	return tok, nil
}

func loopback(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}
