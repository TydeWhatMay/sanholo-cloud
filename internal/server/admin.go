package server

import (
	"context"
	"errors"
	"net/url"

	"github.com/gofiber/fiber/v2"

	"github.com/lhaig/sanholo-cloud/internal/admin"
	"github.com/lhaig/sanholo-cloud/internal/auth"
)

const timeFmt = "2006-01-02 15:04 UTC"

// adminNotices are the confirmations a redirect can ask the page to show.
// Fixed text keyed by a short code: nothing from the query string is echoed.
var adminNotices = map[string]string{
	"invite":    "The invite email is queued.",
	"resend":    "A new invite email is queued. The earlier invite no longer works.",
	"suspend":   "The tenant is suspended.",
	"unsuspend": "The tenant is active again.",
}

func (a *App) mountAdmin(app *fiber.App) {
	if a.Admin == nil {
		return
	}
	adm := app.Group("/admin", a.requireSession, a.requireOperator, a.requireOperatorPasskey)
	adm.Get("/", a.adminTenants)
	adm.Get("/waitlist", a.adminWaitlist)
	adm.Get("/audit", a.adminAudit)
	adm.Post("/waitlist/invite", a.adminInvite("invite", a.Admin.IssueInvite))
	adm.Post("/waitlist/resend", a.adminInvite("resend", a.Admin.ResendInvite))
	adm.Post("/tenants/:id/suspend", a.adminTenantAction("suspend", a.Admin.Suspend))
	adm.Post("/tenants/:id/unsuspend", a.adminTenantAction("unsuspend", a.Admin.Unsuspend))
}

// requireOperator answers anyone who is not an operator exactly as it
// answers an address that does not exist. The flag is read from the database
// on every request, so a revoke applies at once.
func (a *App) requireOperator(c *fiber.Ctx) error {
	ses := c.Locals("session").(auth.Session)
	if !ses.IsOperator {
		return fiber.NewError(fiber.StatusNotFound, "Cannot "+c.Method()+" "+c.Path())
	}
	return c.Next()
}

// requireOperatorPasskey admits only a session opened with a passkey. An
// operator who arrived by magic link is asked to sign in with a passkey.
func (a *App) requireOperatorPasskey(c *fiber.Ctx) error {
	ses := c.Locals("session").(auth.Session)
	if ses.Method != auth.MethodPasskey {
		return a.render(c, fiber.StatusForbidden, "admin_passkey", "Admin sign-in", fiber.Map{"Passkeys": a.Auth.PasskeysEnabled()})
	}
	return c.Next()
}

func (a *App) adminTenants(c *fiber.Ctx) error {
	list, err := a.Admin.Tenants(c.UserContext())
	if err != nil {
		return a.serverError(c)
	}
	type row struct{ ID, Email, Created, Status string }
	rows := make([]row, 0, len(list))
	for _, t := range list {
		rows = append(rows, row{t.ID, t.Email, t.CreatedAt.UTC().Format(timeFmt), t.Status})
	}
	return a.adminPage(c, "admin_tenants", "Tenants", fiber.Map{"Tenants": rows})
}

func (a *App) adminWaitlist(c *fiber.Ctx) error {
	list, err := a.Admin.Waitlist(c.UserContext())
	if err != nil {
		return a.serverError(c)
	}
	type row struct {
		Email, Joined, Invite string
		CanIssue, CanResend   bool
	}
	rows := make([]row, 0, len(list))
	for _, w := range list {
		rows = append(rows, row{
			Email: w.Email, Joined: w.CreatedAt.UTC().Format(timeFmt), Invite: w.InviteState,
			CanIssue: w.InviteState == "none", CanResend: w.InviteState != "none" && w.InviteState != "used",
		})
	}
	return a.adminPage(c, "admin_waitlist", "Waiting list", fiber.Map{"Entries": rows})
}

func (a *App) adminAudit(c *fiber.Ctx) error {
	list, err := a.Admin.Audit(c.UserContext(), 200)
	if err != nil {
		return a.serverError(c)
	}
	type row struct{ At, Actor, Action, Target, Details string }
	rows := make([]row, 0, len(list))
	for _, e := range list {
		rows = append(rows, row{e.At.UTC().Format(timeFmt), e.Actor, e.Action, e.Target, e.Details})
	}
	return a.adminPage(c, "admin_audit", "Audit log", fiber.Map{"Entries": rows})
}

func (a *App) adminPage(c *fiber.Ctx, name, title string, data fiber.Map) error {
	data["Notice"] = adminNotices[c.Query("done")]
	return a.render(c, fiber.StatusOK, name, title, data)
}

func actorOf(c *fiber.Ctx) string { return c.Locals("session").(auth.Session).AccountID }

func (a *App) adminInvite(done string, do func(ctx context.Context, actor, email string) error) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if err := do(c.UserContext(), actorOf(c), c.FormValue("email")); err != nil {
			return a.adminError(c, err)
		}
		return c.Redirect("/admin/waitlist?done="+url.QueryEscape(done), fiber.StatusSeeOther)
	}
}

func (a *App) adminTenantAction(done string, do func(ctx context.Context, actor, tenantID string) error) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if err := do(c.UserContext(), actorOf(c), c.Params("id")); err != nil {
			return a.adminError(c, err)
		}
		return c.Redirect("/admin?done="+url.QueryEscape(done), fiber.StatusSeeOther)
	}
}

func (a *App) adminError(c *fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, admin.ErrNotFound):
		return a.message(c, fiber.StatusNotFound, "Not found", "No such tenant, address or invite.")
	case errors.Is(err, admin.ErrState):
		return a.message(c, fiber.StatusConflict, "No change made", "The tenant is not in a state where this applies.")
	case errors.Is(err, auth.ErrAccountExists):
		return a.message(c, fiber.StatusConflict, "No change made", "An account already exists for this address.")
	case isInputError(err):
		return a.message(c, fiber.StatusBadRequest, "Check the email address", "That is not a valid email address.")
	}
	return a.serverError(c)
}
