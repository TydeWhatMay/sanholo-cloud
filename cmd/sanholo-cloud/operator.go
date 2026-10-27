package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/lhaig/sanholo-cloud/internal/admin"
	"github.com/lhaig/sanholo-cloud/internal/auth"
	"github.com/lhaig/sanholo-cloud/internal/config"
	"github.com/lhaig/sanholo-cloud/internal/db"
)

const operatorUsage = "usage: sanholo-cloud operator grant|revoke <email>"

// runOperator is `sanholo-cloud operator grant|revoke <email>`. It reads the
// same environment as the server and talks to the same database. Operator
// rights cannot be granted from the web: this command is the only way.
func runOperator(ctx context.Context, args []string) error {
	if len(args) != 2 {
		return errors.New(operatorUsage)
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
	return operatorCmd(ctx, admin.New(pool, a), args, os.Stdout)
}

// operatorCmd applies the command to svc. The audit actor is always "cli".
func operatorCmd(ctx context.Context, svc *admin.Service, args []string, out io.Writer) error {
	if len(args) != 2 {
		return errors.New(operatorUsage)
	}
	var on bool
	switch args[0] {
	case "grant":
		on = true
	case "revoke":
	default:
		return errors.New(operatorUsage)
	}
	err := svc.SetOperator(ctx, admin.ActorCLI, args[1], on)
	if errors.Is(err, admin.ErrNotFound) {
		return fmt.Errorf("no account has the address %s", args[1])
	}
	if errors.Is(err, admin.ErrNoPasskey) {
		return fmt.Errorf("%s has no passkey: sign in with an email link, add a passkey on the account page, then grant again", args[1])
	}
	if err != nil {
		return err
	}
	verb := map[bool]string{true: "granted to", false: "revoked from"}[on]
	_, _ = fmt.Fprintf(out, "operator rights %s %s\n", verb, args[1])
	return nil
}
