// Package nomad is the control plane's minimal client for the Nomad HTTP API
// and the one place a tenant's job is built (job.go). It registers a tenant
// job, reads its status and stops it, with a namespace-scoped ACL token.
//
// The client can touch tenant jobs only: every job id must be
// sanholo-tenant-<valid tenant id>, so a bug elsewhere cannot make it read,
// stop or purge another job. The token is sent in X-Nomad-Token, never
// logged and never part of an error; redirects are not followed (they would
// carry the header elsewhere); responses are size-bounded.
package nomad

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/lhaig/sanholo-cloud/internal/tenant"
)

const (
	// DefaultNamespace is the Nomad namespace tenant jobs live in.
	DefaultNamespace = "sanholo"
	// JobPrefix precedes the tenant id in every tenant job id. The client
	// reads it from NOMAD_JOB_ID to learn its tenant.
	JobPrefix = "sanholo-tenant-"

	defaultTimeout = 10 * time.Second
	maxBody        = 4 << 20
	authHeaderName = "X-Nomad-Token"
)

var (
	// ErrDisabled: no Nomad is configured (development). Nothing was sent.
	ErrDisabled = errors.New("nomad is not configured")
	// ErrInvalidJob: the job id or the job itself breaks a rule of this
	// client (not a tenant job, wrong namespace). Nothing was sent.
	ErrInvalidJob = errors.New("invalid tenant job")
	// ErrNotFound: Nomad has no such job.
	ErrNotFound = errors.New("job not found")
	// ErrDenied: Nomad refused the token or its policy (403).
	ErrDenied = errors.New("nomad denied the request")
	// ErrRejected: Nomad found the request invalid (400).
	ErrRejected = errors.New("nomad rejected the request")
	// ErrConflict: the job changed since it was read (modify index).
	ErrConflict = errors.New("the job changed since it was read")
	// ErrUnavailable: Nomad is unreachable, rate limiting or failing.
	ErrUnavailable = errors.New("nomad unavailable")
	// ErrProtocol: Nomad answered in a way this package does not understand.
	ErrProtocol = errors.New("unexpected nomad response")
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
		return fmt.Sprintf("nomad: %s: %v (status %d)", e.Op, e.Err, e.Status)
	}
	return fmt.Sprintf("nomad: %s: %v", e.Op, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// Config configures the client. With no Addr the client is disabled.
type Config struct {
	// Addr is the Nomad origin: https, or http on loopback (tests, a local
	// agent).
	Addr string
	// Token is a Nomad ACL token limited to the tenant namespace; TokenFile
	// is a file holding it, re-read on every request. Exactly one is set.
	Token     string
	TokenFile string
	// Namespace is where tenant jobs live. Default DefaultNamespace.
	Namespace string
	// Region is sent as the region query parameter when set.
	Region string
	// Timeout bounds each request. Default 10 seconds.
	Timeout time.Duration
	// Logger receives operation records (job id, status); never the token.
	// Default: discard.
	Logger *slog.Logger
}

// Client talks to Nomad. The zero value and a nil *Client are disabled.
type Client struct {
	base      string
	token     string
	tokenFile string
	namespace string
	region    string
	http      *http.Client
	log       *slog.Logger
}

var namespaceRE = regexp.MustCompile(`^[a-zA-Z0-9-]{1,128}$`)

// ParseAddr accepts an origin: https, or http on a loopback host.
func ParseAddr(v string) (string, error) {
	u, err := url.Parse(v)
	if err != nil || u.Host == "" || u.Hostname() == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
		return "", errors.New("want an origin such as https://nomad.example.org:4646")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !loopback(u.Hostname()) {
			return "", errors.New("http is allowed for loopback hosts only")
		}
	default:
		return "", errors.New("scheme must be https")
	}
	return u.Scheme + "://" + u.Host, nil
}

func loopback(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

// New validates cfg. An empty Addr (and no token) gives a disabled client.
func New(cfg Config) (*Client, error) {
	if cfg.Addr == "" {
		if cfg.Token != "" || cfg.TokenFile != "" {
			return nil, errors.New("nomad: a token is set but no address")
		}
		return &Client{}, nil
	}
	base, err := ParseAddr(cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("nomad: address: %w", err)
	}
	if (cfg.Token == "") == (cfg.TokenFile == "") {
		return nil, errors.New("nomad: set exactly one of the token and the token file")
	}
	ns := cfg.Namespace
	if ns == "" {
		ns = DefaultNamespace
	}
	if !namespaceRE.MatchString(ns) {
		return nil, errors.New("nomad: invalid namespace name")
	}
	if cfg.Region != "" && !namespaceRE.MatchString(cfg.Region) {
		return nil, errors.New("nomad: invalid region name")
	}
	c := &Client{
		base: base, token: cfg.Token, tokenFile: cfg.TokenFile, namespace: ns, region: cfg.Region, log: cfg.Logger,
	}
	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	c.http = &http.Client{
		Timeout: timeout,
		// A redirect would carry the token header to wherever it points.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	if _, err := c.credential(); err != nil {
		return nil, err
	}
	return c, nil
}

// Enabled reports whether Nomad is configured.
func (c *Client) Enabled() bool { return c != nil && c.base != "" }

// Namespace is the namespace the client works in.
func (c *Client) Namespace() string {
	if c == nil {
		return ""
	}
	return c.namespace
}

// credential returns the token, reading the file when configured. The error
// never includes the token.
func (c *Client) credential() (string, error) {
	tok := c.token
	if c.tokenFile != "" {
		raw, err := os.ReadFile(c.tokenFile)
		if err != nil {
			return "", errors.New("nomad: cannot read the token file")
		}
		tok = strings.TrimSpace(string(raw))
	}
	if tok == "" {
		return "", errors.New("nomad: the token is empty")
	}
	for _, r := range tok {
		if r < 0x21 || r > 0x7e {
			return "", errors.New("nomad: the token has characters not allowed in a header")
		}
	}
	return tok, nil
}

// JobID returns the Nomad job id of a tenant.
func JobID(tenantID string) (string, error) {
	if !tenant.Valid(tenantID) {
		return "", ErrInvalidJob
	}
	return JobPrefix + tenantID, nil
}

// TenantOfJob returns the tenant id of a tenant job id.
func TenantOfJob(jobID string) (string, bool) {
	id, ok := strings.CutPrefix(jobID, JobPrefix)
	return id, ok && tenant.Valid(id)
}

func (c *Client) check(jobID string) error {
	if !c.Enabled() {
		return ErrDisabled
	}
	if _, ok := TenantOfJob(jobID); !ok {
		return &Error{Op: "validate", Err: ErrInvalidJob}
	}
	return nil
}

type request struct {
	op, method, path string
	query            url.Values
	body             any
}

// do sends one request and decodes a 2xx JSON answer into out (when
// non-nil). Every error is an *Error wrapping a sentinel.
func (c *Client) do(ctx context.Context, r request, out any) error {
	tok, err := c.credential()
	if err != nil {
		return &Error{Op: r.op, Err: ErrDenied}
	}
	var rd io.Reader
	if r.body != nil {
		raw, err := json.Marshal(r.body)
		if err != nil {
			return &Error{Op: r.op, Err: ErrProtocol}
		}
		rd = bytes.NewReader(raw)
	}
	q := url.Values{}
	for k, v := range r.query {
		q[k] = v
	}
	q.Set("namespace", c.namespace)
	if c.region != "" {
		q.Set("region", c.region)
	}
	ctx, cancel := context.WithTimeout(ctx, c.http.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, r.method, c.base+r.path+"?"+q.Encode(), rd)
	if err != nil {
		return &Error{Op: r.op, Err: ErrProtocol}
	}
	req.Header.Set(authHeaderName, tok)
	req.Header.Set("Accept", "application/json")
	if r.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// The transport error quotes the URL; report only the kind.
		kind := "network"
		if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			kind = "timeout"
		}
		c.log.Warn("nomad: request failed", "op", r.op, "error", kind)
		return &Error{Op: r.op, Err: ErrUnavailable}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return &Error{Op: r.op, Status: resp.StatusCode, Err: ErrUnavailable}
	}
	if len(body) > maxBody {
		return &Error{Op: r.op, Status: resp.StatusCode, Err: fmt.Errorf("%w: response too large", ErrProtocol)}
	}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
	case resp.StatusCode == http.StatusNotFound:
		return &Error{Op: r.op, Status: resp.StatusCode, Err: ErrNotFound}
	case resp.StatusCode == http.StatusForbidden:
		return &Error{Op: r.op, Status: resp.StatusCode, Err: ErrDenied}
	case resp.StatusCode == http.StatusConflict || bytes.Contains(body, []byte("Enforcing job modify index")):
		return &Error{Op: r.op, Status: resp.StatusCode, Err: ErrConflict}
	case resp.StatusCode == http.StatusBadRequest:
		return &Error{Op: r.op, Status: resp.StatusCode, Err: ErrRejected}
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return &Error{Op: r.op, Status: resp.StatusCode, Err: ErrUnavailable}
	default:
		return &Error{Op: r.op, Status: resp.StatusCode, Err: ErrProtocol}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return &Error{Op: r.op, Status: resp.StatusCode, Err: ErrProtocol}
	}
	return nil
}

// RegisterOptions control a registration. ModifyIndex is the job's index as
// last read: Nomad refuses the registration when the job has changed since
// (ErrConflict). Zero means the job must not exist yet.
type RegisterOptions struct {
	ModifyIndex uint64
}

// Register submits the job with the index check (POST /v1/jobs). The job's
// namespace must be the client's, and its id a tenant job id.
func (c *Client) Register(ctx context.Context, job Job, opt RegisterOptions) error {
	if err := c.check(job.ID); err != nil {
		return err
	}
	if job.Namespace != c.namespace {
		return &Error{Op: "register", Err: ErrInvalidJob}
	}
	payload := struct {
		Job            Job    `json:"Job"`
		EnforceIndex   bool   `json:"EnforceIndex"`
		JobModifyIndex uint64 `json:"JobModifyIndex"`
	}{job, true, opt.ModifyIndex}
	err := c.do(ctx, request{op: "register", method: http.MethodPost, path: "/v1/jobs", body: payload}, nil)
	if err == nil {
		c.log.Info("nomad: job registered", "job", job.ID)
	}
	return err
}

// Stop stops the job and, with purge, forgets it (DELETE /v1/job/<id>). A
// job that is already gone is not an error: stopping is idempotent.
func (c *Client) Stop(ctx context.Context, jobID string, purge bool) error {
	if err := c.check(jobID); err != nil {
		return err
	}
	q := url.Values{}
	if purge {
		q.Set("purge", "true")
	}
	err := c.do(ctx, request{op: "stop", method: http.MethodDelete, path: "/v1/job/" + url.PathEscape(jobID), query: q}, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err == nil {
		c.log.Info("nomad: job stopped", "job", jobID, "purge", purge)
	}
	return err
}

// RestartAllocation restarts the tasks of one allocation in place.
func (c *Client) RestartAllocation(ctx context.Context, allocID string) error {
	if !c.Enabled() {
		return ErrDisabled
	}
	if !allocIDRE.MatchString(allocID) {
		return &Error{Op: "restart", Err: ErrInvalidJob}
	}
	body := map[string]any{"AllTasks": true}
	return c.do(ctx, request{op: "restart", method: http.MethodPut, path: "/v1/client/allocation/" + allocID + "/restart", body: body}, nil)
}

var allocIDRE = regexp.MustCompile(`^[0-9a-f-]{8,64}$`)
