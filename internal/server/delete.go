package server

import (
	"errors"
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/lhaig/sanholo-cloud/internal/auth"
	"github.com/lhaig/sanholo-cloud/internal/connect"
)

const accountDeleteLimit = 10

// deleteConfirm is the confirmation step. It spends nothing (a GET may be
// prefetched), says what deleting does, and its form is what deletes.
func (a *App) deleteConfirm(c *fiber.Ctx) error {
	if c.Method() == fiber.MethodHead || isPrefetch(c) {
		return c.SendStatus(fiber.StatusNoContent)
	}
	if a.Connect == nil {
		return a.message(c, fiber.StatusServiceUnavailable, "Not available", "Deleting an account is not available here.")
	}
	return a.render(c, fiber.StatusOK, "account_delete", "Delete your account", nil)
}

// deleteAccount deletes the account after the customer typed its address,
// then signs the customer out.
func (a *App) deleteAccount(c *fiber.Ctx) error {
	if a.Connect == nil {
		return a.message(c, fiber.StatusServiceUnavailable, "Not available", "Deleting an account is not available here.")
	}
	ses := c.Locals("session").(auth.Session)
	ok, err := a.allow(c, "account:delete:account:"+ses.AccountID, accountDeleteLimit)
	if err != nil {
		return a.serverError(c)
	}
	if !ok {
		return a.message(c, fiber.StatusTooManyRequests, "Too many requests", "Wait an hour before trying again.")
	}
	res, err := a.Connect.DeleteAccount(c.UserContext(), ses.AccountID, ses.TenantID, c.FormValue("email"))
	switch {
	case errors.Is(err, connect.ErrEmailMismatch):
		return a.render(c, fiber.StatusBadRequest, "account_delete", "Delete your account", fiber.Map{
			"Error": "That is not the email address of this account. Nothing was changed.",
		})
	case errors.Is(err, connect.ErrNoAccount):
		return a.message(c, fiber.StatusConflict, "No change made", "There is no account to delete.")
	case errors.Is(err, connect.ErrLockTimeout):
		return a.message(c, fiber.StatusServiceUnavailable, "Try again in a minute",
			"Your bot is being started or changed right now. Nothing was changed.")
	case errors.Is(err, connect.ErrStopFailed), errors.Is(err, connect.ErrLog):
		// The account is marked as being deleted and the Bridge is closed;
		// the background pass finishes the deletion.
		return a.message(c, fiber.StatusBadGateway, "Deletion started",
			"SanHolo could not finish the deletion yet. Your account is marked as being deleted and your Bridge is closed.",
			"SanHolo retries by itself within a few minutes. You can also press Delete again later.")
	case err != nil:
		return a.serverError(c)
	}
	// The sessions went with the account; the cookie is cleared as well.
	clearSessionCookie(c)
	lines := []string{
		"Your account is deleted. SanHolo stopped your bot and deleted your account records.",
		"SanHolo emailed a confirmation to " + strings.TrimSpace(ses.Email) + ".",
	}
	if res.SecretPending {
		lines = append(lines, "Deleting your game token in the secrets store is not finished yet. SanHolo has recorded it and retries until it succeeds.")
	} else if a.Connect.Available() {
		lines = append(lines, "Your game token is deleted from the secrets store.")
	}
	if res.VolumePending {
		lines = append(lines, "Deleting your bot's data volume is not finished yet. SanHolo has recorded it and retries until it succeeds.")
	}
	lines = append(lines,
		"Your Replicant Space account is not deleted. Delete it in the game if you want it gone.",
		"Encrypted backups made before today keep your data until they expire, at most 30 days. A deleted account is not restored from a backup.")
	return a.render(c, fiber.StatusOK, "message", "Account deleted", fiber.Map{
		"Heading": "Account deleted", "Lines": lines, "Link": "/", "LinkText": "SanHolo home",
	})
}
