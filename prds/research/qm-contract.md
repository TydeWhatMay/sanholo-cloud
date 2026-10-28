# qm integration contract (decisions, 2026-10-27)

Source: Appendix A of `prds/research/prd-tenant-scoped-secrets.md` in lhaig/qm,
plus the qm session's answers and Lance's decisions relayed on 2026-10-27.

## Control plane (provisioner)

- Auth: AppRole `sanholo-provisioner`. `POST /qm/v1/auth/approle/login
  {"role_id","secret_id"}` -> `auth.client_token`, sent as `X-QM-Token`;
  extend with `POST /qm/v1/auth/token/renew-self`; log in again on expiry.
  role_id and secret_id are delivered out of band by Lance.
- Interim grants (Part E adds delete + sudo on the purge route when it is
  deployed): create, update on
  `secret/data/sanholo/tenants/*`; read on `secret/metadata/sanholo/tenants/*`
  (version numbers and times, never values). No list, no destroy, no sudo.
- Write: `POST /qm/v1/secret/data/<path>` body `{"data":{...}}`, reply flat
  `{"version":N,"created_time":"..."}`. A rotation is a new write; qm keeps
  16 versions and prunes the oldest.
- Metadata: `GET /qm/v1/secret/metadata/<path>`, reply flat
  `{"current_version",...,"versions":{"<n>":{"destroyed":bool}}}`.
- Health: `GET /qm/v1/sys/health`, no auth, always 200,
  `{"initialized":bool,"sealed":bool}`.
- Errors: every auth failure and policy denial is 403 `forbidden` (never
  401). Sealed: 503 `vault_sealed` on any barrier-backed request.
- Tenants are never enumerated from qm; SanHolo's database is the record.

## Deletion (qm Part E, ADR 0024 accepted 2026-10-28; not yet deployed)

- `DELETE /qm/v1/secret/metadata/<path>`: every version of one path. 200
  `{"destroyed_versions":[..]}` (flat, ascending); 204 if nothing existed.
- `DELETE /qm/v1/secret/purge/<prefix>`: the prefix and everything under it,
  aligned to "/". 200 `{"paths_purged":N}`; 204 if nothing existed. Over
  qm's per-purge path limit: 400, nothing deleted.
- No body and no parameter on either (any field is 400). Both need delete +
  sudo on the path. Redirects are not followed.
- 409 `kv_purge_incomplete`: the 5 s budget ran out; re-run the same call.
  409 `kv_purge_in_progress`: an overlapping purge is running.
- After a purge, data and metadata reads answer 204 and a new write starts
  at version 1.
- Rules (PRD A.6): finish or abandon a pending purge before re-onboarding a
  tenant id; never run an old purge's retry after re-onboarding; never
  write to a tenant while purging it. Backups of qm keep purged data until
  they age out; a qm restore needs `sanholo-cloud deletions reapply`.
- SanHolo uses the purge route only. Until production qm has it, the purge
  is refused (403) and stays pending. Part E deployed is a SanHolo launch
  gate (account deletion, US-018).

## Instance

A production qm run by Lance, shared with his other projects, not mcsvr1.
It does not exist yet; URL and provisioner credentials come from Lance.

## Workload (tenant job)

Nomad JWT login per Appendix A.4 with role `sanholo-tenant`, aud
`qm.sanholo`; reads `secret/data/sanholo/tenants/<id>/game`; 204 (empty body)
means no such path (offboarded), as does a reply without `data`
(soft-deleted); 404 means an unknown mount or route. Fields `api_token`, `replicant_code` (SanHolo's choice; qm
imposes none).

## Reply shapes (confirmed from qm handlers, 2026-10-27)

- Read (workload): `{"data":{"api_token":..,"replicant_code":..},"metadata":{...}}`,
  one `data` level. A soft-deleted current version replies with metadata only;
  a missing path is 404. Client fixed in san-holo `b80927e`.
- Login (approle and nomad share one handler):
  `{"auth":{"client_token","accessor","policies","lease_duration":<seconds>}}`.
  No `tenant` field yet (that is qm's unbuilt Part A).
- renew-self (optional `{"increment":"1h"}`):
  `{"auth":{"accessor","lease_duration"}}`, no new token; 403 means log in again.
- `auth/nomad/login` exists only once the operator mounts the jwt method
  at `auth/nomad` (Appendix A.1).
