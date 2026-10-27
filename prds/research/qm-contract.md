# qm integration contract (decisions, 2026-10-27)

Source: Appendix A of `prds/research/prd-tenant-scoped-secrets.md` in lhaig/qm,
plus the qm session's answers and Lance's decisions relayed on 2026-10-27.

## Control plane (provisioner)

- Auth: AppRole `sanholo-provisioner`. `POST /qm/v1/auth/approle/login
  {"role_id","secret_id"}` -> `auth.client_token`, sent as `X-QM-Token`;
  extend with `POST /qm/v1/auth/token/renew-self`; log in again on expiry.
  role_id and secret_id are delivered out of band by Lance.
- Interim grants (until qm Part E ships): create, update on
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

## Deletion (qm Part E, provisional shapes, no date)

- `DELETE /qm/v1/secret/metadata/<path>`: every version and the metadata.
- `DELETE /qm/v1/secret/purge/<prefix>`: a whole tenant prefix in one
  audited operation. Both need delete + sudo on the matching path.
- Until it ships, offboarding = stop provisioning and stop the tenant job.
  Part E landing is a SanHolo launch gate (account deletion, US-018).

## Instance

A production qm run by Lance, shared with his other projects, not mcsvr1.
It does not exist yet; URL and provisioner credentials come from Lance.

## Workload (tenant job)

Nomad JWT login per Appendix A.4 with role `sanholo-tenant`, aud
`qm.sanholo`; reads `secret/data/sanholo/tenants/<id>/game`; 404 means
offboarded. Fields `api_token`, `replicant_code` (SanHolo's choice; qm
imposes none).
