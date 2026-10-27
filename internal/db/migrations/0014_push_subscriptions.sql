-- Web Push subscriptions (US-016). One row per browser that pressed "Enable
-- notifications" on a tenant's Bridge. tenant_id lets account deletion
-- (US-018) find the rows. A subscription outlives the account session that
-- made it: it is removed when the person unsubscribes, when the browser's
-- push service reports it gone (404 or 410), or with the account.
CREATE TABLE push_subscriptions (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          text NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    account_id         uuid NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    account_session_id uuid REFERENCES account_sessions (id) ON DELETE SET NULL,
    endpoint           text NOT NULL CONSTRAINT push_endpoint_len CHECK (length(endpoint) <= 2048),
    p256dh             text NOT NULL,
    auth               text NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT push_subscriptions_endpoint_key UNIQUE (endpoint)
);
CREATE INDEX push_subscriptions_tenant_idx ON push_subscriptions (tenant_id);
