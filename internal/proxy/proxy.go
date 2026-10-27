// Package proxy is the authenticating reverse proxy in front of each
// tenant's Bridge (US-015). The caller has already decided that the request
// may reach the tenant (a live Bridge session for this tenant); this package
// does the forwarding:
//
//   - every request gets a fresh signed X-SanHolo-Identity, and any identity,
//     X-SanHolo-* or forwarding header the browser sent is dropped first;
//   - the control plane's own cookies are never forwarded, and the Bridge
//     cannot set them; the Bridge's other cookies (its CSRF cookie) and the
//     X-CSRF-Token header pass through untouched;
//   - responses are streamed (no buffering of the body) and carry the
//     proxy's security headers in place of the Bridge's.
//
// Streaming: the Bridge uses polling htmx and JSON, no SSE today, but the
// proxy does not rely on that. The response body is handed to fasthttp as a
// body stream; with no Content-Length it is sent chunked and flushed per
// chunk. Fiber's own proxy middleware (fasthttp client) and the net/http
// adaptor both buffer whole responses, so the proxy uses net/http's
// Transport (own hop-by-hop handling, trailers, connection reuse) and writes
// through Fiber's stream hook.
package proxy

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/lhaig/sanholo-cloud/internal/auth"
	"github.com/lhaig/sanholo-cloud/internal/identity"
	"github.com/lhaig/sanholo-cloud/internal/provision"
)

// Resolver finds where a tenant's Bridge listens. *provision.Service
// satisfies it; ErrStarting and ErrNotRunning from that package select the
// page shown.
type Resolver interface {
	Upstream(ctx context.Context, tenantID string) (string, error)
}

// Config configures a Proxy.
type Config struct {
	// Key signs the identity of every request. Never logged.
	Key      ed25519.PrivateKey
	Resolver Resolver
	// Scheme is the public scheme (https), sent as X-Forwarded-Proto.
	Scheme string
	// AppOrigin is the control plane's origin, linked from the pages the
	// proxy writes itself.
	AppOrigin string
	// TrustedProxies are the edge addresses whose X-Forwarded-For is
	// believed when working out the client address.
	TrustedProxies []netip.Prefix
	// Cookies are the control plane's cookie names: never forwarded to a
	// Bridge, and a Bridge cannot set them.
	Cookies []string
	// Now, CacheTTL and Transport are for tests; zero values give the
	// clock, 10 seconds and a tuned http.Transport.
	Now       func() time.Time
	CacheTTL  time.Duration
	Transport http.RoundTripper
}

// Proxy forwards requests to Bridges.
type Proxy struct {
	cfg     Config
	deny    map[string]bool
	now     func() time.Time
	ttl     time.Duration
	rt      http.RoundTripper
	mu      sync.Mutex
	address map[string]cached
}

type cached struct {
	addr    string
	expires time.Time
}

// New returns a Proxy. It fails without a key or resolver.
func New(cfg Config) (*Proxy, error) {
	if len(cfg.Key) != ed25519.PrivateKeySize {
		return nil, errors.New("proxy: no signing key")
	}
	if cfg.Resolver == nil {
		return nil, errors.New("proxy: no resolver")
	}
	p := &Proxy{cfg: cfg, deny: map[string]bool{}, now: cfg.Now, ttl: cfg.CacheTTL, rt: cfg.Transport, address: map[string]cached{}}
	for _, n := range cfg.Cookies {
		p.deny[n] = true
	}
	if p.now == nil {
		p.now = time.Now
	}
	if p.ttl == 0 {
		p.ttl = 10 * time.Second
	}
	if p.rt == nil {
		p.rt = &http.Transport{
			Proxy:                 nil, // never through an environment proxy: the upstream is on the private network
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			DisableCompression:    true, // pass Content-Encoding through untouched
			ResponseHeaderTimeout: 30 * time.Second,
			MaxIdleConnsPerHost:   8,
			IdleConnTimeout:       60 * time.Second,
		}
	}
	return p, nil
}

// The Bridge's needs, from reading its templates and scripts
// (san-holo internal/web/templates, static): inline scripts (the
// accessibility snippet, the import map for the vendored three.js, an
// onclick), a JSON data block, one inline <style>, style="" attributes, and
// same-origin fetch, fonts, service worker and manifest. There is no CDN, no
// frame, no plugin. Inline script and style therefore stay allowed; every
// other directive is closed.
const csp = "default-src 'none'; " +
	"script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; font-src 'self'; connect-src 'self'; " +
	"manifest-src 'self'; worker-src 'self'; " +
	"form-action 'self'; base-uri 'none'; object-src 'none'; frame-ancestors 'none'"

// securityHeaders are set on every response from a tenant host, and replace
// whatever the Bridge sent under the same names.
var securityHeaders = [][2]string{
	{"Content-Security-Policy", csp},
	{"X-Frame-Options", "DENY"},
	{"Referrer-Policy", "same-origin"},
	{"Strict-Transport-Security", "max-age=31536000"},
	{"X-Content-Type-Options", "nosniff"},
}

// SecurityHeaders sets the proxy's security headers on c's response.
func SecurityHeaders(c *fiber.Ctx) {
	for _, h := range securityHeaders {
		c.Set(h[0], h[1])
	}
}

// hopByHop are the headers that describe one connection, not the message.
var hopByHop = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// dropOnRequest are removed from what the browser sent: identity and
// forwarding headers the proxy sets itself, and credentials meant for other
// parties.
var dropOnRequest = []string{
	"Host", "Content-Length", "Cookie", "Authorization", "Forwarded", "X-Real-Ip",
	"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Forwarded-Port", "X-Forwarded-Server",
	"X-Client-Ip", "True-Client-Ip", "Cf-Connecting-Ip", "X-Original-Forwarded-For",
}

func in(list []string, name string) bool {
	for _, n := range list {
		if strings.EqualFold(n, name) {
			return true
		}
	}
	return false
}

// connectionTokens are the header names a Connection header lists as
// hop-by-hop for this message.
func connectionTokens(values []string) []string {
	var out []string
	for _, v := range values {
		for tok := range strings.SplitSeq(v, ",") {
			if tok = strings.TrimSpace(tok); tok != "" {
				out = append(out, tok)
			}
		}
	}
	return out
}

func (p *Proxy) upstream(ctx context.Context, tenantID string) (string, error) {
	now := p.now()
	p.mu.Lock()
	e, ok := p.address[tenantID]
	p.mu.Unlock()
	if ok && now.Before(e.expires) {
		return e.addr, nil
	}
	addr, err := p.cfg.Resolver.Upstream(ctx, tenantID)
	if err != nil {
		p.forget(tenantID)
		return "", err
	}
	if h, port, err := net.SplitHostPort(addr); err != nil || h == "" {
		return "", fmt.Errorf("proxy: upstream address %q is not host:port", addr)
	} else if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("proxy: upstream address %q has no valid port", addr)
	}
	p.mu.Lock()
	p.address[tenantID] = cached{addr: addr, expires: now.Add(p.ttl)}
	p.mu.Unlock()
	return addr, nil
}

func (p *Proxy) forget(tenantID string) {
	p.mu.Lock()
	delete(p.address, tenantID)
	p.mu.Unlock()
}

// clientAddr is the real client address by the edge-trust rule.
func (p *Proxy) clientAddr(c *fiber.Ctx) string {
	peer := netip.Addr{}
	if ap, err := netip.ParseAddrPort(c.Context().RemoteAddr().String()); err == nil {
		peer = ap.Addr()
	}
	a := auth.ClientAddr(peer, c.Get("X-Forwarded-For"), p.cfg.TrustedProxies)
	if !a.IsValid() {
		return ""
	}
	return a.String()
}

// Handle forwards the request to tenantID's Bridge and writes the response.
// The caller has authenticated the visitor for this tenant.
func (p *Proxy) Handle(c *fiber.Ctx, tenantID string) error {
	ctx := c.UserContext()
	addr, err := p.upstream(ctx, tenantID)
	switch {
	case errors.Is(err, provision.ErrStarting):
		return p.page(c, "Your bot is starting", "It will be ready in a few minutes. Reload this page to try again.", 15)
	case errors.Is(err, provision.ErrNotRunning):
		return p.page(c, "Your bot is not running", "Open your account to see why and to start it again.", 0)
	case err != nil:
		return p.page(c, "Your bot cannot be reached right now", "This is temporary. Reload this page in a minute.", 30)
	}

	raw := string(c.Request().Header.RequestURI())
	if !strings.HasPrefix(raw, "/") {
		return c.SendStatus(fiber.StatusBadRequest)
	}
	out, err := http.NewRequestWithContext(ctx, c.Method(), "http://"+addr+raw, p.body(c))
	if err != nil {
		return c.SendStatus(fiber.StatusBadRequest)
	}
	if err := p.requestHeaders(c, out, tenantID); err != nil {
		// The signing key is wrong; say nothing about why.
		return p.page(c, "Something went wrong", "The request failed. Try again in a few minutes.", 0)
	}

	resp, err := p.rt.RoundTrip(out)
	if err != nil {
		// A connection failure usually means the allocation moved: look the
		// address up again next time.
		p.forget(tenantID)
		return p.page(c, "Your bot is starting", "It will be ready in a few minutes. Reload this page to try again.", 5)
	}
	return p.respond(c, resp)
}

func (p *Proxy) body(c *fiber.Ctx) io.Reader {
	// The raw bytes, still encoded as the Content-Encoding header says (the
	// Fiber helper Body() would decode them).
	if b := c.Request().Body(); len(b) > 0 {
		return bytes.NewReader(b)
	}
	return nil // a literal nil interface: a nil *bytes.Reader would not be nil to NewRequest
}

// requestHeaders builds the upstream request's headers from the browser's.
func (p *Proxy) requestHeaders(c *fiber.Ctx, out *http.Request, tenantID string) error {
	var conn []string
	c.Request().Header.VisitAll(func(k, v []byte) {
		if strings.EqualFold(string(k), "Connection") {
			conn = append(conn, string(v))
		}
	})
	extra := connectionTokens(conn)
	c.Request().Header.VisitAll(func(k, v []byte) {
		name := string(k)
		switch {
		case in(hopByHop, name), in(dropOnRequest, name), in(extra, name):
			return
		case len(name) >= 10 && strings.EqualFold(name[:10], "X-SanHolo-"):
			// The identity header, and any other header in the proxy's
			// namespace: only this proxy writes them.
			return
		}
		out.Header.Add(name, string(v))
	})
	if ck := p.cookies(string(c.Request().Header.Peek("Cookie"))); ck != "" {
		out.Header.Set("Cookie", ck)
	}
	tok, err := identity.Sign(p.cfg.Key, tenantID, p.now())
	if err != nil {
		return err
	}
	out.Header.Set(identity.Header, tok)
	host := string(c.Request().Header.Host())
	out.Host = host
	if ip := p.clientAddr(c); ip != "" {
		out.Header.Set("X-Forwarded-For", ip)
	}
	out.Header.Set("X-Forwarded-Proto", p.cfg.Scheme)
	out.Header.Set("X-Forwarded-Host", host)
	return nil
}

// cookies removes the control plane's cookies from a Cookie header and
// returns the rest, in order.
func (p *Proxy) cookies(header string) string {
	var keep []string
	for part := range strings.SplitSeq(header, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, _, _ := strings.Cut(part, "=")
		if p.deny[strings.TrimSpace(name)] {
			continue
		}
		keep = append(keep, part)
	}
	return strings.Join(keep, "; ")
}

// respond copies the upstream response to c and streams its body.
func (p *Proxy) respond(c *fiber.Ctx, resp *http.Response) error {
	c.Status(resp.StatusCode)
	extra := connectionTokens(resp.Header.Values("Connection"))
	h := &c.Response().Header
	for name, values := range resp.Header {
		switch {
		case in(hopByHop, name), in(extra, name), strings.EqualFold(name, "Content-Length"):
			continue
		case strings.EqualFold(name, "Set-Cookie"):
			for _, v := range values {
				if cn, _, _ := strings.Cut(v, "="); !p.deny[strings.TrimSpace(cn)] {
					h.Add(name, v)
				}
			}
			continue
		}
		for _, v := range values {
			h.Add(name, v)
		}
	}
	// Set replaces whatever the Bridge sent under the same names.
	SecurityHeaders(c)
	switch {
	case c.Method() == fiber.MethodHead || resp.StatusCode == fiber.StatusNoContent || resp.StatusCode == fiber.StatusNotModified:
		// No body: the length is the one the Bridge announced.
		if resp.ContentLength >= 0 && c.Method() == fiber.MethodHead {
			c.Response().Header.SetContentLength(int(resp.ContentLength))
		}
		_ = resp.Body.Close()
	default:
		// fasthttp closes the body when it has been sent.
		c.Context().SetBodyStream(resp.Body, int(resp.ContentLength))
	}
	return nil
}

// page writes a plain page for a bot that cannot be reached. It is the
// control plane speaking, not a proxy error, and the same words whatever
// went wrong underneath.
func (p *Proxy) page(c *fiber.Ctx, heading, text string, retryAfter int) error {
	SecurityHeaders(c)
	c.Set("Cache-Control", "no-store")
	c.Set("Content-Type", "text/html; charset=utf-8")
	if retryAfter > 0 {
		c.Set("Retry-After", strconv.Itoa(retryAfter))
	}
	link := ""
	if p.cfg.AppOrigin != "" {
		if u, err := url.Parse(p.cfg.AppOrigin); err == nil && u.Host != "" {
			link = `<p><a href="` + html.EscapeString(p.cfg.AppOrigin+"/account") + `">Your account</a></p>`
		}
	}
	return c.Status(fiber.StatusServiceUnavailable).SendString(
		`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>` +
			html.EscapeString(heading) + `</title></head><body><h1>` + html.EscapeString(heading) + `</h1><p>` + html.EscapeString(text) + `</p>` + link + `</body></html>`)
}
