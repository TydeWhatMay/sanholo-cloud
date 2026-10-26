package server

import (
	"embed"
	"errors"
	"io/fs"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/filesystem"
	"github.com/gofiber/template/html/v2"

	"github.com/lhaig/sanholo-cloud/internal/auth"
)

//go:embed templates/*.html
var templateFiles embed.FS

//go:embed static
var staticFiles embed.FS

// waitlistLimit is the waiting-list submissions allowed per client IP per hour.
const waitlistLimit = 10

const sessionCookie = "__Host-sanholo_session"

// App is the customer-facing surface of the control plane.
type App struct {
	Auth *auth.Service
	// TrustedProxies are the edge addresses whose X-Forwarded-For is believed.
	TrustedProxies []netip.Prefix
}

func (a *App) views() fiber.Views {
	sub, err := fs.Sub(templateFiles, "templates")
	if err != nil {
		panic(err) // the embed pattern guarantees the directory
	}
	return html.NewFileSystem(http.FS(sub), ".html")
}

func (a *App) mount(app *fiber.App) {
	static, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic(err)
	}
	app.Use("/static", filesystem.New(filesystem.Config{Root: http.FS(static)}))

	app.Use(securityHeaders)
	app.Use(csrf())

	app.Get("/", a.home)
	app.Post("/waitlist", a.joinWaitlist)
	app.Get("/signup", a.signupForm)
	app.Post("/signup", a.signup)
	app.Get("/signin", a.signinForm)
	app.Post("/signin", a.signin)
	app.Get("/signin/verify", a.confirmPage)
	app.Post("/signin/verify", a.verify)

	acct := app.Group("/account", a.requireSession)
	acct.Get("/", a.account)
	acct.Post("/sessions/:id/revoke", a.revoke)
	app.Post("/signout", a.requireSession, a.signout)
}

// securityHeaders keeps pages out of caches, frames and referrers. The
// referrer rule matters most: a sign-in link carries its token in the URL.
func securityHeaders(c *fiber.Ctx) error {
	c.Set("X-Content-Type-Options", "nosniff")
	c.Set("Referrer-Policy", "no-referrer")
	c.Set("X-Frame-Options", "DENY")
	c.Set("Cache-Control", "no-store")
	c.Set("Content-Security-Policy",
		"default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; form-action 'self'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'")
	return c.Next()
}

func (a *App) render(c *fiber.Ctx, status int, name, title string, data fiber.Map) error {
	if data == nil {
		data = fiber.Map{}
	}
	data["Title"] = title
	data["CSRF"], _ = c.Locals("csrf").(string)
	c.Status(status)
	return c.Render(name, data, "layout")
}

func (a *App) message(c *fiber.Ctx, status int, heading string, lines ...string) error {
	return a.render(c, status, "message", heading, fiber.Map{"Heading": heading, "Lines": lines})
}

func (a *App) serverError(c *fiber.Ctx) error {
	return a.message(c, fiber.StatusInternalServerError, "Something went wrong", "The request failed. Try again in a few minutes.")
}

func (a *App) clientIP(c *fiber.Ctx) string {
	peer := netip.Addr{}
	if ap, err := netip.ParseAddrPort(c.Context().RemoteAddr().String()); err == nil {
		peer = ap.Addr()
	}
	return auth.ClientIP(peer, c.Get("X-Forwarded-For"), a.TrustedProxies)
}

func (a *App) home(c *fiber.Ctx) error {
	return a.render(c, fiber.StatusOK, "home", "Waiting list", nil)
}

func (a *App) joinWaitlist(c *fiber.Ctx) error {
	const done = "SanHolo will email this address when a place opens."
	ok, err := a.Auth.Allow(c.UserContext(), "waitlist:ip:"+a.clientIP(c), waitlistLimit, time.Hour)
	if err != nil {
		return a.serverError(c)
	}
	if !ok {
		// Over the limit: the same page, nothing stored.
		return a.message(c, fiber.StatusOK, "You are on the list", done)
	}
	if _, err := auth.ValidateEmail(c.FormValue("email")); err != nil {
		return a.message(c, fiber.StatusBadRequest, "Check the email address", "That is not a valid email address.")
	}
	if err := a.Auth.JoinWaitlist(c.UserContext(), c.FormValue("email")); err != nil {
		return a.serverError(c)
	}
	// Same page whether or not the address was already listed.
	return a.message(c, fiber.StatusOK, "You are on the list", done)
}

func (a *App) signupForm(c *fiber.Ctx) error {
	code := c.Query("code")
	if code == "" {
		return a.message(c, fiber.StatusForbidden, "Sign-up needs an invite", "The beta is invite-only.")
	}
	email, err := a.Auth.InviteEmail(c.UserContext(), code)
	if err != nil {
		return a.inviteError(c, err)
	}
	return a.render(c, fiber.StatusOK, "signup", "Create your account", fiber.Map{"Code": code, "Email": email})
}

func (a *App) signup(c *fiber.Ctx) error {
	_, err := a.Auth.Redeem(c.UserContext(), c.FormValue("code"))
	if err != nil {
		return a.inviteError(c, err)
	}
	return a.render(c, fiber.StatusOK, "message", "Account created", fiber.Map{
		"Heading": "Account created",
		"Lines":   []string{"Sign in with your email address to continue."},
		"Link":    "/signin", "LinkText": "Sign in",
	})
}

func (a *App) inviteError(c *fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, auth.ErrInviteRequired), errors.Is(err, auth.ErrInviteInvalid):
		return a.message(c, fiber.StatusForbidden, "Sign-up needs an invite", "This invite code is not valid.")
	case errors.Is(err, auth.ErrInviteUsed):
		return a.message(c, fiber.StatusGone, "Invite already used", "This invite has already been used.")
	case errors.Is(err, auth.ErrInviteExpired):
		return a.message(c, fiber.StatusGone, "Invite expired", "This invite has expired. Ask for a new one.")
	case errors.Is(err, auth.ErrBetaFull):
		return a.message(c, fiber.StatusServiceUnavailable, "The beta is full", "No places are free. Nothing was created.")
	case errors.Is(err, auth.ErrAccountExists):
		return a.message(c, fiber.StatusConflict, "Account exists", "An account already exists for this address. Sign in instead.")
	}
	return a.serverError(c)
}

func (a *App) signinForm(c *fiber.Ctx) error {
	return a.render(c, fiber.StatusOK, "signin", "Sign in", nil)
}

func (a *App) signin(c *fiber.Ctx) error {
	err := a.Auth.RequestSignIn(c.UserContext(), c.FormValue("email"), a.clientIP(c))
	switch {
	case errors.Is(err, auth.ErrRateLimited):
		return a.message(c, fiber.StatusTooManyRequests, "Too many requests", "Wait an hour before asking for another link.")
	case err != nil && isInputError(err):
		return a.message(c, fiber.StatusBadRequest, "Check the email address", "That is not a valid email address.")
	case err != nil:
		return a.serverError(c)
	}
	// One answer for every valid address, known or not.
	return a.message(c, fiber.StatusOK, "Check your email",
		"If an account exists for this address, a sign-in link is on its way.",
		"The link works once. It expires in 15 minutes.")
}

func isInputError(err error) bool { return strings.HasPrefix(err.Error(), "email:") }

// confirmPage answers GET on the emailed link. It spends nothing: mail
// scanners, link unfurlers and prefetchers fetch links with GET (and HEAD),
// and must not burn a single-use token. The person presses one button, which
// POSTs the token (with the CSRF token) to verify.
func (a *App) confirmPage(c *fiber.Ctx) error {
	if c.Method() == fiber.MethodHead || isPrefetch(c) {
		return c.SendStatus(fiber.StatusNoContent)
	}
	return a.render(c, fiber.StatusOK, "confirm", "Sign in", fiber.Map{"Token": c.Query("token")})
}

// verify spends a sign-in link and opens a session.
func (a *App) verify(c *fiber.Ctx) error {
	token := c.FormValue("token")
	// A new session id on every sign-in; a cookie the browser already holds
	// is revoked, never reused (session fixation).
	if old := c.Cookies(sessionCookie); old != "" {
		_ = a.Auth.RevokeSecret(c.UserContext(), old)
	}
	secret, expires, err := a.Auth.ConsumeMagicLink(c.UserContext(), token, c.Get("User-Agent"))
	if errors.Is(err, auth.ErrLinkInvalid) {
		return a.render(c, fiber.StatusGone, "message", "Link not valid", fiber.Map{
			"Heading": "Link not valid",
			"Lines":   []string{"This sign-in link was used or has expired."},
			"Link":    "/signin", "LinkText": "Ask for a new link",
		})
	}
	if err != nil {
		return a.serverError(c)
	}
	setSessionCookie(c, secret, expires)
	return c.Redirect("/account", fiber.StatusSeeOther)
}

func isPrefetch(c *fiber.Ctx) bool {
	return strings.Contains(strings.ToLower(c.Get("Sec-Purpose")), "prefetch") ||
		strings.EqualFold(c.Get("Purpose"), "prefetch")
}

func setSessionCookie(c *fiber.Ctx, value string, expires time.Time) {
	c.Cookie(&fiber.Cookie{
		Name: sessionCookie, Value: value, Path: "/", // no Domain: host-only
		Expires: expires, MaxAge: int(time.Until(expires).Seconds()),
		Secure: true, HTTPOnly: true, SameSite: fiber.CookieSameSiteLaxMode,
	})
}

func clearSessionCookie(c *fiber.Ctx) {
	c.Cookie(&fiber.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, Expires: time.Unix(0, 0),
		Secure: true, HTTPOnly: true, SameSite: fiber.CookieSameSiteLaxMode,
	})
}

// requireSession loads the session for the cookie or sends the visitor to
// sign in.
func (a *App) requireSession(c *fiber.Ctx) error {
	ses, err := a.Auth.LookupSession(c.UserContext(), c.Cookies(sessionCookie))
	if errors.Is(err, auth.ErrNoSession) {
		clearSessionCookie(c)
		return c.Redirect("/signin", fiber.StatusSeeOther)
	}
	if err != nil {
		return a.serverError(c)
	}
	if ses.Refreshed {
		setSessionCookie(c, c.Cookies(sessionCookie), ses.ExpiresAt)
	}
	c.Locals("session", ses)
	return c.Next()
}

func (a *App) account(c *fiber.Ctx) error {
	ses := c.Locals("session").(auth.Session)
	list, err := a.Auth.ListSessions(c.UserContext(), ses.AccountID)
	if err != nil {
		return a.serverError(c)
	}
	type row struct {
		ID, UserAgent, LastSeen string
		Current                 bool
	}
	rows := make([]row, 0, len(list))
	for _, s := range list {
		rows = append(rows, row{ID: s.ID, UserAgent: s.UserAgent, LastSeen: s.LastSeenAt.UTC().Format("2006-01-02 15:04 UTC"), Current: s.ID == ses.ID})
	}
	return a.render(c, fiber.StatusOK, "account", "Account", fiber.Map{"Email": ses.Email, "Sessions": rows})
}

func (a *App) revoke(c *fiber.Ctx) error {
	ses := c.Locals("session").(auth.Session)
	id := c.Params("id")
	if _, err := a.Auth.RevokeSession(c.UserContext(), ses.AccountID, id); err != nil {
		return a.serverError(c)
	}
	if id == ses.ID {
		clearSessionCookie(c)
		return c.Redirect("/signin", fiber.StatusSeeOther)
	}
	return c.Redirect("/account", fiber.StatusSeeOther)
}

func (a *App) signout(c *fiber.Ctx) error {
	if err := a.Auth.RevokeSecret(c.UserContext(), c.Cookies(sessionCookie)); err != nil {
		return a.serverError(c)
	}
	clearSessionCookie(c)
	return c.Redirect("/signin", fiber.StatusSeeOther)
}
