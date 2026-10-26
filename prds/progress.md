# Progress Log

Append-only execution log. Keep the most recent ~7 entries; move older ones
to `progress-archive.md`.

---

- 2026-10-26: imported the [cloud] stories of prd-sanholo-saas into three
  phases (TASKS.md). Client side complete in TydeWhatMay/san-holo (Phase 86,
  v0.7.1, image ghcr.io/tydewhatmay/sanholo public). Starting 1.1.
- 2026-10-26: 1.1 storage foundation done: env config (DATABASE_URL or PG* from qm db creds), pgx pool, embedded forward-only migrations under an advisory lock, tenants + accounts (tenant id CHECK = qm rule), TenantTables from information_schema, throwaway postgres:17 test DB per run with a schema per test, scripts/gate.sh, /readyz. Gate passed.
- 2026-10-26: 1.2 waitlist + invites and 1.3 magic-link sign-in done. Fixes before commit: SMTP via AhaSend EU (Nectvia pattern, TLS+AUTH mandatory), sign-in link GET shows a confirm button (POST consumes; scanners cannot burn it), invite-only default (open mode refuses to start), waitlist rate limit, accounts.email_hash. Gate passed.
