-- A retry or restart of an instance that has no running allocation must make
-- Nomad create a new job version (registering an identical spec is only a new
-- evaluation, and does not revive a dead or failed job). The counter goes into
-- the job meta as sanholo.restart, outside the spec hash, and every render of
-- the job carries the stored value.
ALTER TABLE tenant_instances ADD COLUMN restart_seq integer NOT NULL DEFAULT 0;
