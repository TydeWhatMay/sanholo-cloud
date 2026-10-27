# Progress Log

Append-only execution log. Keep the most recent ~7 entries; move older ones
to `progress-archive.md`.

---

- 2026-10-26: imported the [cloud] stories of prd-sanholo-saas into three
  phases (TASKS.md). Client side complete in TydeWhatMay/san-holo (Phase 86,
  v0.7.1, image ghcr.io/tydewhatmay/sanholo public). Starting 1.1.
- 2026-10-26: 1.1 storage foundation done: env config (DATABASE_URL or PG* from qm db creds), pgx pool, embedded forward-only migrations under an advisory lock, tenants + accounts (tenant id CHECK = qm rule), TenantTables from information_schema, throwaway postgres:17 test DB per run with a schema per test, scripts/gate.sh, /readyz. Gate passed.
- 2026-10-26: 1.2 waitlist + invites and 1.3 magic-link sign-in done. Fixes before commit: SMTP via AhaSend EU (Nectvia pattern, TLS+AUTH mandatory), sign-in link GET shows a confirm button (POST consumes; scanners cannot burn it), invite-only default (open mode refuses to start), waitlist rate limit, accounts.email_hash. Gate passed.
- 2026-10-26: 1.3b passkeys (go-webauthn v0.18.2, discoverable, UV required, cookie-bound single-use 5 min challenges consumed by DELETE ... RETURNING, sign counter enforced, session method recorded) and 1.4 admin (operator via CLI only, /admin needs operator + passkey session, 404 otherwise, audit row in the same transaction, invite resend revokes the old one, schema test bans unhashed token/secret/password columns). Fix before commit: the invite audit target held the plain email, which would outlive account deletion; now the email hash (mutation-tested). Browser path of passkey.js not yet exercised against a real authenticator. Gate passed.
- 2026-10-27: 1.5 legal pages done: /privacy, /terms, /imprint in Register 2, footer on every page, template test refusing any cross-origin or inline script (mutation-tested three ways). To make the notice true: waitlist entry deleted at sign-up, invites deleted 14 days after expiry. Found and fixed: Cleanup had never run successfully (untyped $1 - interval inferred as interval, SQLSTATE 42883), so spent sign-in rows, rate counters and old outbox mail were never deleted; now cast and covered by a test. Open for launch (3.6): server log retention in the privacy notice is a placeholder; legal review. Phase 1 complete, independent review next.
- 2026-10-27: Phase 1 independent review: PARTIAL (3 MEDIUM: privacy retention copy, TRUSTED_PROXIES not required in production, admin passkey bootstrap via mailbox; LOW: email-lockout ordering, IPv6 /128 keying, reversible audit hash, untested UV/origin, sign-count not CAS, suspension inert, static headers, no signup limit). All fixed with tests, every fix mutation-tested; gate passed. L6 (query-string tokens at the edge) and HSTS moved to the 3.6 checklist; L8 (410 for used/expired invite) kept by choice. Phase 1 closed: PASS after fixes.
