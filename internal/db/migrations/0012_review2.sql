-- Second review round.
--
-- restart_requested_at: when the control plane last restarted a tenant's bot.
-- Nomad counts such a restart in the task's restart count like a crash; the
-- updater's soak uses this to tell the two apart.
ALTER TABLE tenant_instances ADD COLUMN restart_requested_at timestamptz;

-- canary_override: the operator chose to proceed although the canary tenant
-- was unhealthy in the last hour (audited when set).
ALTER TABLE releases ADD COLUMN canary_override boolean NOT NULL DEFAULT false;
