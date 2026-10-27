-- The Nomad job of a tenant (US-012): which job, which image version, and
-- what the control plane last saw of it. No secrets: the job carries none,
-- and failure_reason is one of a fixed set of phrases, never Nomad's text.
--
-- A tenant with no row here is "not provisioned" (no game account yet, or
-- Nomad not configured). state: pending (registered, not yet healthy),
-- running (healthy once), failed (see failure_reason) or stopped (stopped on
-- purpose). last_health is Nomad's reading of the current version.
CREATE TABLE tenant_instances (
    tenant_id      text PRIMARY KEY REFERENCES tenants (id) ON DELETE CASCADE,
    job_id         text NOT NULL,
    image_version  text NOT NULL,
    state          text NOT NULL
                   CONSTRAINT tenant_instances_state_known CHECK (state IN ('pending', 'running', 'failed', 'stopped')),
    last_health    text NOT NULL DEFAULT 'unknown'
                   CONSTRAINT tenant_instances_health_known CHECK (last_health IN ('unknown', 'starting', 'healthy', 'unhealthy')),
    last_health_at timestamptz,
    failure_reason text NOT NULL DEFAULT '',
    requested_at   timestamptz NOT NULL DEFAULT now(),
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);
