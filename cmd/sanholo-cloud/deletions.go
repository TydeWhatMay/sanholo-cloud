package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lhaig/sanholo-cloud/internal/config"
	"github.com/lhaig/sanholo-cloud/internal/connect"
	"github.com/lhaig/sanholo-cloud/internal/db"
	"github.com/lhaig/sanholo-cloud/internal/deletionlog"
	"github.com/lhaig/sanholo-cloud/internal/game"
	"github.com/lhaig/sanholo-cloud/internal/nomad"
	"github.com/lhaig/sanholo-cloud/internal/provision"
	"github.com/lhaig/sanholo-cloud/internal/secrets"
)

const deletionsUsage = "usage: sanholo-cloud deletions reapply [<deletion log file>]"

// buildCore builds the services that delete and verify: the secrets client,
// provisioning and the connect service with the deletion log. The server and
// `deletions reapply` both use it.
func buildCore(ctx context.Context, cfg config.Config, pool *pgxpool.Pool) (*secrets.Client, *provision.Service, *connect.Service, error) {
	sec, err := secrets.New(secrets.Config{
		Addr: cfg.QM.Addr, RoleID: cfg.QM.RoleID, SecretID: cfg.QM.SecretID, SecretIDFile: cfg.QM.SecretIDFile,
		Token: cfg.QM.Token, TokenFile: cfg.QM.TokenFile,
	})
	if err != nil {
		return nil, nil, nil, err
	}
	if sec.Enabled() {
		if err := sec.Ready(ctx); err != nil {
			log.Printf("secrets: qm not ready at start: %v", err)
		}
	} else {
		log.Print("secrets: QM_ADDR not set; storing game tokens is disabled")
	}
	gc, err := game.New(game.Config{BaseURL: cfg.GameURL, Version: version})
	if err != nil {
		return nil, nil, nil, err
	}
	conn, err := connect.New(pool, sec, gc, slog.Default(), cfg.Beta.MaxGameAccountsPerTenant)
	if err != nil {
		return nil, nil, nil, err
	}
	// Provisioning: without NOMAD_ADDR the service is disabled and tenants
	// stay "not provisioned".
	nc, err := nomad.New(cfg.Nomad.Client)
	if err != nil {
		return nil, nil, nil, err
	}
	prov, err := provision.New(pool, nc, provision.Options{
		Settings: cfg.Nomad.Job, Volume: cfg.Nomad.Volume, Version: cfg.Nomad.Version, Timeout: cfg.Nomad.Timeout, Logger: slog.Default(),
	})
	if err != nil {
		return nil, nil, nil, err
	}
	conn.SetProvisioner(prov)
	conn.SetDeletionLog(deletionlog.New(cfg.DeletionLogPath))
	return sec, prov, conn, nil
}

// runDeletions is `sanholo-cloud deletions reapply [<log file>]`: after a
// restore from backup, delete again every account the deletion log records
// that the restored database still holds. It reads the same environment as
// the server. The log is the file DELETION_LOG_PATH names unless one is
// given.
func runDeletions(ctx context.Context, args []string, out io.Writer) error {
	if len(args) < 1 || len(args) > 2 || args[0] != "reapply" {
		return errors.New(deletionsUsage)
	}
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	path := cfg.DeletionLogPath
	if len(args) == 2 {
		path = args[1]
	}
	if path == "" {
		return errors.New("no deletion log: set DELETION_LOG_PATH or pass the file")
	}
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		return err
	}
	_, prov, conn, err := buildCore(ctx, cfg, pool)
	if err != nil {
		return err
	}
	defer prov.Close()
	return reapplyDeletions(ctx, conn, path, out)
}

// reapplyDeletions applies the log through conn and reports each tenant.
func reapplyDeletions(ctx context.Context, conn *connect.Service, path string, out io.Writer) error {
	entries, err := deletionlog.Read(path)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	var failed int
	for _, e := range entries {
		if seen[e.Tenant] {
			continue
		}
		seen[e.Tenant] = true
		res, err := conn.DeleteTenant(ctx, connect.ActorCLI, e.Tenant)
		switch {
		case errors.Is(err, connect.ErrNoAccount):
			_, _ = fmt.Fprintf(out, "%s: nothing to delete\n", e.Tenant)
		case err != nil:
			failed++
			_, _ = fmt.Fprintf(out, "%s: not deleted: %v\n", e.Tenant, err)
		default:
			_, _ = fmt.Fprintf(out, "%s: deleted again (check %s)\n", e.Tenant, res.Check.State)
		}
	}
	_, _ = fmt.Fprintf(out, "%d tenants in the log, %d failed\n", len(seen), failed)
	if failed > 0 {
		return fmt.Errorf("%d deletions failed; run the command again", failed)
	}
	return nil
}
