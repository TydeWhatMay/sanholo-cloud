package server

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/lhaig/sanholo-cloud/internal/admin"
	"github.com/lhaig/sanholo-cloud/internal/auth"
	"github.com/lhaig/sanholo-cloud/internal/updater"
)

const timeFmt = "2006-01-02 15:04 UTC"

// adminNotices are the confirmations a redirect can ask the page to show.
// Fixed text keyed by a short code: nothing from the query string is echoed.
var adminNotices = map[string]string{
	"invite":    "The invite email is queued.",
	"resend":    "A new invite email is queued. The earlier invite no longer works.",
	"suspend":   "The tenant is suspended.",
	"unsuspend": "The tenant is active again.",
	"restart":   "The bot is restarting.",
	"release":   "The release is added.",
	"rollout":   "The rollout is started.",
	"resume":    "The rollout is resumed.",
	"abandon":   "The rollout is abandoned.",
	"canary":    "The canary tenant is set.",
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
	adm.Post("/tenants/:id/restart", a.adminTenantAction("restart", a.Admin.RestartInstance))
	adm.Get("/releases", a.adminReleases)
	adm.Post("/releases/add", a.adminReleaseAdd)
	adm.Post("/releases/canary", a.adminCanary)
	adm.Post("/releases/resume", a.adminRolloutAction("resume", func(ctx context.Context, actor, _ string) error { return a.Admin.ResumeRollout(ctx, actor) }))
	adm.Post("/releases/abandon", a.adminRolloutAction("abandon", func(ctx context.Context, actor, _ string) error { return a.Admin.AbandonRollout(ctx, actor) }))
	adm.Post("/releases/:version/start", a.adminRolloutAction("rollout", a.Admin.StartRollout))
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
	type row struct {
		ID, Email, Created, Status, Game string
		Instance, Health                 string
		CanRestart                       bool
	}
	rows := make([]row, 0, len(list))
	for _, t := range list {
		game := ""
		if t.GameCode != "" {
			game = t.GameName + " (" + t.GameCode + ")"
		}
		r := row{ID: t.ID, Email: t.Email, Created: t.CreatedAt.UTC().Format(timeFmt), Status: t.Status, Game: game}
		if t.InstanceState != "" {
			r.Instance = t.InstanceState + " (v" + t.InstanceVersion + ")"
			if t.InstanceReason != "" {
				r.Instance += ": " + t.InstanceReason
			}
			r.Health = t.InstanceHealth
			if t.InstanceHealthAt != nil {
				r.Health += ", " + t.InstanceHealthAt.UTC().Format(timeFmt)
			}
			r.CanRestart = t.InstanceState != "stopped"
		}
		rows = append(rows, r)
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
	case errors.Is(err, admin.ErrRestart):
		return a.message(c, fiber.StatusBadGateway, "Restart not carried out", "The restart was recorded, but Nomad did not carry it out. The instance state shows what happened.")
	case errors.Is(err, admin.ErrOwnTenant):
		return a.message(c, fiber.StatusConflict, "No change made", "You cannot suspend your own tenant.")
	case errors.Is(err, admin.ErrRolloutActive):
		return a.message(c, fiber.StatusConflict, "No change made", "Another rollout is rolling or halted. Finish it, or abandon it, first.")
	case errors.Is(err, admin.ErrReleaseExists):
		return a.message(c, fiber.StatusConflict, "No change made", "That release is already added.")
	case errors.Is(err, admin.ErrReleaseInvalid):
		return a.message(c, fiber.StatusBadRequest, "Check the release", "The version, digest, notes or link is not valid. A digest looks like sha256: followed by 64 hex digits, and a link starts with https://.")
	case errors.Is(err, auth.ErrAccountExists):
		return a.message(c, fiber.StatusConflict, "No change made", "An account already exists for this address.")
	case isInputError(err):
		return a.message(c, fiber.StatusBadRequest, "Check the email address", "That is not a valid email address.")
	}
	return a.serverError(c)
}

// releaseRow is one release on the releases page.
type releaseRow struct {
	Version, Digest, State, Added, Notes, URL string
	Target, CanStart, Halted                  bool
	HaltedTenant, HaltReason                  string
}

type rolloutRow struct {
	Tenant, State, Reason, From string
	Batch                       string
}

func (a *App) adminReleases(c *fiber.Ctx) error {
	list, err := a.Admin.Releases(c.UserContext())
	if err != nil {
		return a.serverError(c)
	}
	rows := make([]releaseRow, 0, len(list))
	target, shown := "none yet", ""
	var showRel *updater.Release
	for i, r := range list {
		row := releaseRow{
			Version: r.Version, Digest: r.Digest, State: r.State, Added: r.CreatedAt.UTC().Format(timeFmt),
			Notes: r.Notes, URL: r.URL, Target: r.IsTarget,
			CanStart: r.State == updater.StateAvailable || r.State == updater.StateDone,
			Halted:   r.State == updater.StateHalted, HaltedTenant: r.HaltedTenant, HaltReason: r.HaltReason,
		}
		if r.IsTarget {
			target = "v" + r.Version
		}
		// The rollout shown is the active one, else the one that made the
		// current target.
		if r.State == updater.StateRolling || r.State == updater.StateHalted || (showRel == nil && r.IsTarget) {
			showRel = &list[i]
		}
		rows = append(rows, row)
	}
	var progress []rolloutRow
	var halted bool
	if showRel != nil {
		shown = "v" + showRel.Version + " (" + showRel.State + ")"
		halted = showRel.State == updater.StateHalted
		pr, err := a.Admin.RolloutRows(c.UserContext(), showRel.Version)
		if err != nil {
			return a.serverError(c)
		}
		for _, r := range pr {
			batch := "batch " + strconv.Itoa(r.Batch)
			if r.Batch == 0 {
				batch = "canary"
			}
			progress = append(progress, rolloutRow{Tenant: r.TenantID, State: r.State, Reason: r.Reason, From: "v" + r.PreviousVersion, Batch: batch})
		}
	}
	return a.adminPage(c, "admin_releases", "Releases", fiber.Map{
		"Releases": rows, "Target": target, "Shown": shown, "Progress": progress, "Halted": halted,
	})
}

func (a *App) adminReleaseAdd(c *fiber.Ctx) error {
	if err := a.Admin.AddRelease(c.UserContext(), actorOf(c), c.FormValue("version"), c.FormValue("digest"), c.FormValue("notes"), c.FormValue("url")); err != nil {
		return a.adminError(c, err)
	}
	return c.Redirect("/admin/releases?done=release", fiber.StatusSeeOther)
}

func (a *App) adminCanary(c *fiber.Ctx) error {
	if err := a.Admin.SetCanary(c.UserContext(), actorOf(c), strings.TrimSpace(c.FormValue("tenant"))); err != nil {
		return a.adminError(c, err)
	}
	return c.Redirect("/admin/releases?done=canary", fiber.StatusSeeOther)
}

// adminRolloutAction runs a rollout action; the release version, when the
// action needs one, is the :version path parameter.
func (a *App) adminRolloutAction(done string, do func(ctx context.Context, actor, version string) error) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if err := do(c.UserContext(), actorOf(c), c.Params("version")); err != nil {
			return a.adminError(c, err)
		}
		return c.Redirect("/admin/releases?done="+url.QueryEscape(done), fiber.StatusSeeOther)
	}
}
