package server

import (
	"errors"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/lhaig/sanholo-cloud/internal/auth"
)

// challengeCookie binds a passkey ceremony to the browser that started it.
// The value is a random secret; the server keeps only its hash. __Host-
// makes the browser refuse it unless it is Secure, host-only and Path=/.
const challengeCookie = "__Host-sanholo_wa"

const (
	passkeyBeginLimit = 30
	passkeyRegLimit   = 20
)

func setChallengeCookie(c *fiber.Ctx, secret string) {
	c.Cookie(&fiber.Cookie{
		Name: challengeCookie, Value: secret, Path: "/",
		MaxAge: int(auth.ChallengeTTL.Seconds()), Expires: time.Now().Add(auth.ChallengeTTL),
		Secure: true, HTTPOnly: true, SameSite: fiber.CookieSameSiteLaxMode,
	})
}

func clearChallengeCookie(c *fiber.Ctx) {
	c.Cookie(&fiber.Cookie{
		Name: challengeCookie, Value: "", Path: "/", MaxAge: -1, Expires: time.Unix(0, 0),
		Secure: true, HTTPOnly: true, SameSite: fiber.CookieSameSiteLaxMode,
	})
}

func jsonError(c *fiber.Ctx, status int, msg string) error {
	return c.Status(status).JSON(fiber.Map{"error": msg})
}

// passkeyError maps a service error to a JSON answer that says nothing about
// which check failed.
func passkeyError(c *fiber.Ctx, err error, failStatus int) error {
	switch {
	case errors.Is(err, auth.ErrPasskeysDisabled):
		return c.SendStatus(fiber.StatusNotFound)
	case errors.Is(err, auth.ErrPasskeyChallenge):
		return jsonError(c, fiber.StatusBadRequest, "The request expired. Start again.")
	case errors.Is(err, auth.ErrPasskeyFailed):
		return jsonError(c, failStatus, "The passkey was not accepted.")
	}
	return jsonError(c, fiber.StatusInternalServerError, "The request failed. Try again in a few minutes.")
}

func (a *App) allow(c *fiber.Ctx, key string, limit int) (bool, error) {
	return a.Auth.Allow(c.UserContext(), key, limit, time.Hour)
}

func (a *App) passkeyRegisterBegin(c *fiber.Ctx) error {
	if !a.Auth.PasskeysEnabled() {
		return c.SendStatus(fiber.StatusNotFound)
	}
	ses := c.Locals("session").(auth.Session)
	ok, err := a.allow(c, "passkey:reg:"+ses.AccountID, passkeyRegLimit)
	if err != nil {
		return passkeyError(c, err, 0)
	}
	if !ok {
		return jsonError(c, fiber.StatusTooManyRequests, "Too many requests. Wait an hour.")
	}
	opts, secret, err := a.Auth.BeginPasskeyRegistration(c.UserContext(), ses.AccountID)
	if err != nil {
		return passkeyError(c, err, fiber.StatusBadRequest)
	}
	setChallengeCookie(c, secret)
	c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	return c.Send(opts)
}

func (a *App) passkeyRegisterFinish(c *fiber.Ctx) error {
	if !a.Auth.PasskeysEnabled() {
		return c.SendStatus(fiber.StatusNotFound)
	}
	ses := c.Locals("session").(auth.Session)
	_, err := a.Auth.FinishPasskeyRegistration(c.UserContext(), ses.AccountID, c.Cookies(challengeCookie), c.Body())
	clearChallengeCookie(c)
	if err != nil {
		return passkeyError(c, err, fiber.StatusBadRequest)
	}
	return c.JSON(fiber.Map{"redirect": "/account"})
}

func (a *App) passkeyLoginBegin(c *fiber.Ctx) error {
	if !a.Auth.PasskeysEnabled() {
		return c.SendStatus(fiber.StatusNotFound)
	}
	ok, err := a.allow(c, "passkey:ip:"+a.clientIP(c), passkeyBeginLimit)
	if err != nil {
		return passkeyError(c, err, 0)
	}
	if !ok {
		return jsonError(c, fiber.StatusTooManyRequests, "Too many requests. Wait an hour.")
	}
	opts, secret, err := a.Auth.BeginPasskeyLogin(c.UserContext())
	if err != nil {
		return passkeyError(c, err, fiber.StatusBadRequest)
	}
	setChallengeCookie(c, secret)
	c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	return c.Send(opts)
}

func (a *App) passkeyLoginFinish(c *fiber.Ctx) error {
	if !a.Auth.PasskeysEnabled() {
		return c.SendStatus(fiber.StatusNotFound)
	}
	// A new session id on every sign-in, as with the magic link.
	if old := c.Cookies(sessionCookie); old != "" {
		_ = a.Auth.RevokeSecret(c.UserContext(), old)
	}
	secret, expires, err := a.Auth.FinishPasskeyLogin(c.UserContext(), c.Cookies(challengeCookie), c.Body(), c.Get("User-Agent"))
	clearChallengeCookie(c)
	if err != nil {
		return passkeyError(c, err, fiber.StatusUnauthorized)
	}
	setSessionCookie(c, secret, expires)
	return c.JSON(fiber.Map{"redirect": "/account"})
}

func (a *App) passkeyDelete(c *fiber.Ctx) error {
	ses := c.Locals("session").(auth.Session)
	if _, err := a.Auth.DeletePasskey(c.UserContext(), ses.AccountID, c.Params("id")); err != nil {
		return a.serverError(c)
	}
	return c.Redirect("/account", fiber.StatusSeeOther)
}
