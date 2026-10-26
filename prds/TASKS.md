# TASKS — SanHolo Hosted (sanholo-cloud)

Single source of truth for task status. See `prds/config.md` for commands.
Imported from `prds/backlog/prd-sanholo-saas.md` (2026-10-26): the **[cloud]**
stories. The client half (Phase 86 in TydeWhatMay/san-holo) is done; the
cloud side must match `prds/research/client-handoff-notes.md`.

External dependencies, and how each phase copes with them:
- qm integration contract (tenant-scoped secrets): pending. Build against a
  fake qm with every wire detail in one file.
- Nomad on nomad-prod, DNS, Traefik: owned by the TydeWhatMay/infra session.
  Build against a fake Nomad HTTP API; deploy files live in `deploy/`.
- Developer permission, fresh-player soak, security review: launch gates
  (US-001, US-001b, US-025), operator-owned.

## Phase 1: Foundation — accounts, invites, sign-in, admin (no external deps)

| # | Task | PRD | Status | Notes |
|---|------|-----|--------|-------|
| 1.1 | Storage foundation: config from env, PostgreSQL via pgx, embedded migrations, `tenant_id` convention, schema-derived "tables with tenant_id" helper (for US-018), test Postgres via a throwaway Docker container (`scripts/test-db.sh`), gate script. | [prd](backlog/prd-sanholo-saas.md) | DONE | first |
| 1.2 | US-009 waitlist, single-use invites (14 days, bound to email), `beta.invite_only`, hard tenant cap. | [prd](backlog/prd-sanholo-saas.md) | TODO | needs 1.1 |
| 1.3 | US-010 sign-in: magic link (15 min, single use, no enumeration, no existence-dependent work), server-side sessions (host-only cookie on app.), rate limits keyed correctly behind the edge, AhaSend client (fake in tests), passkeys (WebAuthn). | [prd](backlog/prd-sanholo-saas.md) | TODO | needs 1.1 |
| 1.4 | US-020 admin skeleton: operator accounts with passkey required, tenant list, audit table for every admin action; no path to a token. | [prd](backlog/prd-sanholo-saas.md) | TODO | needs 1.3 |
| 1.5 | US-024 privacy notice, beta terms, imprint in the company register; CSP with no third-party scripts (test). | [prd](backlog/prd-sanholo-saas.md) | TODO | copy via the voice skill (Register 2) |

## Phase 2: Tenants — secrets, onboarding, provisioning, proxy, updater (fakes for qm and Nomad)

| # | Task | PRD | Status | Notes |
|---|------|-----|--------|-------|
| 2.1 | `secrets` package: write-only qm client (put, list, destroy; never read), `PurgeTenant` / `Rotate` as the only deletion path; fake qm. | [prd](backlog/prd-sanholo-saas.md) | TODO | US-002/US-014 cloud side |
| 2.2 | US-011 connect a game account: validate the token with one game read, store via `secrets`, refuse an account already connected elsewhere (by account id), one account per tenant in the beta, create-account flow queued if registration is per IP. | [prd](backlog/prd-sanholo-saas.md) | TODO | needs 2.1 |
| 2.3 | US-012 provisioning through the Nomad HTTP API: job template (one task, image `ghcr.io/tydewhatmay/sanholo:vX.Y.Z`, two identity blocks `qm.sanholo` + `sanholo.cloud`, kill_timeout 20 s, health check on /healthz with warm-up grace), golden test, register/poll/stop, namespace-scoped token; fake Nomad. | [prd](backlog/prd-sanholo-saas.md) | TODO | needs 2.2 |
| 2.4 | US-015 authenticating proxy: per-tenant subdomain routing, Ed25519 identity per request (format in the handoff notes), strip client copies, one-time handoff code to a host-only Bridge cookie, CSRF headers passed through, security headers. | [prd](backlog/prd-sanholo-saas.md) | TODO | needs 1.3, 2.3 |
| 2.5 | US-021 updater: releases table, canary then batches of 5 with health waits, halt + revert on failure, idempotent; fake Nomad. | [prd](backlog/prd-sanholo-saas.md) | TODO | needs 2.3 |

## Phase 3: Notifications, data rights, operations

| # | Task | PRD | Status | Notes |
|---|------|-----|--------|-------|
| 3.1 | US-016 delivery: `/push/subscribe`, VAPID keys from qm, `POST /internal/alerts` verifying the Nomad workload JWT (aud `sanholo.cloud`, tenant from job id) against Nomad's JWKS, Web Push send with per-tenant cap. | [prd](backlog/prd-sanholo-saas.md) | TODO | needs 2.3 |
| 3.2 | US-017 export (ZIP, single-use link, 24 h, no token) and US-019 disconnect. | [prd](backlog/prd-sanholo-saas.md) | TODO | needs 2.1-2.3 |
| 3.3 | US-018 delete account with schema-derived verification (no rows, no qm path, no volume). | [prd](backlog/prd-sanholo-saas.md) | TODO | needs 3.2 |
| 3.4 | US-022 backups and restore runbook, US-023 monitoring and operator alerts (deploy/ + docs); coordinate with the infra session. | [prd](backlog/prd-sanholo-saas.md) | TODO | infra |
| 3.5 | US-013 / US-014 isolation tests for staging (tenant A cannot reach B; A's identity cannot read B's secret). | [prd](backlog/prd-sanholo-saas.md) | TODO | needs staging |
| 3.6 | US-025 launch checklist document. | [prd](backlog/prd-sanholo-saas.md) | TODO | last |
