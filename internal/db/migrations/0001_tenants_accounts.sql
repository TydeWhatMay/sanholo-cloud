-- Convention: every tenant-owned table has a tenant_id column referencing
-- tenants(id). Account deletion (US-018) finds those tables by asking
-- information_schema for that column name, so never rename it.

CREATE TABLE tenants (
    id         text PRIMARY KEY
               CONSTRAINT tenants_id_rule CHECK (id ~ '^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$'),
    status     text NOT NULL DEFAULT 'pending'
               CONSTRAINT tenants_status_known CHECK (status IN ('pending', 'active', 'deleting')),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE accounts (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  text NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    email      text NOT NULL CONSTRAINT accounts_email_nonempty CHECK (email <> ''),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX accounts_tenant_id_idx ON accounts (tenant_id);
-- Case-insensitive uniqueness without the citext extension, which a
-- least-privilege database role may not be allowed to create.
CREATE UNIQUE INDEX accounts_email_lower_key ON accounts (lower(email));
