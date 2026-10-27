package server

import (
	"bufio"
	"context"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/lhaig/sanholo-cloud/internal/auth"
	"github.com/lhaig/sanholo-cloud/internal/export"
)

// exportStreamTimeout bounds one archive, however slow the reader is.
const exportStreamTimeout = 45 * time.Minute

func (a *App) exportAvailable(c *fiber.Ctx) bool {
	if a.Export != nil {
		return true
	}
	_ = a.message(c, fiber.StatusServiceUnavailable, "Not available", "Exporting data is not available here.")
	return false
}

// exportCreate issues a download link, mails it and shows it once. The limit
// is per account and per day.
func (a *App) exportCreate(c *fiber.Ctx) error {
	if !a.exportAvailable(c) {
		return nil
	}
	ses := c.Locals("session").(auth.Session)
	ok, err := a.Auth.Allow(c.UserContext(), "export:account:"+ses.AccountID, export.DailyLimit, 24*time.Hour)
	if err != nil {
		return a.serverError(c)
	}
	if !ok {
		return a.message(c, fiber.StatusTooManyRequests, "Too many exports",
			"An account can ask for "+strconv.Itoa(export.DailyLimit)+" exports a day. Try again tomorrow.")
	}
	link, err := a.Export.CreateLink(c.UserContext(), ses.AccountID, ses.TenantID, ses.Email)
	switch {
	case errors.Is(err, export.ErrDeleting):
		return a.message(c, fiber.StatusConflict, "No export", "This account is being deleted.")
	case err != nil:
		return a.serverError(c)
	}
	return a.render(c, fiber.StatusOK, "export_created", "Your export is ready", fiber.Map{
		"Email": ses.Email, "Link": link.URL, "Expires": link.Expires.UTC().Format("2006-01-02 15:04 UTC"),
	})
}

// exportConfirm answers GET on the download link. It spends nothing: mail
// scanners, link unfurlers and prefetchers fetch links with GET (and HEAD),
// and must not burn a single-use link. The person presses one button, which
// POSTs the token to exportDownload. This is the /signin/verify pattern.
func (a *App) exportConfirm(c *fiber.Ctx) error {
	if c.Method() == fiber.MethodHead || isPrefetch(c) {
		return c.SendStatus(fiber.StatusNoContent)
	}
	if !a.exportAvailable(c) {
		return nil
	}
	ses := c.Locals("session").(auth.Session)
	token := c.Query("token")
	ok, err := a.Export.Valid(c.UserContext(), token, ses.AccountID)
	if err != nil {
		return a.serverError(c)
	}
	if !ok {
		return a.exportInvalid(c)
	}
	return a.render(c, fiber.StatusOK, "export_download", "Download your data", fiber.Map{"Token": token})
}

func (a *App) exportInvalid(c *fiber.Ctx) error {
	return a.render(c, fiber.StatusGone, "message", "Link not valid", fiber.Map{
		"Heading": "Link not valid",
		"Lines":   []string{"This download link was used, has expired, or belongs to another account. Ask for a new export on your account page."},
		"Link":    "/account", "LinkText": "Back to your account",
	})
}

// exportDownload spends the link and streams the archive. The archive is
// built while it is sent; nothing of it is held in memory beyond the buffers
// of the streams.
func (a *App) exportDownload(c *fiber.Ctx) error {
	if !a.exportAvailable(c) {
		return nil
	}
	ses := c.Locals("session").(auth.Session)
	d, err := a.Export.Open(c.UserContext(), c.FormValue("token"), ses.AccountID)
	switch {
	case errors.Is(err, export.ErrLinkInvalid):
		return a.exportInvalid(c)
	case errors.Is(err, export.ErrBusy):
		c.Set(fiber.HeaderRetryAfter, "60")
		return a.message(c, fiber.StatusServiceUnavailable, "Try again in a minute",
			"Many exports are running. The link has not been used. Press the button again in a minute.")
	case err != nil:
		return a.serverError(c)
	}
	c.Set(fiber.HeaderContentType, "application/zip")
	c.Set(fiber.HeaderContentDisposition, `attachment; filename="`+d.Filename()+`"`)
	c.Set(fiber.HeaderCacheControl, "no-store")
	base := context.WithoutCancel(c.UserContext())
	c.Context().SetBodyStreamWriter(func(w *bufio.Writer) {
		defer d.Close()
		ctx, cancel := context.WithTimeout(base, exportStreamTimeout)
		defer cancel()
		if err := d.WriteTo(ctx, w); err != nil {
			slog.Warn("export: the download stopped", "error", err)
		}
		_ = w.Flush()
	})
	return nil
}
