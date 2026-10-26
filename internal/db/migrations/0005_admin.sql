-- Operator accounts, tenant suspension, invite revocation and the audit log.

-- Granted only by the CLI (sanholo-cloud operator grant), never from the web.
ALTER TABLE accounts ADD COLUMN is_operator boolean NOT NULL DEFAULT false;

-- A suspended tenant remembers the status to return to.
ALTER TABLE tenants DROP CONSTRAINT tenants_status_known;
ALTER TABLE tenants ADD CONSTRAINT tenants_status_known
    CHECK (status IN ('pending', 'active', 'suspended', 'deleting'));
ALTER TABLE tenants ADD COLUMN status_before_suspend text
    CONSTRAINT tenants_prev_status_known CHECK (status_before_suspend IN ('pending', 'active'));

-- A resent invite revokes the one before it.
ALTER TABLE invites ADD COLUMN revoked_at timestamptz;

-- One row per admin action, written in the same transaction as the change.
-- actor is an account id or 'cli'. details holds no secrets: never an invite
-- code, a token or a session id. No tenant_id column: operator records are
-- not customer data and outlive a tenant.
CREATE TABLE audit_log (
    id      bigserial PRIMARY KEY,
    at      timestamptz NOT NULL DEFAULT now(),
    actor   text NOT NULL CONSTRAINT audit_log_actor_nonempty CHECK (actor <> ''),
    action  text NOT NULL,
    target  text NOT NULL DEFAULT '',
    details jsonb NOT NULL DEFAULT '{}'
);
CREATE INDEX audit_log_at_idx ON audit_log (at DESC);
