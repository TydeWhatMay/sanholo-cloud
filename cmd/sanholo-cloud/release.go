package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/lhaig/sanholo-cloud/internal/admin"
	"github.com/lhaig/sanholo-cloud/internal/auth"
	"github.com/lhaig/sanholo-cloud/internal/config"
	"github.com/lhaig/sanholo-cloud/internal/db"
	"github.com/lhaig/sanholo-cloud/internal/updater"
)

const releaseUsage = `usage:
  sanholo-cloud release add <version> [--digest sha256:...] [--notes text] [--url https://...]
  sanholo-cloud release list
  sanholo-cloud release start <version>    plan the rollout; the running service carries it out
  sanholo-cloud release resume             resume the halted rollout
  sanholo-cloud release abandon            give up the halted rollout
  sanholo-cloud release canary <tenant-id>|none`

// runRelease is `sanholo-cloud release ...`. It reads the same environment as
// the server and writes to the same database; the running service does the
// rolling. Releases are added here or on the admin page, never by polling
// GitHub.
func runRelease(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(releaseUsage)
	}
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		return err
	}
	a := auth.New(pool, auth.Options{MaxTenants: cfg.Beta.MaxTenants, BaseURL: cfg.AppBaseURL})
	svc := admin.New(pool, a)
	svc.SetUpdater(updater.New(pool, nil, updater.Options{BatchSize: cfg.Nomad.RolloutBatch}))
	return releaseCmd(ctx, svc, args, os.Stdout)
}

// releaseCmd applies the command to svc. The audit actor is always "cli".
func releaseCmd(ctx context.Context, svc *admin.Service, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(releaseUsage)
	}
	rest := args[1:]
	switch args[0] {
	case "add":
		fs := flag.NewFlagSet("release add", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		digest := fs.String("digest", "", "")
		notes := fs.String("notes", "", "")
		link := fs.String("url", "", "")
		// The version comes first; flags may follow it.
		if len(rest) == 0 || strings.HasPrefix(rest[0], "-") {
			return errors.New(releaseUsage)
		}
		version := rest[0]
		if err := fs.Parse(rest[1:]); err != nil || fs.NArg() != 0 {
			return errors.New(releaseUsage)
		}
		if err := svc.AddRelease(ctx, admin.ActorCLI, version, *digest, *notes, *link); err != nil {
			return releaseErr(err, version)
		}
		_, _ = fmt.Fprintf(out, "release %s added\n", version)
	case "list":
		if len(rest) != 0 {
			return errors.New(releaseUsage)
		}
		list, err := svc.Releases(ctx)
		if err != nil {
			return err
		}
		for _, r := range list {
			mark := ""
			if r.IsTarget {
				mark = " (target)"
			}
			pin := "tag"
			if r.Digest != "" {
				pin = r.Digest
			}
			_, _ = fmt.Fprintf(out, "v%s\t%s%s\t%s\n", r.Version, r.State, mark, pin)
		}
	case "start":
		if len(rest) != 1 {
			return errors.New(releaseUsage)
		}
		if err := svc.StartRollout(ctx, admin.ActorCLI, strings.TrimPrefix(rest[0], "v")); err != nil {
			return releaseErr(err, rest[0])
		}
		_, _ = fmt.Fprintf(out, "rollout of %s planned; the running service carries it out\n", rest[0])
	case "resume":
		if len(rest) != 0 {
			return errors.New(releaseUsage)
		}
		if err := svc.ResumeRollout(ctx, admin.ActorCLI); err != nil {
			return releaseErr(err, "")
		}
		_, _ = fmt.Fprintln(out, "rollout resumed")
	case "abandon":
		if len(rest) != 0 {
			return errors.New(releaseUsage)
		}
		if err := svc.AbandonRollout(ctx, admin.ActorCLI); err != nil {
			return releaseErr(err, "")
		}
		_, _ = fmt.Fprintln(out, "rollout abandoned")
	case "canary":
		if len(rest) != 1 {
			return errors.New(releaseUsage)
		}
		tenantID := rest[0]
		if tenantID == "none" {
			tenantID = ""
		}
		if err := svc.SetCanary(ctx, admin.ActorCLI, tenantID); err != nil {
			return releaseErr(err, rest[0])
		}
		_, _ = fmt.Fprintf(out, "canary tenant set to %s\n", rest[0])
	default:
		return errors.New(releaseUsage)
	}
	return nil
}

func releaseErr(err error, what string) error {
	switch {
	case errors.Is(err, admin.ErrNotFound):
		return fmt.Errorf("%s: not found", what)
	case errors.Is(err, admin.ErrReleaseExists):
		return fmt.Errorf("release %s already exists", what)
	case errors.Is(err, admin.ErrReleaseInvalid):
		return fmt.Errorf("release %s: %w", what, err)
	case errors.Is(err, admin.ErrRolloutActive):
		return errors.New("another rollout is rolling or halted: resume or abandon it first")
	case errors.Is(err, admin.ErrCanaryUnhealthy):
		return errors.New("the canary tenant failed its health check in the last hour: start the rollout when it has been healthy for an hour")
	case errors.Is(err, admin.ErrState):
		return errors.New("not applicable in the current state (the release is already rolling, or there is no halted rollout, or a revert is still under way)")
	}
	return err
}
