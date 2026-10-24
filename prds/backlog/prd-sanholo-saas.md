# PRD: SanHolo Hosted (SaaS) — free invite-only beta

Status: RESEARCH (not yet imported). Author: agent with the operator, 2026-10-24.
Supersedes nothing. Builds on: v0.6.0 (public client), the 2026-10-17
proposal "Hosted notification relay as a SanHolo SaaS feature", the
operator-specific-code cleanup (relay-supply retired, errand protection by
device type, 2026-10-24).

## 1. Introduction / Overview

SanHolo today is a single-user program: one person downloads the binary,
puts their Replicant Space API token on their machine, and runs
`san-holo serve`, which starts the bot and the Bridge cockpit on a local
port. That works for people who run servers. Most players do not.

SanHolo Hosted lets a Replicant Space player sign up, hand SanHolo their
game token once, and get their own bot and Bridge running in TydeWhatMay's
infrastructure, reachable from any browser and phone, with notifications.

The first release is a **free, invite-only beta for up to 20 players**. Its
purpose is to prove the model works, learn the real running cost per
player, and earn the trust a paid service will need. Billing is designed
here but stays switched off.

### Decisions already taken (operator, 2026-10-24)

| # | Decision | Choice |
|---|----------|--------|
| D1 | Business model for v1 | Free beta first; billing designed, switched on later |
| D2 | Tenancy | **One bot per customer**: each customer gets their own isolated instance; nothing shared at runtime between customers except the front door |
| D3 | Game token custody | Encrypted in our secrets broker, per tenant |
| D4 | Secrets broker | A **dedicated production Quartermaster (qm)** for SanHolo Hosted, separate from any development broker |
| D5 | AI copilot | Off in v1; a metered add-on later |
| D6 | Hosting | Hetzner, Germany; HashiCorp Nomad schedules the instances |
| D7 | Sign-in | Email magic link, plus optional passkeys; no passwords |
| D8 | Beta scope | Invite-only (up to ~20 players), owned notifications (Web Push), several replicants per customer, self-service data export and deletion |
| D9 | Game developer | Written permission from the Replicant Space developer is a **launch gate** for the beta |
| D10 | Domain | `sanholo.space` (registered by the operator 2026-10-24). Account pages at `app.sanholo.space`; each customer's Bridge at its own `<tenant-id>.sanholo.space` (random id, never a name) |
| D11 | Unit of a customer | One customer = one Replicant Space **game account** = one API token = **one bot process**. Replicants the customer creates in the game (cloning, like Stokvis-2) run inside that same process and share the account's token and rate budget, exactly as today |
| D12 | Transactional email | AhaSend, as the other TydeWhatMay products |
| D13 | Security review | The standard norman security skill, run as the launch-gate review |
| D14 | Repositories | The hosted service lives in a **second repository**, `sanholo-cloud` (control plane, Nomad job templates, updater, infrastructure). This repository keeps the client; its hosted-mode changes stay behind `-hosted` |

## 2. Goals

- G1: A player with an invite goes from "no account" to "my bot is running
  and I can see my Bridge" in **under 10 minutes**, without installing
  anything.
- G2: No customer can see, command or affect another customer's bot, data,
  secrets or rate limit. Verified by automated isolation tests (US-012,
  US-013) and an external review before beta opens.
- G3: A customer's game token is never written to disk in plaintext, never
  logged, never shown back after entry, and is readable only by that
  customer's own instance.
- G4: Running cost per beta customer is measured and below **EUR 3 per
  month** in infrastructure (target; see Success Metrics).
- G5: Notifications reach the customer's phone through infrastructure
  TydeWhatMay owns (Web Push with our own VAPID keys); no third-party
  notification service is the default.
- G6: A customer can export everything we hold about them and delete their
  account (and every copy of their token) by themselves.
- G7: The self-hosted client keeps working exactly as today. Hosted-only
  code is opt-in through configuration, never forced on self-hosters.

## 3. Architecture overview (read this before the stories)

Two parts. Names are used throughout the PRD.

**Control plane** (`sanholo-cloud`, a new Go + Fiber + htmx service,
PostgreSQL): sign-up and invites, sign-in, the customer's account page,
replicant onboarding (token entry), the authenticating reverse proxy in
front of every Bridge, Web Push delivery, export and delete, and the admin
page. One deployment, horizontally simple, holds **no game tokens**.

**Tenant instance** (today's `san-holo serve` binary, unchanged in
behaviour, started with hosted flags): one Nomad job per customer
(`sanholo-tenant-<tenant_id>`) with **one task**: the customer's bot. It
reads the customer's game token from qm at start, keeps its catalog on the
customer's own volume, listens only on a private address, and is reachable
only through the control plane's proxy.

```
browser ──HTTPS──> edge (Traefik/Caddy, TLS)
                     └─> control plane ── session check ──┐
                                                          │ private network
                     tenant job A: bot task (replicants A1, A2) <┘   (only the proxy may connect)
                     tenant job B: bot task (replicant B1)
tenant task ──(token via qm, scoped to its own path)──> qm (prod)
tenant task ──HTTPS──> api.replicant.space (its own token, its own limits)
```

Why one process per customer: the game issues one API token per account,
and every replicant on the account (the first and any clones) is commanded
with it; the rate limits are per token. Today's binary already runs the
main replicant's bot and the second replicant's errands in one process with
one token (verified: `newErrandRunner` shares the bot's `game.Actions`). So
the customer, the token, the rate budget and the process line up one to
one (D2, D11) and nothing in the bot has to be rewritten.

Domains (D10): account and sign-in pages live on `app.sanholo.space`;
each Bridge on its own subdomain. A Bridge renders text that comes from
other players (replicant names, messages, trade notes). If any of that ever
escaped into script, a separate origin per customer keeps it away from the
account pages (no token entry, deletion or session cookie reachable from a
Bridge) and away from every other customer. Wildcard TLS via Let's Encrypt
DNS-01.

Repositories (D14): `sanholo-cloud` holds the control plane, the tenant
job template, the updater and the infrastructure code; it consumes the
client as a released container image (`ghcr.io/tydewhatmay/sanholo:X.Y.Z`, built by this
repository's release workflow, new step). Stories are tagged **[client]**
(this repository) or **[cloud]** (`sanholo-cloud`) in section 10.

## 4. User Stories

Stories are grouped in phases. Each phase can ship and be verified on its
own. Phase 0 items are gates, not code.

### Phase 0 — Gates (no code)

### US-001: Game developer permission
**Description:** As the operator, I want written permission from the
Replicant Space developer before anyone else's bot runs on our servers, so
that the service cannot be shut off or be against the game's terms.

**Acceptance Criteria:**
- [ ] `prds/research/saas-developer-agreement.md` exists and quotes (or
  links) the developer's written answer covering: hosted third-party bots
  allowed yes/no; any request limits beyond the documented ones; whether
  sign-up/verification limits (10/h, 30/h) are per IP; attribution or
  User-Agent requirements; whether the account webhook may point at our
  service.
- [ ] The beta launch checklist (US-030) has a line "developer permission:
  yes" that blocks launch while it reads anything else.

### US-002: Dedicated production qm
**Description:** As the operator, I want a production Quartermaster
instance that only SanHolo Hosted uses, so that customer secrets never sit
in the development broker.

qm is building a generic "tenant-scoped secrets" feature set with SanHolo
as its first consumer (qm agent reply, 2026-10-24). The qm integration
contract (jobspec identity block, auth role, templated policy, login and
read flow) arrives once qm's PRD is accepted; until then the model below is
the target and the stories that depend on the **[qm-planned]** items are
blocked on qm, not on SanHolo.

**Acceptance Criteria:**
- [ ] A qm server runs on the production network, sealed with its own
  shares, which are stored off the production hosts (operator-held).
- [ ] Tenant secrets live only under `secret/data/sanholo/tenants/<id>/...`.
- [ ] **One** auth role for the whole app accepts Nomad workload-identity
  JWTs whose job id matches `sanholo-tenant-*` and audience `qm.sanholo`,
  and derives the tenant id from the verified claim **[qm-planned:
  pattern-bound claims; private JWKS opt-in, or the static-PEM sync
  fallback]**.
- [ ] **One** templated policy grants read on
  `secret/data/sanholo/tenants/{{tenant}}/*` **[qm-planned: templated
  policies]**. No per-customer policies or roles exist (count check).
- [ ] The sign-up service's principal has create/update on
  `secret/data/sanholo/tenants/*`, list on
  `secret/metadata/sanholo/tenants/*`, delete on `secret/data/...`, delete
  plus sudo on `secret/destroy/...` (qm main requires sudo to destroy a
  version), and **no read** (available today). Verified by a
  read with that principal returning 403. It holds no policy or role
  authority.
- [ ] The control plane's Postgres credentials come from `qm db creds`
  (available today), using an AppRole of its own (separate from the
  write-only sign-up principal; separate principals, least privilege).
- [ ] Platform health checks and alerting use `GET /qm/v1/sys/ready`
  (no auth; 503 when qm is sealed, uninitialised or not yet operational,
  200 otherwise). `sys/health` stays a liveness probe (always 200).
- [ ] The development qm on this machine holds no SanHolo Hosted secrets.

### US-001b: Fresh-player soak test
**Description:** As the operator, I want SanHolo to run a brand-new game
account from zero for several days before any stranger's does, because
every hosted customer starts with one vessel and a few drones, and those
early-game paths have far less live mileage than a 262-device fleet.

**Acceptance Criteria:**
- [ ] A test game account (operator-created; uses one of the game's
  10-per-hour registrations; developer informed, US-001) runs the release
  candidate with default settings for at least 72 hours.
- [ ] Recorded in `prds/research/saas-soak-<date>.md`: help requests raised,
  error streaks, API requests per minute, time to the first mining drone
  print, and any strategy that never acted or looped. Every loop or stall
  found is fixed (or ticketed with a reason) before beta invites go out.

### Phase 1 — Make the binary host-ready (changes in this repo, self-hosted behaviour unchanged)

### US-003: Token from qm, not from a file
**Description:** As a tenant instance, I want to read my game token from qm
at start-up with my Nomad workload identity, so that no token or bootstrap
secret exists on the host or in the job.

**Acceptance Criteria:**
- [ ] `san-holo serve -hosted -credentials-source qm` reads its workload
  identity JWT from the file Nomad writes for the `qm.sanholo` identity
  (path given by `-qm-identity-file`, default
  `${NOMAD_SECRETS_DIR}/nomad_qm.sanholo.jwt`), logs in to qm's JWT mount
  (`-qm-addr`, `-qm-auth-mount`, `-qm-role`), reads
  `secret/data/sanholo/tenants/<id>/game` and starts with that token. The
  exact login and read calls follow qm's integration contract; a fake qm
  server in the test implements them.
- [ ] The tenant id the client asks for comes from its own job id
  (`NOMAD_JOB_ID` = `sanholo-tenant-<id>`), and a job id outside
  `^sanholo-tenant-[a-z0-9-]{1,40}$` exits with `invalid tenant job id`.
- [ ] No `credentials.json` is created anywhere; the token value appears in
  no log line (test greps the captured log for the fixture token); qm
  unreachable or a 403 exits non-zero within 30 s with
  `credentials: qm unavailable`.
- [ ] Nomad re-signs the identity before it expires; the client reads the
  file again on re-login, never caches a stale JWT past its `exp`.
- [ ] With no `-credentials-source` flag, behaviour is today's (file / env),
  covered by the existing auth tests.

### US-004: Hosted mode flag
**Description:** As the platform, I want one flag that switches the binary
into hosted mode, so that self-hosters never get hosted behaviour by
accident.

**Acceptance Criteria:**
- [ ] `-hosted` (default false) is required for every hosted-only flag
  below; passing one without `-hosted` exits with `flag X needs -hosted`.
- [ ] In hosted mode the copilot cannot be enabled: `-strategist` other
  than `none` exits with `copilot is not available in hosted mode`, and the
  Bridge hides the copilot controls (template test).
- [ ] In hosted mode the HTTP server binds only to the address given by
  `-addr` and refuses to start if that address is `0.0.0.0` or `::`
  without `-hosted-allow-any-bind` (guard against public exposure).

### US-005: Trust the proxy's identity, refuse everyone else
**Description:** As a tenant instance, I want to accept requests only from
the control plane's proxy carrying a valid signed identity, so that nobody
who reaches my port by mistake can command my bot.

**Acceptance Criteria:**
- [ ] In hosted mode every request except `GET /healthz` must carry header
  `X-SanHolo-Identity` = a token signed (Ed25519) by the control plane, with
  claims `tenant`, `exp` (≤ 5 min ahead) and `aud` = this
  instance's tenant id.
- [ ] A request with no header returns 401; a valid token for another
  tenant returns 403; an expired token returns 401; a valid token
  passes. Table-driven test with a test key pair.
- [ ] The public key is read from `-hosted-proxy-pubkey` (file path) at
  start; a missing or unparsable key exits non-zero.
- [ ] `GET /healthz` returns 200 `ok` only, and no game data.

### US-006: CSRF and same-origin for state-changing requests
**Description:** As a customer, I want my Bridge's buttons to be
unforgeable from other websites, so that a malicious page cannot command my
bot through my session.

**Acceptance Criteria:**
- [ ] Every POST under `/actions/*`, `/bot/*`, `/copilot/*` and `/bridge/*`
  without a valid `X-CSRF-Token` (double-submit cookie, per session) returns
  403 in hosted mode. htmx sends the header on every request
  (`hx-headers` on `<body>`), template test.
- [ ] Self-hosted mode keeps today's behaviour (no token needed) unless
  `-csrf` is set; existing tests stay green.
- [ ] The one-shot enqueue confirm token (`/bridge/preview/enqueue`) keeps
  working on top of CSRF.

### US-007: Instance metrics and health for the platform
**Description:** As the operator, I want each instance to report its health
and resource use, so that the platform can restart a stuck bot and I can
see the cost per tenant.

**Acceptance Criteria:**
- [ ] `GET /healthz` returns 200 while the bot loop ticked within the last
  2 minutes and the poller read the replicant within the last 2 minutes;
  503 with body `bot stalled` or `poller stalled` otherwise (fake clock
  test).
- [ ] `GET /metrics` (Prometheus text, hosted mode only, identity required)
  exposes: `sanholo_api_requests_total{caller}`, `sanholo_api_throttled_total`,
  `sanholo_bot_ticks_total`, `sanholo_errors_total`, `go_memstats_*`.
- [ ] No metric label carries a device code, location name or token.

### US-008: Graceful stop and fast start
**Description:** As the platform, I want an instance to stop cleanly on
SIGTERM and start in under 15 seconds, so that upgrades and moves do not
leave a customer's fleet half-commanded or their Bridge offline for long.

**Acceptance Criteria:**
- [ ] On SIGTERM the engine finishes its current action, stops planning,
  flushes the catalog and exits within 20 s (test with a fake action that
  blocks 1 s).
- [ ] Start-up to first `/healthz` 200 is under 15 s with a 4 MB catalog
  (today ~50 s: the star-catalogue seed must not block serving; load it in
  the background and show "loading the galaxy" in the Bridge until done).

### US-026: Container image per release
**Description:** As the platform, I want every `vX.Y.Z` release to publish
a container image, so that the cloud repository deploys exactly the
released client.

**Acceptance Criteria:**
- [ ] A `Dockerfile` (multi-stage: Go build, then a distroless/static
  runtime, non-root user, no shell) builds `san-holo` with the release's
  version stamped in.
- [ ] The release workflow builds linux/amd64 and linux/arm64 and pushes
  `ghcr.io/tydewhatmay/sanholo:X.Y.Z` plus its digest (OQ-11); the
  release notes list the digest.
- [ ] The image contains no credentials file, no catalog and no docs
  mirror (test: `docker run --entrypoint` is impossible without a shell;
  a CI step lists the image filesystem and fails on `*.db`, `*.json`
  credentials or `docs_site`).
- [ ] The running process is non-root (CI step checks the image's user).

### US-027: Structured logs to stdout
**Description:** As the operator, I want each instance to write its
activity log to stdout as JSON lines, so that Nomad and the log pipeline
can show what a customer's bot did when they ask for help.

**Acceptance Criteria:**
- [ ] With `-log-format json` (default in hosted mode; default `off` when
  self-hosted, so today's console stays quiet), every `store.Log` entry is
  also written to stdout as one JSON object: `time`, `level`, `strategy`
  (the prefix before the first colon), `message`.
- [ ] The game token never appears: a test runs a fixture bot whose token
  is a known marker and greps the captured stdout for it.
- [ ] Messages longer than 2 KB are truncated with a marker; newlines in
  messages are escaped by the JSON encoder (no log-line injection).

### US-028: Errand hardening follow-ups
**Description:** As a customer whose second replicant runs errands on our
servers, I want every errand command type-checked and every refusal path
tested.

**Acceptance Criteria:**
- [ ] settle/grow `collect`, `deposit` and `print` on the vessel
  (`rs.Host`) pass the device type read from the API to the guard, so a
  host that is not a vessel type is refused before the call (review
  2026-10-24 finding 1); the existing "vessel is a vector_charge" tests
  now feed the type production supplies.
- [ ] A test pins scout's vessel-read failure to the retry path (`fail`,
  backoff, nothing sent), not `help` (finding 2); mutating the read error
  away turns it red.

### US-029: Webhook in hosted mode
**Description:** As the platform, I want the game's (deprecated) webhook
off in hosted mode, so that no public endpoint into a tenant instance
exists.

**Acceptance Criteria:**
- [ ] In hosted mode `POST /webhook` returns 404; the bot relies on the
  event stream the poller already reads. (Self-hosted: signed deliveries
  only, done 2026-10-24, commit 4b5eac7.)

### Phase 2 — Control plane: accounts, invites, sign-in

### US-009: Invite and waitlist
**Description:** As a player, I want to join a waitlist and receive an
invite, so that the beta stays small and controlled.

**Acceptance Criteria:**
- [ ] `POST /waitlist` with a valid email stores it once (second submit of
  the same email returns the same "you are on the list" page, no
  duplicate row) and sends nothing else.
- [ ] Admin can issue an invite: a single-use code, valid 14 days, bound to
  the email; redeeming it a second time returns "invite already used".
- [ ] Sign-up without a valid invite returns 403 while `beta.invite_only`
  is true.
- [ ] Hard cap: with `beta.max_tenants` = 20 active tenants, invite
  redemption number 21 returns "the beta is full" and creates nothing.

### US-010: Sign-in with magic link and passkeys
**Description:** As a customer, I want to sign in without a password, so
that there is no password of mine to leak.

**Acceptance Criteria:**
- [ ] `POST /signin` with a known email sends one link valid 15 minutes,
  single-use; an unknown email gets the same response page and no email
  (no account enumeration; response time does not depend on existence —
  no existence-dependent work, per the security learnings).
- [ ] Following the link creates a session cookie: `HttpOnly`, `Secure`,
  `SameSite=Lax`, 30-day sliding expiry, server-side session row
  revocable from the account page.
- [ ] A signed-in customer can register a passkey (WebAuthn) and then sign
  in with it; deleting the passkey removes the row.
- [ ] Magic-link requests are rate-limited per email (5/hour) and per
  client IP as seen **behind** the edge (trusted `X-Forwarded-For` from the
  edge only); a test proves a spoofed header from a non-edge address is
  ignored.
- [ ] Email is sent through AhaSend (D12), its API key read from qm;
  templates in the company register.

### US-011: Connect my game account (onboarding)
**Description:** As a signed-in customer, I want to connect my existing
Replicant Space account by pasting its API token, or create a new account,
so that my bot starts.

**Acceptance Criteria:**
- [ ] The "Add replicant" form takes an API token; the control plane
  validates it with one `GET /v1/me` (or the current equivalent) and shows
  the replicant's name and code for confirmation; the token is then
  written to qm at `secret/sanholo/tenants/<tenant>/game` (fields
  `api_token`, `replicant_code`) and the plaintext is dropped from memory. It is never stored in
  PostgreSQL (schema test: no column named like `token`).
- [ ] An invalid token shows "the game did not accept this token" and
  writes nothing.
- [ ] A game account already connected to **another** tenant is refused
  with "this game account is already connected to a SanHolo account" (no
  detail about which). Matched on the account id the game returns, never
  on the token.
- [ ] "Create a new account" runs the game's register + verify flow
  server-side (the client's existing `internal/auth` flow) and stores the
  resulting token the same way. Blocked by the US-001 answer on per-IP
  limits; if registration is per IP, the form says so and the flow queues.
- [ ] One game account per tenant in the beta; a token for a different game
  account is refused with "beta limit: one game account".
- [ ] Replicants the customer creates in the game on that account (clones)
  appear in their Bridge and run in the same bot; no extra onboarding step
  (US-011b).

### US-011b: Create a second replicant from the Bridge
**Description:** As a customer, I want to create my second replicant from
my Bridge, as the operator did with Stokvis-2, so that I do not need API
tools to grow.

**Acceptance Criteria:**
- [ ] Check first: if the Bridge already offers the game's cloning /
  replicate action with a preview and confirmation, this story only adds a
  test that it works through the hosted proxy. Otherwise add it: a Build
  screen action that shows the game's cost and requirements, needs typed
  confirmation (a large one-way spend), and on success the new replicant
  appears in the Bridge's replicant list within one poll.
- [ ] The second replicant's errands (scout / settle / grow) are armed from
  the Bridge as today and keep the device-type protection (never a vector
  charge or replicant matrix).

### Phase 3 — Provisioning and isolation

### US-012: Start, stop and restart a tenant's bots
**Description:** As the platform, I want the control plane to create and
update one Nomad job per tenant, so that each customer's bots run, restart
on failure and stop when asked.

**Acceptance Criteria:**
- [ ] Provisioning is fully automatic from the sign-up flow: completing
  US-011 calls the **Nomad HTTP API** (`POST /v1/jobs`, with the job's
  `JobModifyIndex` for updates, never the CLI) using a Nomad ACL token
  limited to the `sanholo-tenants` namespace. Within 60 s of confirming the
  token, the customer's Bridge link answers (polls `/v1/job/<id>/allocations`
  until healthy, shows progress on the onboarding page).
- [ ] Connecting the game account renders job `sanholo-tenant-<tenant>`
  with one task: the pinned image `ghcr.io/tydewhatmay/sanholo:<version>` started with
  `-hosted -live -credentials-source qm`, a per-tenant host volume for the
  catalog, memory 384 MB hard limit (room for two replicants), CPU 300 MHz,
  restart policy 3 attempts / 10 min then fail.
- [ ] Disconnecting the game account stops the job and deletes the qm path
  and volume (deletion verified by listing).
- [ ] The customer's "Pause bot" button sets the bot stopped (today's
  `/bot` control through the proxy), not the Nomad job, so the Bridge stays
  readable while paused.
- [ ] A job template test renders a fixture tenant and compares to a golden
  HCL/JSON file.

### US-013: Network isolation
**Description:** As a customer, I want no other customer's instance to be
able to reach mine, so that isolation does not rely only on application
checks.

**Acceptance Criteria:**
- [ ] Tenant tasks listen on a private address; firewall / Nomad network
  rules allow inbound only from the control plane's proxy and Prometheus.
- [ ] An automated test task in tenant A cannot open a TCP connection to
  tenant B's task port (integration test in the staging environment, run
  before each beta release).
- [ ] Tenant tasks may connect out only to `api.replicant.space:443`, the
  qm address and the control plane's push endpoint (egress allow-list).

### US-014: Secrets scoped per tenant
**Description:** As a customer, I want my token readable only by my own
instance, so that a bug in another tenant's instance cannot read it.

**Acceptance Criteria:**
- [ ] Each tenant job declares an `identity` block with audience
  `qm.sanholo` and `file = true`; tenant ids follow qm's rule: 1 to 63
  characters, lowercase letters, digits and hyphens, starting and ending
  with a letter or digit (`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`; never
  `/`, `.`, `*` or anything qm could read as path syntax), checked by a
  regex test on the generator and enforced by the client (US-003).
- [ ] Integration test against a staging qm: tenant A's identity reading
  `secret/data/sanholo/tenants/<B>/game` gets 403; reading its own gets
  200.
- [ ] **One deletion code path** in the control plane
  (`secrets.PurgeTenant(id)` and `secrets.Rotate(id)`): today it destroys
  every version of each path and then the path; when qm's
  `DELETE secret/metadata/<path>` and sudo-gated tenant purge land
  **[qm-planned]**, only this function changes. After deletion a `list`
  of the tenant prefix returns nothing (test).
- [ ] Rotating a token writes the new value, destroys the previous
  version(s), and restarts only that tenant's task.
- [ ] The privacy notice (US-024) states the remaining copies plainly:
  deleted secrets stay in qm's raft log until compaction and in backups
  taken before deletion, for at most the backup retention (30 days).

### US-015: Authenticating proxy to each Bridge
**Description:** As a customer, I want `https://<my-subdomain-or-path>` to
show my Bridge after I sign in, and nothing to anyone else.

**Acceptance Criteria:**
- [ ] `https://<tenant-id>.sanholo.space/` with a Bridge session for that
  tenant proxies to the tenant task with a fresh `X-SanHolo-Identity`
  (US-005). Without a session it redirects to `app.sanholo.space/signin`.
  With a session for another tenant it returns 404 (not 403, so ids cannot
  be probed).
- [ ] The Bridge session is a separate cookie, host-only on that subdomain
  (no `Domain` attribute), issued by a one-time handoff: `app.sanholo.space`
  creates a 60-second single-use code and redirects to
  `<tenant-id>.sanholo.space/auth?code=...`, which swaps it for the cookie.
  The account session cookie is host-only on `app.sanholo.space` and never
  sent to a Bridge (test both cookie headers).
- [ ] Response headers set by the proxy: CSP (self only, no inline scripts
  beyond the Bridge's existing nonce-free policy — audit needed),
  `X-Frame-Options: DENY`, `Referrer-Policy: same-origin`, HSTS.
- [ ] The proxy strips any incoming `X-SanHolo-Identity` header from the
  browser before adding its own (test).
- [ ] htmx partial requests, JSON endpoints (`/bridge/map.json`) and static
  assets all work through the proxy (end-to-end test against a fixture
  instance started with `-fixture`).

### Phase 4 — Notifications we own

### US-016: Bridge as an installable PWA with Web Push
**Description:** As a customer, I want my phone to buzz when my bot needs me
(an asteroid inbound, a decision waiting, help needed), so that I do not
have to keep the Bridge open.

**Acceptance Criteria:**
- [ ] The Bridge serves a web app manifest and a service worker (hosted
  mode), installable on Android Chrome and iOS Safari 16.4+ (home-screen
  install required on iOS; the settings page says so).
- [ ] "Enable notifications" stores the browser's push subscription in the
  control plane (not the instance), tied to the tenant.
- [ ] The instance posts alerts to the control plane's
  `POST /internal/alerts` (identity-signed, tenant-bound); the control plane
  sends Web Push with TydeWhatMay's VAPID keys. Alert payloads carry kind,
  short text and a deep link, never coordinates of other players or
  tokens.
- [ ] Per-tenant send cap: 30 pushes/hour; over it, one "more alerts are
  waiting in your Bridge" push and silence until the hour rolls.
- [ ] The existing per-alert preferences (snooze, quiet hours, which kinds)
  apply before sending (reuse `internal/alert` prefs).
- [ ] Self-hosted mode: unchanged; BYO sinks stay as today, no default
  third-party URL.

### Phase 5 — Data rights and account lifecycle

### US-017: Export my data
**Description:** As a customer, I want to download everything SanHolo holds
about me, so that I can see it or take it elsewhere.

**Acceptance Criteria:**
- [ ] "Export" produces a ZIP within 5 minutes containing: account record
  (email, created, invites), sessions list, push subscriptions, per
  replicant the catalog as SQLite plus a JSON dump of goals, jobs,
  decisions and the activity log. The game token is **not** included.
- [ ] The link is single-use and expires in 24 h.

### US-018: Delete my account
**Description:** As a customer, I want to delete my account and have every
copy of my token and data removed, so that nothing of mine stays.

**Acceptance Criteria:**
- [ ] "Delete account" asks to type the account email to confirm, then:
  stops and removes the tenant job, deletes the qm paths, deletes volumes,
  deletes PostgreSQL rows (account, sessions, passkeys, subscriptions,
  invites), and emails a confirmation.
- [ ] After deletion, a check job verifies: no qm path under the tenant, no
  volume, no rows (`SELECT count(*) ... WHERE tenant_id = $1` = 0 in every
  table that has the column; the list of tables is derived from the
  schema, not hand-kept).
- [ ] Backups: the privacy notice states backups containing the data are
  kept up to 30 days; deleted tenants are excluded from restores after
  deletion (restore procedure documents the purge step).
- [ ] The game account itself is not wiped (that is the player's, done in
  the game); the page says so.

### US-019: Disconnect my game account
**Description:** As a customer, I want to disconnect my game account
without deleting my SanHolo account.

**Acceptance Criteria:**
- [ ] "Disconnect" stops the job, deletes the qm path and volume, keeps the
  SanHolo account; the game account can be connected again later (to this
  or another SanHolo account), starting with an empty catalog.

### Phase 6 — Operating the beta

### US-020: Admin page
**Description:** As the operator, I want one page listing every tenant and
instance, so that I can support the beta.

**Acceptance Criteria:**
- [ ] `/admin` (operator accounts only, passkey required) lists tenants:
  email, created, replicants, instance state (running / failed /
  paused), last health, API requests/min, memory, and actions: restart
  instance, suspend tenant, resend invite.
- [ ] The admin page never shows a token and cannot read one (control
  plane policy is write-only, US-002).
- [ ] Every admin action is written to an audit table with actor, action,
  target and time.

### US-021: Fleet update job (upgrades and rollbacks)
**Description:** As the operator, I want a job that updates every tenant's
instances when I publish a new SanHolo version, in stages, so that I never
redeploy tenants by hand and a bad release hurts few customers.

**Acceptance Criteria:**
- [ ] Publishing a release (a `vX.Y.Z` tag builds image `ghcr.io/tydewhatmay/sanholo:X.Y.Z`)
  records the version in the control plane's `releases` table; nothing
  rolls until the operator marks it `rollout`.
- [ ] The **updater** is a Nomad periodic/batch job (or a control-plane
  worker; same code path) that, for a release in `rollout`, re-registers
  each tenant job through the Nomad HTTP API with the new image version:
  canary tenants (`beta.channel = canary`, the operator's own first), then
  waits 1 hour of healthy `/healthz` across canaries, then the rest in
  batches of 5 with a 10-minute health wait between batches.
- [ ] A batch with any instance failing health stops the rollout, marks the
  release `halted`, re-pins the failed tenants to the previous version and
  alerts the operator (US-023). Test with a fake Nomad API: batch 2 fails ->
  batches 3+ untouched, batch 2 rolled back.
- [ ] Each tenant job's task group uses Nomad's own `update` stanza
  (`max_parallel = 1`, `health_check = "checks"`, `auto_revert = true`)
  so a single bad allocation reverts on its own.
- [ ] Rollback of a whole release is one admin action that sets the
  previous version as target and runs the same updater.
- [ ] The updater is idempotent: running it twice at the same state
  changes nothing (it compares the job's current image to the target before
  registering).
- [ ] A release cannot be promoted while any canary instance has failed
  health in the last hour.
- [ ] The catalog schema migration runs on start (today's behaviour);
  migrations must be backward compatible for one version so rollback works
  (documented rule, checked in review).

### US-022: Backups and restore
**Description:** As the operator, I want the control-plane database and the
tenant catalogs backed up and restorable, so that a disk loss is not a
data loss.

**Acceptance Criteria:**
- [ ] Nightly encrypted backups of PostgreSQL and tenant volumes to a
  second Hetzner location; kept 30 days.
- [ ] A restore drill on staging is run and timed before beta opens; the
  runbook records the time.
- [ ] Secrets (qm) are backed up per qm's own procedure; restore drill
  included.

### US-023: Monitoring and alerting for the operator
**Acceptance Criteria:**
- [ ] Prometheus scrapes the control plane and every instance (US-007);
  dashboards show per-tenant API requests/min, throttles, restarts,
  memory.
- [ ] Operator alerts (to the operator's own push, US-016 path): instance
  crash-looping, any tenant throttled (429) more than 5 times in 10 min,
  qm sealed, backups failed, disk > 80%.

### Phase 7 — Legal and launch

### US-024: Privacy notice, terms, imprint (Register 2 copy)
**Acceptance Criteria:**
- [ ] Pages: privacy notice (controller TydeWhatMay, data list, purposes,
  retention per item with numbers, processors with locations, rights),
  beta terms (free, no warranty, may end with 30 days' notice, the game
  is a third party's), imprint. Written in the company register (plain
  facts, company as subject).
- [ ] A processor list includes Hetzner (DE) and the email provider; no
  analytics or tracking scripts on any page (CSP test).

### US-025: Beta launch checklist
**Acceptance Criteria:**
- [ ] `prds/research/saas-launch-checklist.md` lists and ticks: US-001
  permission, isolation test pass (US-013/014), restore drill (US-022),
  a norman security-skill review (D13) of the proxy, identity, handoff,
  CSRF and sign-in (US-005/006/010/015) with no open HIGH or MEDIUM,
  privacy pages live, operator alerting live, canary tenant (operator)
  ran 7 days without a crash loop.
- [ ] Beta opens only when every line is ticked.

## 5. Functional Requirements

Client binary (this repo):
- FR-1: The binary must read credentials from qm when started with
  `-credentials-source qm`, and must never write them to disk in that mode.
- FR-2: A `-hosted` flag must gate every hosted-only behaviour; without it
  the binary behaves exactly as v0.6.0.
- FR-3: In hosted mode the copilot must be unavailable.
- FR-4: In hosted mode every request except `/healthz` must carry a valid
  control-plane identity for this tenant.
- FR-5: In hosted mode state-changing requests must carry a valid CSRF
  token.
- FR-6: The binary must expose `/healthz` and, in hosted mode, `/metrics`.
- FR-7: The binary must stop cleanly within 20 s of SIGTERM and serve
  `/healthz` within 15 s of start.
- FR-8: Alerts in hosted mode must be posted to the control plane, not to
  BYO sinks, unless the customer adds a BYO sink themselves.

Control plane (new service):
- FR-9: Waitlist, single-use invites, and a hard tenant cap.
- FR-10: Magic-link sign-in, passkeys, server-side revocable sessions.
- FR-11: Replicant onboarding that validates a token against the game and
  writes it only to qm.
- FR-12: One Nomad job per tenant with one task (the customer's bot), rendered
  from a tested template and registered through the Nomad HTTP API from the
  sign-up flow, with no manual step.
- FR-13: An authenticating reverse proxy that signs a short-lived identity
  per request and strips client-supplied identity headers.
- FR-14: Web Push delivery with owned VAPID keys and per-tenant caps.
- FR-15: Self-service export and deletion that removes qm paths, volumes
  and rows, with a verification step.
- FR-16: An admin page with an audit log; no path from admin to a token.
- FR-17: An updater job rolls each new release to every tenant through the
  Nomad HTTP API in stages (canary, then batches), halts and reverts on
  failed health, and is idempotent.

Platform:
- FR-18: A dedicated production qm; the control plane holds a write-only
  policy on tenant secrets; each tenant identity reads only its own path.
- FR-19: Network isolation between tenants and an egress allow-list.
- FR-20: Encrypted nightly backups with a tested restore.

## 6. Non-Goals (Out of Scope for the beta)

- Billing, plans, invoices, VAT (designed in OQ-8, not built).
- The AI copilot in hosted mode.
- A shared multi-tenant bot service (one process serving many customers).
- Moving a customer's self-hosted catalog into the hosted service (manual
  import may be offered case by case; self-service import is later).
- Mobile apps; the PWA is the mobile story.
- Teams / several people sharing one tenant.
- Any change to bot strategy behaviour for hosted customers.
- Public sign-up without invites.
- Regions outside Germany.

## 7. Technical Considerations

**Game limits and etiquette.**
- The per-minute limits are per API token: 120 reads and 60 actions. Each
  customer's bot spends only its own budget; today's bot runs at about
  40 reads a minute.
- Registration (10/h), verification (30/h), webhook changes (12/h) and the
  star catalogue (1/min) are listed separately. If they count per IP, every
  hosted customer shares our egress address for them, so onboarding must
  queue. Confirm with the developer (US-001).
- Every instance sends `User-Agent: SanHolo/<version>`. Hosted instances
  should add `(hosted)` so the developer can tell the traffic apart; ask
  them whether they want this.
- The game offers an account webhook. Using it would let us poll less, but
  it is a later optimisation: today's poller already fits the budget.

**The Bridge today has no authentication.** Anyone who can reach its port
can command the bot. Hosted mode must never expose it directly: US-005
(identity), US-006 (CSRF) and US-013 (network isolation) are layered on
purpose. Any one of them alone is not enough.

**Fiber specifics carried over** (from the operator's learnings):
`Immutable: true` (already set), `recover` middleware first (already
set), and GET handlers with side effects must ignore HEAD and prefetch.

**Footprint.** About 100 MB RSS per replicant process and a ~4 MB catalog
today. Twenty tenants (one process each, up to two replicants inside) fit on one CPX31-class
server with headroom. Budget two servers: one for the control plane and qm,
one for tenants. The startup star-catalogue seed (~50 s) must move off the
start path (US-008).

**Nomad.**
- Use the docker or exec driver with resource limits and a per-tenant
  host volume.
- Template rendering lives in the control plane, with a golden-file test.
- The control plane's Nomad token can manage only `sanholo-tenant-*` jobs,
  through a namespace with a quota.

**qm (dedicated, production).**
- Paths: `secret/data/sanholo/tenants/<tenant_id>/game`, fields
  `api_token` and `replicant_code` (one game account per tenant).
- The sign-up service writes, lists and destroys but never reads. Tenants
  read only their own prefix.
- Identity: each tenant job's Nomad workload identity (audience
  `qm.sanholo`) logs in to one pattern-bound qm role; one templated policy
  maps the tenant id from the verified job id to its prefix (US-002,
  US-014). No bootstrap secret in any job; no per-customer policies.

**Database (control plane).**
- PostgreSQL, parameterised queries only, migrations in the repo.
- `tenant_id` on every tenant-owned table. Deletion verification reads the
  schema to find those tables.
- Database credentials come from qm short-lived roles (`qm db creds`).

**Code layout.**
- A new repository `sanholo-cloud` for the control plane, in Go + Fiber +
  htmx with vendored static assets (no CDN), matching the operator's stack.
- Changes to the client stay in this repository behind `-hosted`. The
  public README documents hosted flags only briefly, as self-hosters do not
  need them.

**Security review targets.**
- Identity signing (key rotation: two keys accepted during rollover).
- CSRF.
- Proxy header stripping.
- Session fixation.
- Magic-link replay.
- Rate-limit key derivation behind the edge (per the operator's learning:
  per-IP keys collapse behind a proxy unless the forwarded address is
  trusted from the edge only).
- Control characters in email input.

## 8. Success Metrics

- 20 invited players onboarded; median time from invite click to "bot
  running" under 10 minutes (measured by control-plane timestamps).
- Zero cross-tenant access findings in the external review and the
  isolation tests.
- Zero plaintext tokens found by a periodic scan of logs, volumes and
  database dumps (scan job greps for each tenant's token hash prefix
  pattern; the scan itself never logs tokens).
- Instance availability ≥ 99% over the beta's first 30 days (healthz).
- No tenant throttled by the game more than once a day on average.
- Infrastructure cost per tenant measured; target ≤ EUR 3/month.
- At least 10 of 20 beta players still active after 30 days.

## 9. Open Questions

Answered 2026-10-24: subdomains per customer (D10), domain `sanholo.space`
(D10), one process per customer with in-game clones inside (D11), AhaSend
(D12), norman security skill (D13), a second repository (D14).

- OQ-1 (developer): permission, IP-scoped limits, attribution, webhook use
  (US-001). Also ask whether the name `sanholo.space` next to
  `replicant.space` is fine with them. Blocking.
- OQ-3 (qm) ANSWERED 2026-10-24: workload-identity JWT login with one
  pattern-bound role and one templated policy; write-only sign-up
  principal works today; db creds work today. **Waiting on qm** for:
  private JWKS opt-in (or static-PEM sync), pattern-bound claims, templated
  policies, metadata delete and tenant purge, non-200 sealed health, a
  backup/restore runbook and snapshot API, and the integration contract.
  US-002, US-003 (live login), US-014 and US-022 (qm restore drill) depend
  on these; the client side of US-003 can be built now against a fake qm.
- OQ-4 ANSWERED 2026-10-24: `sanholo.space` is registered at OVH. The
  TydeWhatMay/infra session holds a scoped OVH DNS token and owns DNS,
  Traefik and the Nomad platform side on **nomad-prod** (DNS-01 wildcard
  for `*.sanholo.space`, `app.sanholo.space`, AhaSend SPF/DKIM records;
  no inbound email). The operator confirms before it is set up.
- OQ-11 ANSWERED 2026-10-24: GitHub Container Registry under the
  TydeWhatMay org, image `ghcr.io/tydewhatmay/sanholo:X.Y.Z`. Both SanHolo
  repositories moved to the TydeWhatMay org on 2026-10-24
  (`TydeWhatMay/san-holo`, public binaries `TydeWhatMay/SanHolo`), so the
  release workflow pushes with its own `GITHUB_TOKEN`
  (`permissions: packages: write`); no extra secret. The package is
  created by the first push. Visibility: public, like the
  binaries, so Nomad pulls without registry credentials (operator to
  confirm after the first push).
- OQ-8 (later, billing): plans per customer per month through an EU
  merchant of record (Vatly) with Mollie, VAT handled by the merchant of
  record. Price levels follow the cost measured in the beta.
- OQ-9: should hosted customers be able to add a BYO notification sink
  (Discord, their own ntfy) in addition to Web Push? Default answer: yes,
  as an opt-in, never as the default.
- OQ-10: retention after a tenant is suspended (not deleted) for
  inactivity: proposed 90 days, then a warning email, then deletion after
  a further 30 days.

## 10. Suggested phase order

1. Phase 0 gates in parallel with Phase 1. Phase 1 changes ship in the
   public client too (`-hosted` off by default), so they are low risk.
2. Phase 2 and Phase 3 together make the first internal tenant possible:
   the operator's own replicant as canary.
3. Phase 4 (push) and Phase 5 (export and delete) must be done before any
   other player is invited.
4. Phase 6 and Phase 7, then the launch checklist (US-025).

Repository per story: **[client]** US-003 to US-008, US-011b, US-026 to
US-029 and the
Bridge side of US-016 (manifest, service worker, alert post). **[cloud]**
US-002, US-009 to US-015, the delivery side of US-016, US-017 to US-025.
New in this repository: a release step that builds and pushes the
`ghcr.io/tydewhatmay/sanholo:X.Y.Z` container image the cloud repository deploys.
