package server

import (
	"errors"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/lhaig/sanholo-cloud/internal/auth"
	"github.com/lhaig/sanholo-cloud/internal/connect"
)

// gameCookie binds a validated, unconfirmed connection to the browser that
// validated it. The value is a random secret; the server keeps only its
// hash, and the token stays in server memory (see connect.PendingTTL).
const gameCookie = "__Host-sanholo_game"

const (
	gameValidateAccountLimit = 10
	gameValidateIPLimit      = 30
	gameConfirmLimit         = 20
)

func setGameCookie(c *fiber.Ctx, secret string) {
	c.Cookie(&fiber.Cookie{
		Name: gameCookie, Value: secret, Path: "/",
		MaxAge: int(connect.PendingTTL.Seconds()), Expires: time.Now().Add(connect.PendingTTL),
		Secure: true, HTTPOnly: true, SameSite: fiber.CookieSameSiteLaxMode,
	})
}

func clearGameCookie(c *fiber.Ctx) {
	c.Cookie(&fiber.Cookie{
		Name: gameCookie, Value: "", Path: "/", MaxAge: -1, Expires: time.Unix(0, 0),
		Secure: true, HTTPOnly: true, SameSite: fiber.CookieSameSiteLaxMode,
	})
}

func (a *App) gameNotAvailable(c *fiber.Ctx) error {
	return a.message(c, fiber.StatusServiceUnavailable, "Not available",
		"Connecting a game account is not available here.")
}

// gameValidate checks the token with one game read and, if it may be
// connected, shows the replicant for confirmation. The token is read from
// the POST body only, is never echoed, and is not logged.
func (a *App) gameValidate(c *fiber.Ctx) error {
	if !a.Connect.Available() {
		return a.gameNotAvailable(c)
	}
	ses := c.Locals("session").(auth.Session)
	// Both counters are always spent, so a client cannot dodge one by
	// exhausting the other first.
	okAcct, err := a.allow(c, "game:validate:account:"+ses.AccountID, gameValidateAccountLimit)
	if err != nil {
		return a.serverError(c)
	}
	okIP, err := a.allow(c, "game:validate:ip:"+a.clientIP(c), gameValidateIPLimit)
	if err != nil {
		return a.serverError(c)
	}
	if !okAcct || !okIP {
		return a.message(c, fiber.StatusTooManyRequests, "Too many requests", "Wait an hour before trying another token.")
	}
	prev, secret, err := a.Connect.Begin(c.UserContext(), ses.AccountID, ses.TenantID, strings.TrimSpace(c.FormValue("token")))
	if err != nil {
		return a.gameError(c, err)
	}
	setGameCookie(c, secret)
	return a.render(c, fiber.StatusOK, "game_confirm", "Confirm your game account", fiber.Map{
		"Name": prev.ReplicantName, "Code": prev.ReplicantCode, "Reconnect": prev.Reconnect,
	})
}

func (a *App) gameConfirm(c *fiber.Ctx) error {
	if !a.Connect.Available() {
		return a.gameNotAvailable(c)
	}
	ses := c.Locals("session").(auth.Session)
	ok, err := a.allow(c, "game:confirm:account:"+ses.AccountID, gameConfirmLimit)
	if err != nil {
		return a.serverError(c)
	}
	if !ok {
		return a.message(c, fiber.StatusTooManyRequests, "Too many requests", "Wait an hour before trying again.")
	}
	g, err := a.Connect.Confirm(c.UserContext(), ses.AccountID, ses.TenantID, c.Cookies(gameCookie))
	clearGameCookie(c)
	if err != nil {
		return a.gameError(c, err)
	}
	return a.render(c, fiber.StatusOK, "message", "Game account connected", fiber.Map{
		"Heading": "Game account connected",
		"Lines":   []string{"SanHolo is connected to the replicant " + g.ReplicantName + " (" + g.ReplicantCode + ")."},
		"Link":    "/account", "LinkText": "Back to your account",
	})
}

func (a *App) gameCancel(c *fiber.Ctx) error {
	ses := c.Locals("session").(auth.Session)
	a.Connect.Cancel(ses.AccountID, c.Cookies(gameCookie))
	clearGameCookie(c)
	return c.Redirect("/account", fiber.StatusSeeOther)
}

// gameError maps a connect error to fixed text. Nothing from the game, qm
// or the request is echoed.
func (a *App) gameError(c *fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, connect.ErrNotAvailable):
		return a.gameNotAvailable(c)
	case errors.Is(err, connect.ErrRejected):
		return a.message(c, fiber.StatusBadRequest, "Token not accepted", "The game did not accept this token.")
	case errors.Is(err, connect.ErrGameUnreachable):
		return a.message(c, fiber.StatusServiceUnavailable, "The game did not answer",
			"SanHolo could not reach the game. Nothing was saved. Try again in a few minutes.")
	case errors.Is(err, connect.ErrNoReplicant):
		return a.message(c, fiber.StatusUnprocessableEntity, "No replicant found",
			"This game account has no replicant yet. Create one in the game, then try again.")
	case errors.Is(err, connect.ErrTaken):
		return a.message(c, fiber.StatusConflict, "Game account in use",
			"This game account is already connected to a SanHolo account.")
	case errors.Is(err, connect.ErrLimit):
		return a.message(c, fiber.StatusConflict, "Beta limit", "Beta limit: one game account.")
	case errors.Is(err, connect.ErrExpired):
		return a.render(c, fiber.StatusGone, "message", "Confirmation expired", fiber.Map{
			"Heading": "Confirmation expired",
			"Lines":   []string{"The token was not kept. Enter it again to continue."},
			"Link":    "/account", "LinkText": "Back to your account",
		})
	case errors.Is(err, connect.ErrStore):
		return a.message(c, fiber.StatusServiceUnavailable, "Not saved",
			"SanHolo could not store the token. Nothing was saved. Enter the token again in a few minutes.")
	}
	return a.serverError(c)
}
