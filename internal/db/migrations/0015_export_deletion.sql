-- Data export links (US-017) and account deletion records (US-018).

-- A download link for one account's export. Only the SHA-256 of the link
-- secret is stored. The link is single use (used_at) and expires after 24
-- hours; it belongs to one account, and another account's session is refused.
-- tenant_id lets account deletion find the rows.
CREATE TABLE export_links (
    token_hash text PRIMARY KEY,
    account_id uuid NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    tenant_id  text NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    used_at    timestamptz
);
CREATE INDEX export_links_account_idx ON export_links (account_id);
CREATE INDEX export_links_tenant_idx ON export_links (tenant_id);

-- One row per account deletion: the operator's record that a tenant is being
-- (or was) deleted and what the last verification found. Together with the
-- tenants row (status 'deleting', a random id and a creation date) it is the
-- tombstone that stays while a deletion is incomplete. It holds no email
-- address, no hash of one and nothing else personal. tenant_key is not named
-- tenant_id on purpose: it must survive the tenant's rows, which deletion
-- counts by tenant_id (same reasoning as pending_cleanups).
--
-- deleted_account is the random id of the deleted account; it is kept only
-- until the deletion completes, so that every verification pass can look for
-- rows keyed by it. It is no foreign key (the account row is gone) and is not
-- named account_id on purpose: account deletion deletes from, and counts in,
-- every table that has an account_id column (db.AccountTables).
--
-- check_state: unchecked, pending (the only things left are cleanups that are
-- recorded and retried), passed or failed (see check_detail, a fixed set of
-- phrases and table names). completed_at is set when the tombstone's tenants
-- row has been removed.
CREATE TABLE tenant_deletions (
    tenant_key   text PRIMARY KEY,
    deleted_account uuid,
    requested_at timestamptz NOT NULL,
    completed_at timestamptz,
    checked_at   timestamptz,
    check_state  text NOT NULL DEFAULT 'unchecked'
                 CONSTRAINT tenant_deletions_check_state_known CHECK (check_state IN ('unchecked', 'pending', 'passed', 'failed')),
    check_detail text NOT NULL DEFAULT ''
);
