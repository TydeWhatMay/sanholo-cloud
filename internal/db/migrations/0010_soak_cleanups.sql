-- Soak, canary health and deferred cleanups (Phase 2 review).
--
-- last_unhealthy_at: the last time the control plane saw the instance
-- unhealthy (a failing live check, a failed deployment, a failed soak). The
-- updater refuses to start or finish a rollout while the canary tenant was
-- unhealthy in the last hour.
ALTER TABLE tenant_instances ADD COLUMN last_unhealthy_at timestamptz;

-- Soak of an updated tenant: when its deployment became healthy, and the
-- allocation and task restart count it had then. During the soak a failing
-- check, a rise in the restart count or another allocation fails the tenant.
ALTER TABLE rollout_tenants ADD COLUMN soak_started_at timestamptz;
ALTER TABLE rollout_tenants ADD COLUMN soak_alloc text NOT NULL DEFAULT '';
ALTER TABLE rollout_tenants ADD COLUMN soak_restarts integer NOT NULL DEFAULT 0;

-- Deletions that were asked for and could not be completed yet: the tenant's
-- game token in qm (until qm's purge endpoint ships, every purge is denied)
-- and the tenant's host volume (Nomad refuses while the stopped job still
-- holds it). A worker retries them. tenant_key is not named tenant_id on
-- purpose: it must survive the tenant's rows (account deletion counts rows
-- by tenant_id), and it holds an id, never personal data or a secret.
CREATE TABLE pending_cleanups (
    tenant_key      text NOT NULL,
    kind            text NOT NULL CONSTRAINT pending_cleanups_kind_known CHECK (kind IN ('secret', 'volume')),
    requested_at    timestamptz NOT NULL DEFAULT now(),
    attempts        integer NOT NULL DEFAULT 0,
    last_attempt_at timestamptz,
    -- one of a fixed set of phrases, never the remote system's text
    last_error      text NOT NULL DEFAULT '',
    PRIMARY KEY (tenant_key, kind)
);
