-- The game account a tenant has connected (US-011). Only facts that are not
-- secret: the game's account identifier, the replicant's name and code, and
-- when it was connected. The API token is never stored here; it is written
-- to qm and nowhere else, and there is deliberately no column for it.
--
-- tenant_id is UNIQUE because the qm layout holds one game credential per
-- tenant (beta limit: one game account). game_account_id is UNIQUE across
-- tenants so that a race between two tenants cannot connect one game
-- account twice: the second insert fails.
CREATE TABLE game_accounts (
    id              bigserial PRIMARY KEY,
    tenant_id       text NOT NULL UNIQUE REFERENCES tenants (id) ON DELETE CASCADE,
    game_account_id text NOT NULL UNIQUE CONSTRAINT game_accounts_id_nonempty CHECK (game_account_id <> ''),
    replicant_name  text NOT NULL,
    replicant_code  text NOT NULL,
    connected_at    timestamptz NOT NULL DEFAULT now()
);
