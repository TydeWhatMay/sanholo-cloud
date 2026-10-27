// Package game is the control plane's minimal client for the Replicant Space
// API. It makes exactly one read, GET /v1/accounts/me, to learn which game
// account an API token belongs to. It does not import the SanHolo client.
//
// The token is sent only in the Authorization header. No error returned by
// this package, and no log line, contains the token or a response body.
package game

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// DefaultURL is the production game API.
const DefaultURL = "https://api.replicant.space"

const (
	mePath  = "/v1/accounts/me"
	maxBody = 1 << 20
	// maxToken bounds what is sent as a header value.
	maxToken = 512
)

var (
	// ErrRejected: the game did not accept the token (401/403), or the token
	// cannot be a valid header value.
	ErrRejected = errors.New("the game did not accept the token")
	// ErrUnavailable: the game could not be reached or is failing (network
	// error, timeout, 429, 5xx). Retrying later may work.
	ErrUnavailable = errors.New("the game is unavailable")
	// ErrProtocol: the game answered in a way this package does not
	// understand, or the account has no replicant.
	ErrProtocol = errors.New("unexpected game response")
	// ErrNoReplicant: the token is valid but its account has no replicant.
	ErrNoReplicant = errors.New("the game account has no replicant")
)

// Account is what the control plane learns from the game.
type Account struct {
	// ID identifies the game account. The game's account record carries no
	// identifier of its own (its email address can be changed), so the ID is
	// derived from the account's earliest replicant, which never changes:
	// "replicant:<code>".
	ID            string
	ReplicantCode string
	ReplicantName string
}

// Config configures the client.
type Config struct {
	// BaseURL is the game API origin; empty means DefaultURL.
	BaseURL string
	// Version goes into the User-Agent, SanHolo-Cloud/<version>.
	Version string
	// Timeout bounds each request. Default 10 seconds.
	Timeout time.Duration
}

// Client reads the game. It is safe for concurrent use.
type Client struct {
	base string
	ua   string
	http *http.Client
}

// New validates cfg.
func New(cfg Config) (*Client, error) {
	raw := cfg.BaseURL
	if raw == "" {
		raw = DefaultURL
	}
	base, err := ParseURL(raw)
	if err != nil {
		return nil, fmt.Errorf("game: %w", err)
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	version := cfg.Version
	if version == "" {
		version = "dev"
	}
	return &Client{
		base: base,
		ua:   "SanHolo-Cloud/" + version,
		http: &http.Client{
			Timeout: timeout,
			// A redirect would carry the Authorization header elsewhere.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// ParseURL validates GAME_API_URL: an origin, https, or http on a loopback
// host (tests and local development). It returns the origin without a
// trailing slash.
func ParseURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", errors.New("want an origin such as https://api.replicant.space")
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

var (
	codeRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	// A header value: printable ASCII, no spaces.
	tokenRE = regexp.MustCompile(`^[\x21-\x7e]+$`)
)

// Me validates token with one read and returns the game account it belongs
// to. Errors wrap ErrRejected, ErrUnavailable, ErrProtocol or ErrNoReplicant.
func (c *Client) Me(ctx context.Context, token string) (Account, error) {
	if token == "" || len(token) > maxToken || !tokenRE.MatchString(token) {
		return Account{}, ErrRejected
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+mePath, nil)
	if err != nil {
		return Account{}, ErrProtocol
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		// The transport error quotes the URL; report only the kind.
		return Account{}, ErrUnavailable
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return Account{}, ErrRejected
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return Account{}, ErrUnavailable
	case resp.StatusCode != http.StatusOK:
		return Account{}, ErrProtocol
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return Account{}, ErrUnavailable
	}
	if len(body) > maxBody {
		return Account{}, ErrProtocol
	}
	return decodeMe(body)
}

type replicantSummary struct {
	Code      string `json:"replicant_code"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
}

func decodeMe(body []byte) (Account, error) {
	var r struct {
		Replicants []replicantSummary `json:"replicants"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return Account{}, ErrProtocol
	}
	if len(r.Replicants) == 0 {
		return Account{}, ErrNoReplicant
	}
	reps := r.Replicants
	// Earliest first, by the time the replicant was created, not by the text
	// of it: RFC 3339 allows any fractional precision and any offset, and
	// "2026-01-01T00:00:00.5Z" sorts before "2026-01-01T00:00:00Z" as text.
	// A time that cannot be read sorts after every readable one. The code
	// breaks ties so the choice is deterministic.
	type keyed struct {
		rep replicantSummary
		at  time.Time
		ok  bool
	}
	ks := make([]keyed, len(reps))
	for i, rp := range reps {
		at, err := time.Parse(time.RFC3339, rp.CreatedAt)
		ks[i] = keyed{rp, at, err == nil}
	}
	sort.SliceStable(ks, func(i, j int) bool {
		a, b := ks[i], ks[j]
		switch {
		case a.ok != b.ok:
			return a.ok
		case a.ok && !a.at.Equal(b.at):
			return a.at.Before(b.at)
		}
		return a.rep.Code < b.rep.Code
	})
	reps = reps[:0]
	for _, k := range ks {
		reps = append(reps, k.rep)
	}
	first := reps[0]
	if !codeRE.MatchString(first.Code) || !displayable(first.Name) {
		return Account{}, ErrProtocol
	}
	return Account{ID: "replicant:" + first.Code, ReplicantCode: first.Code, ReplicantName: first.Name}, nil
}

// displayable: a short name with no control characters.
func displayable(s string) bool {
	if s == "" || len(s) > 100 {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
