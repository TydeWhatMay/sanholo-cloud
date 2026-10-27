package server

import (
	"errors"
	"html"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/lhaig/sanholo-cloud/internal/auth"
	"github.com/lhaig/sanholo-cloud/internal/provision"
	"github.com/lhaig/sanholo-cloud/internal/proxy"
	"github.com/lhaig/sanholo-cloud/internal/tenant"
)

// bridgeCookie is the Bridge session cookie on <tenant-id>.<domain>. It is a
// different cookie from the account session: host-only (the __Host- prefix
// forbids a Domain attribute), set only by /auth on the tenant host, and
// never sent to the app host. The account session cookie is host-only on the
// app host, so a browser never sends it to a tenant host either.
const bridgeCookie = "__Host-sanholo_bridge"

// ControlPlaneCookies are the control plane's own cookie names that the proxy
// must never forward to a Bridge nor let a Bridge set.
func ControlPlaneCookies() []string { return []string{sessionCookie, bridgeCookie} }

// bridgeAuthLimit is /auth requests per client IP per minute. Codes are 256
// random bits, so this is about not letting one address hammer the database.
const (
	bridgeAuthLimit  = 20
	bridgeAuthWindow = time.Minute
)

// bridgeRouting is the host routing of the control plane: the app host
// serves the control plane, <tenant-id>.<domain> is a tenant's Bridge, any
// other host is 404. It is off (nil) unless App.Proxy is set.
type bridgeRouting struct {
	proxy     *proxy.Proxy
	domain    string // tenant domain, lower case, no dot at either end
	appHost   string // host of the app origin, lower case, no port
	appOrigin string // https://app.example.org[:port]
	scheme    string
	port      string // port of the app origin, "" for the default
}

// newBridgeRouting derives the routing from the app origin and the tenant
// domain. It is validated by config; an unusable value is a bug.
func newBridgeRouting(p *proxy.Proxy, appBaseURL, domain string) (*bridgeRouting, error) {
	u, err := url.Parse(appBaseURL)
	if err != nil || u.Hostname() == "" {
		return nil, errors.New("server: the app origin is not a URL")
	}
	domain = strings.ToLower(strings.Trim(domain, "."))
	if domain == "" {
		return nil, errors.New("server: no tenant domain")
	}
	return &bridgeRouting{
		proxy: p, domain: domain, appHost: strings.ToLower(u.Hostname()),
		appOrigin: strings.TrimRight(appBaseURL, "/"), scheme: u.Scheme, port: u.Port(),
	}, nil
}

// hostOf is the request's Host header without port or trailing dot, lower
// case. X-Forwarded-Host is not consulted: the edge forwards the Host.
func hostOf(c *fiber.Ctx) string {
	h := string(c.Request().Header.Host())
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	return strings.TrimSuffix(strings.ToLower(h), ".")
}

// tenantOf returns the tenant a host names, if it is <valid-id>.<domain>.
func (b *bridgeRouting) tenantOf(host string) (string, bool) {
	label, ok := strings.CutSuffix(host, "."+b.domain)
	if !ok || strings.Contains(label, ".") || !tenant.Valid(label) {
		return "", false
	}
	return label, true
}

// origin is the Bridge origin of a tenant.
func (b *bridgeRouting) origin(tenantID string) string {
	return tenantOrigin(b.scheme, b.domain, b.port, tenantID)
}

func tenantOrigin(scheme, domain, port, tenantID string) string {
	host := tenantID + "." + domain
	if port != "" {
		host += ":" + port
	}
	return scheme + "://" + host
}

// TenantOriginFunc returns the Bridge origin of a tenant, derived from the app
// origin (scheme and port) and the tenant domain exactly as the routing does.
func TenantOriginFunc(appBaseURL, domain string) (func(tenantID string) string, error) {
	b, err := newBridgeRouting(nil, appBaseURL, domain)
	if err != nil {
		return nil, err
	}
	return b.origin, nil
}

// route is the first middleware after recover. The app host falls through to
// the control plane's routes; a tenant host is handled here and goes no
// further; anything else is 404, the same answer for every unknown name.
func (b *bridgeRouting) route(a *App) fiber.Handler {
	return func(c *fiber.Ctx) error {
		host := hostOf(c)
		if host == b.appHost {
			return c.Next()
		}
		if id, ok := b.tenantOf(host); ok {
			return a.bridge(c, id)
		}
		return notFound(c)
	}
}

func notFound(c *fiber.Ctx) error {
	proxy.SecurityHeaders(c)
	c.Set("Cache-Control", "no-store")
	return c.Status(fiber.StatusNotFound).SendString("not found")
}

// bridge serves a request on a tenant host.
func (a *App) bridge(c *fiber.Ctx, tenantID string) error {
	proxy.SecurityHeaders(c)
	if c.Path() == "/auth" {
		return a.bridgeAuth(c, tenantID)
	}
	secret := c.Cookies(bridgeCookie)
	bs, err := a.Auth.LookupBridgeSession(c.UserContext(), secret)
	switch {
	case errors.Is(err, auth.ErrNoSession):
		if secret != "" {
			clearBridgeCookie(c)
		}
		if isPushPath(c.Path()) {
			// A script's fetch must not follow a redirect to the sign-in page.
			return jsonStatus(c, fiber.StatusUnauthorized, "sign in again")
		}
		return a.toSignin(c)
	case err != nil:
		return c.Status(fiber.StatusInternalServerError).SendString("Something went wrong")
	}
	// Another customer's session, or a suspended or deleting tenant: the same 404 as an
	// unknown host, so neither tells the visitor anything.
	if bs.TenantID != tenantID || bs.Unavailable {
		return notFound(c)
	}
	if isPushPath(c.Path()) {
		return a.bridgePush(c, tenantID, bs)
	}
	return a.bridges.proxy.Handle(c, tenantID)
}

// toSignin sends a visitor with no Bridge session to the app's sign-in.
// An htmx request gets HX-Redirect, since a fetch would follow a plain
// redirect and swap the sign-in page into the Bridge.
func (a *App) toSignin(c *fiber.Ctx) error {
	c.Set("Cache-Control", "no-store")
	to := a.bridges.appOrigin + "/signin"
	if c.Get("HX-Request") != "" {
		c.Set("HX-Redirect", to)
		return c.SendStatus(fiber.StatusUnauthorized)
	}
	return c.Redirect(to, fiber.StatusSeeOther)
}

// bridgeAuth swaps a handoff code for the Bridge session cookie. GET only,
// and a GET that spends something, so HEAD and prefetch requests (mail
// scanners, unfurlers, browsers warming links) are answered without
// touching the code.
func (a *App) bridgeAuth(c *fiber.Ctx, tenantID string) error {
	c.Set("Cache-Control", "no-store")
	switch c.Method() {
	case fiber.MethodHead:
		return c.SendStatus(fiber.StatusNoContent)
	case fiber.MethodGet:
	default:
		return c.SendStatus(fiber.StatusMethodNotAllowed)
	}
	if isPrefetch(c) {
		return c.SendStatus(fiber.StatusNoContent)
	}
	ok, err := a.Auth.Allow(c.UserContext(), "bridge-auth:ip:"+a.clientIP(c), bridgeAuthLimit, bridgeAuthWindow)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).SendString("Something went wrong")
	}
	if !ok {
		c.Set("Retry-After", "60")
		return a.bridgeMessage(c, fiber.StatusTooManyRequests, "Too many requests", "Wait a minute before trying again.")
	}
	// A cookie the browser already holds is revoked, never reused.
	if old := c.Cookies(bridgeCookie); old != "" {
		_ = a.Auth.RevokeBridgeSecret(c.UserContext(), old)
	}
	secret, expires, err := a.Auth.RedeemHandoff(c.UserContext(), c.Query("code"), tenantID)
	if errors.Is(err, auth.ErrHandoffInvalid) {
		return a.bridgeMessage(c, fiber.StatusGone, "Link not valid", "This link was used or has expired. Open your Bridge again from your account.")
	}
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).SendString("Something went wrong")
	}
	c.Cookie(&fiber.Cookie{
		Name: bridgeCookie, Value: secret, Path: "/", // no Domain: host-only
		Expires: expires, MaxAge: int(time.Until(expires).Seconds()),
		// Lax, not Strict: a notification that opens the Bridge is a
		// cross-site navigation and must still arrive signed in.
		Secure: true, HTTPOnly: true, SameSite: fiber.CookieSameSiteLaxMode,
	})
	return c.Redirect("/", fiber.StatusSeeOther)
}

func clearBridgeCookie(c *fiber.Ctx) {
	c.Cookie(&fiber.Cookie{
		Name: bridgeCookie, Value: "", Path: "/", MaxAge: -1, Expires: time.Unix(0, 0),
		Secure: true, HTTPOnly: true, SameSite: fiber.CookieSameSiteLaxMode,
	})
}

// bridgeMessage is a plain page on a tenant host. The tenant origin serves
// no control plane assets, so the page carries none.
func (a *App) bridgeMessage(c *fiber.Ctx, status int, heading, text string) error {
	c.Set("Content-Type", "text/html; charset=utf-8")
	return c.Status(status).SendString(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>` +
		html.EscapeString(heading) + `</title></head><body><h1>` + html.EscapeString(heading) + `</h1><p>` + html.EscapeString(text) +
		`</p><p><a href="` + html.EscapeString(a.bridges.appOrigin+"/account") + `">Your account</a></p></body></html>`)
}

// bridgeOpen is the signed-in "Open my Bridge" action: it makes a 60 second
// single-use code for the person's own tenant and sends the browser to the
// tenant origin to swap it for a Bridge session. POST with the CSRF token,
// so another site cannot make a browser spend codes.
func (a *App) bridgeOpen(c *fiber.Ctx) error {
	if a.bridges == nil {
		return a.message(c, fiber.StatusNotFound, "Not available", "Opening the Bridge is not set up here.")
	}
	ses := c.Locals("session").(auth.Session)
	ok, err := a.allow(c, "bridge:open:account:"+ses.AccountID, bridgeOpenLimit)
	if err != nil {
		return a.serverError(c)
	}
	if !ok {
		return a.message(c, fiber.StatusTooManyRequests, "Too many requests", "Wait an hour before trying again.")
	}
	if a.Provision.Enabled() {
		inst, err := a.Provision.Get(c.UserContext(), ses.TenantID)
		if err != nil {
			return a.serverError(c)
		}
		if inst == nil || inst.State == provision.StateStopped || inst.State == provision.StateFailed {
			return a.message(c, fiber.StatusConflict, "Your bot is not running", "Start the bot from your account first.")
		}
	}
	code, tenantID, err := a.Auth.CreateHandoff(c.UserContext(), ses.ID)
	if err != nil {
		return a.serverError(c)
	}
	c.Set("Referrer-Policy", "no-referrer")
	return c.Redirect(a.bridges.origin(tenantID)+"/auth?code="+url.QueryEscape(code), fiber.StatusSeeOther)
}

// bridgeOpenLimit is "Open my Bridge" presses per account per hour.
const bridgeOpenLimit = 60
