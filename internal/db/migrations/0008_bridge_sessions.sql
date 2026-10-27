-- Access to a tenant's Bridge (US-015). The Bridge lives on its own origin,
-- <tenant-id>.<domain>, so the account session cookie (host-only on the app
-- origin) never reaches it. A signed-in person gets there through a one-time
-- handoff code, which the tenant origin swaps for a Bridge session cookie.
-- Secrets are stored as SHA-256 hex only, like sessions and sign-in links.

-- A handoff code: single use, 60 seconds, bound to one account session and
-- the tenant of that account (reached through the account; like the other
-- sign-in tables these carry no tenant_id column of their own).
CREATE TABLE bridge_handoffs (
    code_hash          text PRIMARY KEY,
    account_session_id uuid NOT NULL REFERENCES account_sessions (id) ON DELETE CASCADE,
    created_at         timestamptz NOT NULL DEFAULT now(),
    expires_at         timestamptz NOT NULL,
    used_at            timestamptz
);

-- A Bridge session. It dies with the account session it came from (revoking
-- that session, signing out, or deleting the account removes the row), and a
-- lookup refuses it once the tenant is suspended or the account session has
-- expired.
CREATE TABLE bridge_sessions (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    secret_hash        text NOT NULL UNIQUE,
    account_session_id uuid NOT NULL REFERENCES account_sessions (id) ON DELETE CASCADE,
    created_at         timestamptz NOT NULL DEFAULT now(),
    expires_at         timestamptz NOT NULL
);
CREATE INDEX bridge_sessions_account_session_idx ON bridge_sessions (account_session_id);
