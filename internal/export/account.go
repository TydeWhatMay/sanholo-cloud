package export

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
)

// accountDoc is account.json. Every field is a fact the customer gave or
// caused. Deliberately absent: invite codes and their hashes, session and
// link secrets and their hashes, passkey ids and key material, push keys and
// the path of a push endpoint (a secret of its own), the game token (the
// database has none) and the game account's internal identifier.
type accountDoc struct {
	ExportedAt        time.Time    `json:"exported_at"`
	Email             string       `json:"email"`
	CreatedAt         time.Time    `json:"created_at"`
	Invites           []inviteDoc  `json:"invites"`
	Sessions          []sessionDoc `json:"sessions"`
	Passkeys          []passkeyDoc `json:"passkeys"`
	PushSubscriptions []pushDoc    `json:"push_subscriptions"`
	GameAccount       *gameDoc     `json:"game_account"`
	Instance          *instanceDoc `json:"instance"`
}

type inviteDoc struct {
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	UsedAt    *time.Time `json:"used_at"`
	RevokedAt *time.Time `json:"revoked_at"`
}

type sessionDoc struct {
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	Method     string    `json:"method"`
	UserAgent  string    `json:"user_agent"`
}

type passkeyDoc struct {
	AddedAt    time.Time  `json:"added_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

type pushDoc struct {
	CreatedAt time.Time `json:"created_at"`
	// EndpointHost is the host of the browser vendor's push service. The rest
	// of the endpoint URL is a secret that lets anyone push to the browser.
	EndpointHost string `json:"endpoint_host"`
}

type gameDoc struct {
	ReplicantName string    `json:"replicant_name"`
	ReplicantCode string    `json:"replicant_code"`
	ConnectedAt   time.Time `json:"connected_at"`
}

type instanceDoc struct {
	State   string `json:"state"`
	Version string `json:"version"`
}

// accountDoc reads account.json's content inside tx.
func (s *Service) accountDoc(ctx context.Context, tx pgx.Tx, accountID, tenantID string, now time.Time) (accountDoc, error) {
	d := accountDoc{ExportedAt: now.UTC(), Invites: []inviteDoc{}, Sessions: []sessionDoc{}, Passkeys: []passkeyDoc{}, PushSubscriptions: []pushDoc{}}
	fail := func(what string, err error) (accountDoc, error) {
		return accountDoc{}, fmt.Errorf("export: %s: %w", what, err)
	}
	if err := tx.QueryRow(ctx, `SELECT email, created_at FROM accounts WHERE id = $1 AND tenant_id = $2`, accountID, tenantID).Scan(&d.Email, &d.CreatedAt); err != nil {
		return fail("account", err)
	}
	rows, err := tx.Query(ctx, `
		SELECT created_at, expires_at, used_at, revoked_at FROM invites WHERE lower(email) = lower($1) ORDER BY created_at, id`, d.Email)
	if err != nil {
		return fail("invites", err)
	}
	for rows.Next() {
		var i inviteDoc
		if err := rows.Scan(&i.CreatedAt, &i.ExpiresAt, &i.UsedAt, &i.RevokedAt); err != nil {
			rows.Close()
			return fail("invites", err)
		}
		d.Invites = append(d.Invites, i)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fail("invites", err)
	}
	if rows, err = tx.Query(ctx, `
		SELECT created_at, last_seen_at, expires_at, method, user_agent FROM account_sessions
		WHERE account_id = $1 AND expires_at > $2 ORDER BY created_at, id`, accountID, now); err != nil {
		return fail("sessions", err)
	}
	for rows.Next() {
		var x sessionDoc
		if err := rows.Scan(&x.CreatedAt, &x.LastSeenAt, &x.ExpiresAt, &x.Method, &x.UserAgent); err != nil {
			rows.Close()
			return fail("sessions", err)
		}
		d.Sessions = append(d.Sessions, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fail("sessions", err)
	}
	if rows, err = tx.Query(ctx, `SELECT created_at, last_used_at FROM passkeys WHERE account_id = $1 ORDER BY created_at`, accountID); err != nil {
		return fail("passkeys", err)
	}
	for rows.Next() {
		var p passkeyDoc
		if err := rows.Scan(&p.AddedAt, &p.LastUsedAt); err != nil {
			rows.Close()
			return fail("passkeys", err)
		}
		d.Passkeys = append(d.Passkeys, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fail("passkeys", err)
	}
	if rows, err = tx.Query(ctx, `SELECT created_at, endpoint FROM push_subscriptions WHERE account_id = $1 ORDER BY created_at, id`, accountID); err != nil {
		return fail("push subscriptions", err)
	}
	for rows.Next() {
		var p pushDoc
		var endpoint string
		if err := rows.Scan(&p.CreatedAt, &endpoint); err != nil {
			rows.Close()
			return fail("push subscriptions", err)
		}
		p.EndpointHost = endpointHost(endpoint)
		d.PushSubscriptions = append(d.PushSubscriptions, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fail("push subscriptions", err)
	}
	var g gameDoc
	switch err := tx.QueryRow(ctx, `SELECT replicant_name, replicant_code, connected_at FROM game_accounts WHERE tenant_id = $1`, tenantID).Scan(&g.ReplicantName, &g.ReplicantCode, &g.ConnectedAt); {
	case err == nil:
		d.GameAccount = &g
	case !errors.Is(err, pgx.ErrNoRows):
		return fail("game account", err)
	}
	var in instanceDoc
	switch err := tx.QueryRow(ctx, `SELECT state, image_version FROM tenant_instances WHERE tenant_id = $1`, tenantID).Scan(&in.State, &in.Version); {
	case err == nil:
		d.Instance = &in
	case !errors.Is(err, pgx.ErrNoRows):
		return fail("instance", err)
	}
	return d, nil
}

// endpointHost is the host name of a push endpoint, nothing else.
func endpointHost(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
