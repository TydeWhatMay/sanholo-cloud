package server

import (
	"errors"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/lhaig/sanholo-cloud/internal/auth"
	"github.com/lhaig/sanholo-cloud/internal/connect"
	"github.com/lhaig/sanholo-cloud/internal/provision"
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
	lines := []string{"SanHolo is connected to the replicant " + g.ReplicantName + " (" + g.ReplicantCode + ")."}
	if a.Provision.Enabled() {
		// After the commit, in the background: the account page shows the
		// bot starting. A new token also restarts a running bot, which reads
		// the token from the secrets store when it starts.
		if g.Reconnect {
			a.Provision.TriggerRestart(ses.TenantID)
			lines = append(lines, "Your bot is restarting with the new token.")
		} else {
			a.Provision.Trigger(ses.TenantID)
			lines = append(lines, "Your bot is starting. The account page shows its state.")
		}
	}
	return a.render(c, fiber.StatusOK, "message", "Game account connected", fiber.Map{
		"Heading": "Game account connected",
		"Lines":   lines,
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

const instanceRetryLimit = 10

// instanceView is what the account page shows of the tenant's bot: a fixed
// label, a fixed detail, and whether the page should keep polling.
func (a *App) instanceView(c *fiber.Ctx) (fiber.Map, error) {
	ses := c.Locals("session").(auth.Session)
	if !a.Provision.Enabled() {
		return fiber.Map{"Label": "Not provisioned", "Detail": "Running bots is not set up here."}, nil
	}
	inst, err := a.Provision.Get(c.UserContext(), ses.TenantID)
	if err != nil {
		return nil, err
	}
	if inst == nil {
		return fiber.Map{"Label": "Not started"}, nil
	}
	// Version is the release the bot runs, as the updater last set it.
	switch inst.State {
	case provision.StatePending:
		return fiber.Map{"Label": "Starting", "Detail": "This can take a few minutes. The page updates by itself.", "Starting": true, "Version": inst.ImageVersion}, nil
	case provision.StateRunning:
		if inst.Health == provision.HealthUnhealthy {
			return fiber.Map{"Label": "Running, not answering", "Detail": "The bot is not passing its health check. SanHolo is watching it.", "Open": true, "Version": inst.ImageVersion}, nil
		}
		return fiber.Map{"Label": "Running", "Open": true, "Version": inst.ImageVersion}, nil
	case provision.StateFailed:
		return fiber.Map{"Label": "Failed", "Detail": inst.FailureReason + ".", "CanRetry": true, "Version": inst.ImageVersion}, nil
	}
	return fiber.Map{"Label": "Stopped", "Version": inst.ImageVersion}, nil
}

// instanceRetry starts a failed bot again. Only a failed instance can be
// retried; a stopped one was stopped on purpose.
func (a *App) instanceRetry(c *fiber.Ctx) error {
	ses := c.Locals("session").(auth.Session)
	if !a.Provision.Enabled() {
		return c.Redirect("/account", fiber.StatusSeeOther)
	}
	ok, err := a.allow(c, "instance:retry:account:"+ses.AccountID, instanceRetryLimit)
	if err != nil {
		return a.serverError(c)
	}
	if !ok {
		return a.message(c, fiber.StatusTooManyRequests, "Too many requests", "Wait an hour before trying again.")
	}
	inst, err := a.Provision.Get(c.UserContext(), ses.TenantID)
	if err != nil {
		return a.serverError(c)
	}
	if inst == nil || inst.State != provision.StateFailed {
		return a.message(c, fiber.StatusConflict, "No change made", "Your bot is not in a failed state.")
	}
	// A failed deployment leaves the same job in Nomad, and registering an
	// unchanged job is skipped, so a plain start would do nothing: the retry
	// takes the restart path, which registers the job again.
	a.Provision.TriggerRestart(ses.TenantID)
	return c.Redirect("/account", fiber.StatusSeeOther)
}

const gameDisconnectLimit = 10

// gameDisconnectConfirm is the confirmation step: it spends nothing (a GET
// may be prefetched), it says what disconnecting does, and its form is what
// disconnects.
func (a *App) gameDisconnectConfirm(c *fiber.Ctx) error {
	if c.Method() == fiber.MethodHead || isPrefetch(c) {
		return c.SendStatus(fiber.StatusNoContent)
	}
	ses := c.Locals("session").(auth.Session)
	g, err := a.Connect.Get(c.UserContext(), ses.TenantID)
	if err != nil {
		return a.serverError(c)
	}
	if g == nil {
		return a.message(c, fiber.StatusConflict, "No change made", "No game account is connected.")
	}
	return a.render(c, fiber.StatusOK, "game_disconnect", "Disconnect your game account", fiber.Map{
		"Name": g.ReplicantName, "Code": g.ReplicantCode,
	})
}

// gameDisconnect stops the bot and removes the game account from SanHolo.
func (a *App) gameDisconnect(c *fiber.Ctx) error {
	ses := c.Locals("session").(auth.Session)
	ok, err := a.allow(c, "game:disconnect:account:"+ses.AccountID, gameDisconnectLimit)
	if err != nil {
		return a.serverError(c)
	}
	if !ok {
		return a.message(c, fiber.StatusTooManyRequests, "Too many requests", "Wait an hour before trying again.")
	}
	res, err := a.Connect.Disconnect(c.UserContext(), ses.AccountID, ses.TenantID)
	switch {
	case errors.Is(err, connect.ErrNotConnected):
		return a.message(c, fiber.StatusConflict, "No change made", "No game account is connected.")
	case errors.Is(err, connect.ErrStopFailed):
		return a.message(c, fiber.StatusBadGateway, "Not disconnected",
			"SanHolo could not stop your bot. Nothing was changed. Try again in a few minutes.")
	case err != nil:
		return a.serverError(c)
	}
	lines := []string{"Your game account is disconnected and your bot is stopped. Your SanHolo account is unchanged."}
	if res.SecretPending || res.VolumePending {
		what := []string{}
		if res.SecretPending {
			what = append(what, "your token in the secrets store")
		}
		if res.VolumePending {
			what = append(what, "your bot's data volume")
		}
		lines = append(lines, "Deleting "+strings.Join(what, " and ")+" is not finished yet. SanHolo has recorded it and retries until it succeeds.")
	} else {
		lines = append(lines, "Your token and your bot's data volume are deleted.")
	}
	return a.render(c, fiber.StatusOK, "message", "Game account disconnected", fiber.Map{
		"Heading": "Game account disconnected",
		"Lines":   lines,
		"Link":    "/account", "LinkText": "Back to your account",
	})
}
