// Package audit writes rows of the audit log. Every writer passes the
// transaction of the change it records, so the row and the change commit
// together or not at all.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Write inserts one row inside tx. actor is an account id, "cli" or a system
// name; details must not hold secrets (no token, invite code or session id).
func Write(ctx context.Context, tx pgx.Tx, at time.Time, actor, action, target string, details map[string]any) error {
	if details == nil {
		details = map[string]any{}
	}
	raw, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("audit: details: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO audit_log (at, actor, action, target, details) VALUES ($1, $2, $3, $4, $5)`,
		at, actor, action, target, raw); err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	return nil
}
