-- Client releases and their rollout to the fleet (US-021).
--
-- A release is the operator's decision that an image version may run; rows
-- are added by the command line or the admin page, never by polling GitHub.
-- digest, when present, makes the tenant job run image@digest instead of the
-- tag. state: available (added), rolling (a rollout is under way), halted
-- (a rollout stopped on a failure and waits for the operator) or done (every
-- eligible tenant was moved). is_target marks the fleet target: the version
-- a new or restarted-from-stopped instance starts at. It moves to a release
-- only when its rollout is done.
CREATE TABLE releases (
    version       text PRIMARY KEY
                  CONSTRAINT releases_version_semver CHECK (version ~ '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]{1,32})?$'),
    digest        text CONSTRAINT releases_digest_format CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
    notes         text NOT NULL DEFAULT '' CONSTRAINT releases_notes_len CHECK (char_length(notes) <= 2000),
    url           text NOT NULL DEFAULT '' CONSTRAINT releases_url_len CHECK (char_length(url) <= 300),
    state         text NOT NULL DEFAULT 'available'
                  CONSTRAINT releases_state_known CHECK (state IN ('available', 'rolling', 'halted', 'done')),
    is_target     boolean NOT NULL DEFAULT false,
    batch_size    integer NOT NULL DEFAULT 5 CONSTRAINT releases_batch_size_positive CHECK (batch_size > 0),
    -- Why a halted rollout stopped: one of a fixed set of phrases, never
    -- Nomad's text.
    halted_tenant text NOT NULL DEFAULT '',
    halt_reason   text NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    started_at    timestamptz,
    finished_at   timestamptz
);

-- One fleet target, and one rollout (rolling or halted) at a time: the
-- database refuses a second, whichever process asks.
CREATE UNIQUE INDEX releases_one_target ON releases (is_target) WHERE is_target;
CREATE UNIQUE INDEX releases_one_rollout ON releases ((true)) WHERE state IN ('rolling', 'halted');

-- Per-tenant progress of the rollout of one release. batch 0 is the canary.
-- state: pending, updating (registered, waiting for health), revert_pending
-- and reverting (going back to previous_version after a failure), done,
-- skipped (suspended, stopped or not running: see reason), reverted (failed
-- and back on the previous version) or failed (failed, and the revert did not
-- become healthy). reason is a fixed phrase. previous_* is what the instance
-- ran when the rollout was planned.
CREATE TABLE rollout_tenants (
    release_version  text NOT NULL REFERENCES releases (version) ON DELETE CASCADE,
    tenant_id        text NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    batch            integer NOT NULL CONSTRAINT rollout_tenants_batch_nonneg CHECK (batch >= 0),
    state            text NOT NULL DEFAULT 'pending'
                     CONSTRAINT rollout_tenants_state_known CHECK (state IN
                         ('pending', 'updating', 'revert_pending', 'reverting', 'done', 'skipped', 'reverted', 'failed')),
    reason           text NOT NULL DEFAULT '',
    previous_version text NOT NULL,
    previous_digest  text NOT NULL DEFAULT '',
    started_at       timestamptz,
    updated_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (release_version, tenant_id)
);
CREATE INDEX rollout_tenants_tenant_idx ON rollout_tenants (tenant_id);

-- The operator's canary tenant; at most one. With none marked, a rollout
-- uses the first eligible tenant by creation.
ALTER TABLE tenants ADD COLUMN is_canary boolean NOT NULL DEFAULT false;
CREATE UNIQUE INDEX tenants_one_canary ON tenants (is_canary) WHERE is_canary;

-- The digest an instance's job is pinned to; empty means the tag.
ALTER TABLE tenant_instances ADD COLUMN image_digest text NOT NULL DEFAULT '';
