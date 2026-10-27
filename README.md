# sanholo-cloud

The hosted SanHolo service: the control plane, tenant provisioning on Nomad,
the release updater and the infrastructure behind `sanholo.space`.

SanHolo itself (the Replicant Space client, bot and Bridge cockpit) is in
`TydeWhatMay/san-holo`. This repository runs released SanHolo container images,
one per customer.

Status: the free, invite-only beta is being built. The requirements are in
`prds/backlog/prd-sanholo-saas.md`.

## Development

The service reads its settings from the environment only; there are no flags
for them.

| Variable | Meaning |
| --- | --- |
| `DATABASE_URL` | PostgreSQL URL. If unset, built from `PGHOST`, `PGUSER`, `PGDATABASE` (required) and `PGPORT`, `PGPASSWORD`, `PGSSLMODE`. |
| `LISTEN_ADDR` | Listen address. Default `127.0.0.1:8090`. |
| `BETA_INVITE_ONLY` | Default `true`. `false` is refused at start: open sign-up needs email verification, which is not built. |
| `BETA_MAX_TENANTS` | Tenant cap. Default `20`. |
| `BETA_MAX_GAME_ACCOUNTS_PER_TENANT` | Default `1`. Only `1` is supported: qm holds one game credential per tenant, and any other value is refused at start. |
| `GAME_API_URL` | Replicant Space API origin. Default `https://api.replicant.space`. `https`, or `http` on a loopback host (tests). |
| `APP_BASE_URL` | Public origin, used to build sign-in links. `https` unless the host is loopback. Default `http://127.0.0.1:8090`. |
| `TRUSTED_PROXIES` | Comma-separated CIDRs or addresses of the edge. `X-Forwarded-For` is believed only from these peers; otherwise the peer address is the client address for rate limits. Default empty. |
| `APP_ENV` | `production` or `development` (default). In production `SMTP_HOST`, `SMTP_USERNAME`, `TRUSTED_PROXIES` and an explicit https `APP_BASE_URL` are required, and the service refuses to start without them. |
| `SMTP_HOST` | Mail server (production: `send.ahasend.com`, EU). Unset in development: mail is logged as dropped and never sent. |
| `SMTP_PORT` | Default `587` (STARTTLS). `465` is implicit TLS. |
| `SMTP_USERNAME`, `SMTP_PASSWORD` | When the username is set, STARTTLS and AUTH are mandatory; the service refuses to send without them. The password comes from qm and is never logged. |
| `SMTP_FROM` | Sender. Default `no-reply@<SMTP_HOST>`. |
| `QM_ADDR` | Origin of the dedicated qm that holds customer game tokens: `https`, or `http` on a loopback host. Unset in development: secrets are disabled (the service starts; operations return `ErrSecretsUnavailable`). Required when `APP_ENV=production`. |
| `QM_ROLE_ID`, `QM_SECRET_ID`, `QM_SECRET_ID_FILE` | The control plane's qm AppRole (`sanholo-provisioner`): the role id and exactly one of the secret id or a file holding it (re-read on every login). The service logs in with `POST /qm/v1/auth/approle/login`, sends the client token as `X-QM-Token`, renews it with `renew-self` past two thirds of its lease and logs in again when qm rejects it (once per request). Required with `QM_ADDR` when `APP_ENV=production`. The principal is write-only; none of these values is logged. |
| `QM_TOKEN`, `QM_TOKEN_FILE` | A static qm client token, exactly one of the two, for development and tests only. Refused when `APP_ENV=production`, and never combined with the AppRole variables. |
| `NOMAD_ADDR` | Nomad origin (`https`, or `http` on loopback). Unset in development: provisioning is disabled, the service starts and tenants stay "not provisioned". Required when `APP_ENV=production`. With it set, `QM_ADDR`, `SANHOLO_IMAGE_VERSION` and a proxy key (below) are required too. |
| `NOMAD_TOKEN`, `NOMAD_TOKEN_FILE` | The control plane's Nomad ACL token, limited to the tenant namespace; exactly one of the two (the file is re-read on every request). Never logged. |
| `NOMAD_NAMESPACE`, `NOMAD_REGION`, `NOMAD_DATACENTERS`, `NOMAD_HOST_NETWORK` | Namespace (default `sanholo`), region, datacenters (default `dc1`) and the host network the bot port is mapped onto. |
| `SANHOLO_IMAGE_VERSION`, `SANHOLO_IMAGE_REPO` | Version new instances start at (`0.7.1` or `v0.7.1`) and the image repository (default `ghcr.io/tydewhatmay/sanholo`). |
| `PROXY_SIGNING_KEY`, `PROXY_SIGNING_KEY_FILE` | The Ed25519 private key the proxy signs each request's identity with: a PKCS#8 `PRIVATE KEY` PEM, or base64 of the 32-byte seed. Exactly one of the two. Turns the Bridge proxy on; required when `APP_ENV=production`. Never logged. The public key in every tenant job is derived from it. Generate: `openssl genpkey -algorithm ed25519`. |
| `TENANT_PROXY_PUBLIC_KEYS`, `TENANT_PROXY_PUBLIC_KEYS_FILE` | With `PROXY_SIGNING_KEY`: optional, the next public key during a rotation (PEM, or base64 one per line), added after the signing key's own. Without it (development, proxy off): the proxy's public key(s), exactly one of the two. |
| `TENANT_DOMAIN`, `TENANT_ALERT_URL` | Parent domain of the Bridge hosts (default `sanholo.space` once Nomad is configured; required with `PROXY_SIGNING_KEY` otherwise) and the alert endpoint the bots post to (default `APP_BASE_URL/internal/alerts`). |
| `NOMAD_JWKS_URL` | Where Nomad's signing keys are fetched to verify the bots' alert tokens. Default `${NOMAD_ADDR}/.well-known/jwks.json`; `https`, or `http` on loopback. Unset without `NOMAD_ADDR`: `/internal/alerts` answers 503. |
| `NOMAD_JWT_ISSUER` | Optional. When set, the tokens' `iss` must equal it. Nomad adds `iss` only when the server sets `oidc_issuer`, so the default is not to check it; the keys, the audience, the namespace and the job id are checked either way. |
| `VAPID_PUBLIC_KEY`, `VAPID_PRIVATE_KEY` or `VAPID_PRIVATE_KEY_FILE`, `VAPID_SUBJECT` | TydeWhatMay's Web Push (VAPID) keys, all or none. Keys are base64url P-256 (public 65 bytes, private 32 bytes); the public key must be the private key's, or the service refuses to start. `VAPID_SUBJECT` is `mailto:address` or an https URL. The public key is the single source for the tenant jobs' `-hosted-vapid-public-key` too. Generate a pair with `sanholo-cloud vapid generate` (prints both as environment lines). Required when `APP_ENV=production`; in development, without them nothing is pushed and the Bridge shows no notification button. The private key is a secret: deliver it as a file from qm. |
| `TENANT_MEMORY_MB`, `TENANT_CPU_MHZ`, `PROVISION_TIMEOUT` | Bot resources (384, 300) and how long a bot may take to become healthy (15m). |
| `ROLLOUT_BATCH_SIZE`, `ROLLOUT_HEALTH_TIMEOUT` | How many tenants an update moves together after the canary (5, 1 to 50) and how long one may take to become healthy on the new version (default `PROVISION_TIMEOUT`; 1m to 6h). |
| `ROLLOUT_CANARY_SOAK`, `ROLLOUT_BATCH_SOAK` | How long an updated tenant is watched after its deployment turned healthy before it counts as done: the canary (default `1h`) and every other batch (`10m`). `0` turns a soak off; the maximum is 24h. |
| `TENANT_VOLUME_SIZE_MB`, `TENANT_VOLUME_NODE_POOL`, `TENANT_VOLUME_CONSTRAINT` | Each tenant's dynamic host volume (Nomad 1.10+, plugin `mkdir`): requested size (default 1024), optional node pool, and an optional node constraint written `${attribute}=value`. |
| `WEBAUTHN_RP_ID` | Passkey relying-party id: a domain, such as `app.sanholo.space`. Credentials are bound to it; changing it orphans every passkey. Default: the host of `WEBAUTHN_RP_ORIGIN`. A bare IP host cannot be an id, so passkeys stay off. Required when `APP_ENV=production`. |
| `WEBAUTHN_RP_ORIGIN` | Exact origin the browser reports. `https`, or `http` for `localhost` only. Its host must be the id or a subdomain of it. Default: `APP_BASE_URL`. |
| `WEBAUTHN_RP_NAME` | Name shown by the authenticator. Default `SanHolo`. |

In operation, the credentials come from qm:

```sh
qm db creds -role <role> -env -- sanholo-cloud
```

Migrations (`internal/db/migrations`) are embedded and applied at start under
a PostgreSQL advisory lock. Every tenant-owned table has a `tenant_id`
column; `db.TenantTables` lists them from the schema.

Run the service against a local database:

```sh
DATABASE_URL=postgres://user:pass@127.0.0.1:5432/sanholo go run ./cmd/sanholo-cloud
curl http://127.0.0.1:8090/healthz   # process is up, no dependencies
curl http://127.0.0.1:8090/readyz    # 200 only when the database answers
```

### Sign-up and sign-in

Pages: `/` (waiting list), `/waitlist`, `/signup`, `/signin`,
`/signin/verify` (the emailed link), `/account` (sessions, passkeys), `/signout`, and the legal pages `/privacy`, `/terms` and `/imprint`.
The emailed link opens a page with one button; the button signs in. Mail goes through the `mail_outbox` table and a worker that retries with
backoff. A sign-in request stores only a hash of the address; a worker turns
it into a link and a queued email when an account exists, so the HTTP answer
is the same for every address. Issue an invite from Go with
`auth.Service.IssueInvite` (the admin page will call it).

### Passkeys

A signed-in account adds a passkey on `/account` and can then sign in from
`/signin` with no email address. Passkeys are discoverable and always
require user verification (PIN or biometric). The ceremony state is stored
server-side, bound to the browser by a `__Host-` cookie, single use, and
valid for 5 minutes. A sign-in whose sign counter does not advance is
refused, and the counter write is a compare-and-set. Once an account has a
passkey, or is an operator, another passkey can be added only from a session
opened with a passkey (a sign-in link can add the first passkey of an
ordinary account). Every registration queues an email to the account address.
Deleting a passkey ends the account's passkey sessions. The page script is `static/passkey.js`; the content security policy
stays `script-src 'self'`. Library: `github.com/go-webauthn/webauthn` v0.18.2,
the current release (September 2026) of the maintained fork of the
original Duo library, with discoverable login and clone detection.

### Admin

`/admin` (tenants), `/admin/waitlist` and `/admin/audit`. Access needs a
session, `accounts.is_operator`, and a session opened with a passkey. A
non-operator gets a 404. An operator who signed in by magic link is asked to
sign in with a passkey. Operator rights are granted only from the command
line, with the same environment as the server, and only for an account that
already has a passkey: sign in by link, add a passkey on `/account`, then grant.
An operator cannot suspend their own tenant, and a suspended tenant's
sessions stop working:

```sh
sanholo-cloud operator grant <email>
sanholo-cloud operator revoke <email>
```

Both write an audit row with actor `cli`. An invite is named in the audit log by its row id, not by the address. Every admin POST (issue or resend
an invite, suspend or unsuspend a tenant) writes one audit row in the same
transaction as the change. An invite code is sent by email only; the database
keeps its hash and no admin page shows it. A suspended tenant keeps its place
in the beta cap. The database holds no game token; a test fails if any
column named for a token, secret or password is not a `*_hash`.

### Tenant secrets

`internal/secrets` is the only route to tenant secrets in qm. It can store
a tenant's game credentials (`Put`), report the current version number of
that secret from qm's metadata (`Version`, 0 when absent) and purge a tenant
(`PurgeTenant`); it has no method that returns a value and none that lists
or enumerates tenants (SanHolo's database is the record), and `TestAPISurface`
fails if one is added. A token rotation is a plain new `Put`: qm keeps 16
versions and prunes the oldest. Deletion goes only through `PurgeTenant`,
which uses qm's Part E purge endpoint. That endpoint has not shipped, so
until it does a purge is refused with `ErrDenied` (the first-connect
clean-up is best effort and logs the refusal). Qm errors map to `ErrDenied`
(403, after one fresh login), `ErrSealed` (503 `vault_sealed`, also an
`ErrUnavailable`), `ErrUnavailable` (other 5xx, 429, network, timeout) and
`ErrProtocol`. `Ready` reads `GET /qm/v1/sys/health` and passes only when qm
is initialized and unsealed. The wire is in `secrets/wire.go`, per the
contract in `prds/research/qm-contract.md` (the Part E endpoints are marked
provisional there); `secrets/qmfake` is the in-memory qm the tests use.

### Game account

The account page has a "Game account" section. The customer pastes an API
token; the control plane checks it with one `GET /v1/accounts/me` to the game
(`internal/game`: timeout, bounded body, no redirects, User-Agent
`SanHolo-Cloud/<version>`), then shows the replicant's name and code. A
second button, Confirm, stores it. The game's account record has no
identifier of its own (its email can be changed), so the game account id is
derived from the account's earliest replicant: `replicant:<code>`.

Between the two steps the token is held in process memory only, for 10
minutes, under the hash of a random secret in a `__Host-` cookie, bound to
the signed-in account and used once. It is never written to PostgreSQL, not
even encrypted. A restart, or a second instance answering the confirm,
loses it and the customer enters the token again.

Confirm writes the `game_accounts` row (tenant, game account id, replicant
name and code, connected date; `tenant_id` and `game_account_id` are both
unique) inside a transaction, then writes `{api_token, replicant_code}` to
qm with `secrets.Put`, then commits. A game account already connected to
another tenant loses the race at the unique constraint before anything
reaches qm; a failed qm write rolls the row back. A different game account
for a tenant that has one is refused (beta limit: one). Connecting the same
game account again with a new token is a new `secrets.Put` (a new version). Without `QM_ADDR`
the section says connecting is not available. "Create a new account" is not
built: it waits on the developer's answer about registrations per IP.

### Provisioning

Confirming a game account starts the customer's bot in the background
(`internal/provision`): the control plane builds the Nomad job for
`sanholo-tenant-<id>` in Go (`internal/nomad/job.go`; the golden file is
`internal/nomad/testdata/job.golden.json`, regenerate with
`go test ./internal/nomad -update`), registers it through the Nomad HTTP API
with the job's modify index, and records the instance in `tenant_instances`
(job, image version, state `pending`, `running`, `failed` or `stopped`, last
health, a fixed failure reason; no secrets). A re-registration of an unchanged
job is skipped, and an existing instance keeps its image version. The account
page shows the state and refreshes itself while the bot starts; a failed bot has
a "Start the bot again" button. A new token for the same game account restarts
the running bot, which reads the token from qm at start. A worker refreshes every
instance each 30 seconds, so a bot that stops being healthy shows on `/admin`,
where an operator can also restart it (audited in the same transaction as the
state change). `provision.Service.Stop` stops (and with purge, forgets) a job;
disconnect and delete use it. The job carries no secret: the bot's game token
comes from qm through the Nomad workload identity. The Nomad client
(`internal/nomad`) only touches jobs named `sanholo-tenant-<valid tenant id>`.
What the platform must provide is in `deploy/README.md`.

### Updates (releases and rollouts)

The operator adds a client release, then starts its rollout (`internal/updater`).
Releases come from the command line or `/admin/releases`; nothing polls GitHub
(a possible follow-up: read the release and its digest from the `san-holo`
release notes). Every action writes its audit row in the same transaction.

```sh
sanholo-cloud release add v0.8.0 [--digest sha256:...] [--notes text] [--url https://...]
sanholo-cloud release start v0.8.0     # plans the rollout; the running service carries it out
sanholo-cloud release list
sanholo-cloud release resume | abandon # for a halted rollout
sanholo-cloud release canary <tenant-id>|none
```

With a digest the tenant job runs `image@digest`; without, `image:vX.Y.Z`.
A rollout moves one canary tenant first (the tenant marked with `release canary`,
else the oldest that has to move), waits for it to be healthy on the new version
(Nomad's deployment verdict for the new job version) and then to soak, then
moves batches of 5 (`ROLLOUT_BATCH_SIZE`), each waiting for the one before and
soaking in turn. Nomad decides its verdict once, 30 seconds after the task
starts, while the client answers 200 "starting" for up to ten minutes; the soak
(1 hour for the canary, 10 minutes for a batch) is what notices a bot that never
really came up. During it the live results of the `/healthz` check, the task
restart count and the identity of the allocation are read each pass: a failing
check, a restart or a replaced allocation counts as a failed update. The canary
is also held to a rule over time: a rollout is not started, and does not
complete, while the canary tenant was seen unhealthy in the last hour. Tenants that are
suspended, stopped or not running are recorded as skipped and not touched; a
tenant already on the release counts as done. The state is in PostgreSQL
(`releases`, `rollout_tenants`), so a restarted control plane resumes where the
last one stopped, repeating a step is harmless (a spec Nomad already holds is not
registered again), and the database allows one rollout (rolling or halted) at a
time. A release becomes the fleet target, the version new instances and stopped
instances starting again use, when its rollout is done; a rollout of the target
again acts as a catch-up for tenants that were skipped.

Any failure (a failed deployment, a deployment Nomad reverted itself, the health
timeout, a registration Nomad refuses) halts the rollout. The tenant that failed
and any tenant of the same batch still updating are put back on their previous
version by registering the previous spec again (a no-op when Nomad's
`auto_revert` already did it) and the revert counts once Nomad reports the old
version healthy; tenants not yet reached are never touched. The release shows the
tenant and a fixed reason, never Nomad's text. A halted rollout waits for the
operator: resume (the tenants that were reverted are tried again) or abandon
(tenants already on the new version stay there). The account page shows the
bot's version.

A restart the control plane asked for during a soak (a rotated token, a retry,
an admin restart) is recorded in `tenant_instances.restart_requested_at` before
Nomad is asked, because Nomad counts it in the task's restart count like a
crash; the soak then starts over instead of failing the update. A canary that
halts a rollout for any reason is recorded as unhealthy. When the canary gate
holds a release back, the log says so (at most every ten minutes) and the
admin releases page shows the tenant and the time the gate opens by itself. The
operator can override it for a release that is held right now (available or
rolling), audited (`release override-canary <version>`, or the button, which is
shown only then). The override is good for one decision: starting, resuming,
completing or abandoning the rollout spends it. The soak starts over at most
three times for restarts the control plane asked for; the fourth fails the
update, so a bot that keeps crashing cannot be hidden behind reconnects. A
disconnect waits at most 30 seconds for the tenant's lock and then asks the
customer to try again.

The reconcile worker also keeps each running instance in line with what the
control plane expects, without fighting the updater: a job whose spec differs
from the one the current settings render (a rotated proxy key, a changed
limit) is registered again, a few tenants per pass, and an instance that was
skipped by a rollout because it was not running, and that is running on an old
version later, is moved to the fleet target, only to a newer version (an abandoned rollout
leaves tenants ahead of the target, and they stay there); every re-registration
it makes is audited (`instance.converge`, `instance.reregister`). Neither happens for a tenant with
an unfinished row in a rollout, and the second waits for every rollout to end.
The health shown on the account page and in `/admin` comes from the live check
results, because Nomad's deployment verdict never changes after it is decided.

A retry or an admin restart that finds no running allocation to restart
registers the job again with a bumped `sanholo.restart` job meta (stored in
`tenant_instances.restart_seq`, outside the spec hash). Registering an identical
spec is only a new evaluation on Nomad and does not revive a failed job; a
changed meta value makes a new job version, a new deployment and a replacement
allocation. Every later render (updater, revert, reconcile) carries the stored
value, and drift detection ignores it.

### Disconnecting a game account

`/account` offers "Disconnect this game account" (a confirmation page, then a
POST with the CSRF token). It stops the bot (without purge, then with), deletes
the game account and instance records and writes an audit row, in one
transaction; the tenant and the account stay. Deleting the token in qm
(`secrets.PurgeTenant`) and the tenant's host volume then follow. Until qm ships
its purge endpoint the purge is refused: the deletion is kept in
`pending_cleanups` (fixed reason phrase, attempt count) and retried every
minute while it works. A failing deletion backs off (a minute, doubling, at most six
hours) and is logged at error level at the 5th, 10th, 25th, 50th and 100th
failure; the kinds are retried independently. A new connect for the tenant
cancels the pending purge, and the purge runs under the tenant's row lock, so a
late purge cannot remove a new token. A pending volume deletion is carried out
by the next start before it makes a new volume, so another game account never
inherits the old data. The disconnect holds the tenant's provisioning lock from
the stop to the commit. Lock order: that lock, then the tenants row.

### Bridge access and the proxy

With `PROXY_SIGNING_KEY` set the control plane also serves each customer's
Bridge (`internal/proxy`, `internal/server/bridge.go`). Routing is by the
`Host` header:

| Host | Served |
| --- | --- |
| host of `APP_BASE_URL` (`app.sanholo.space`) | the control plane, as before |
| `<tenant-id>.<TENANT_DOMAIN>` (`tenant.Valid` id, one label) | that tenant's Bridge, through the proxy |
| anything else | `404 not found`, the same answer every time |

`/healthz` and `/readyz` answer on any host, for the platform's probes. A
tenant id that does not exist is indistinguishable from one that does until a
session says otherwise.

Getting in: the signed-in account page has "Open my Bridge" (`POST
/account/bridge/open`, CSRF-protected). It creates a 60 second single-use code
(stored hashed, bound to that account session and the account's own tenant) and
redirects to `https://<tenant-id>.<domain>/auth?code=...`. `GET /auth` on the
tenant host swaps the code for the Bridge cookie `__Host-sanholo_bridge`
(host-only, Secure, HttpOnly, SameSite=Lax, 24 hours at most and never longer
than the account session). `HEAD` and prefetch requests to `/auth` answer
204 and spend nothing; the endpoint is rate-limited per client IP (20 a minute).
The account session cookie is host-only on the app host, so a browser never
sends it to a tenant host, and the proxy removes both control plane cookies from
anything it forwards. The Bridge session is a row that dies with the account
session (sign out, revoke), and is refused once the tenant is suspended.

Per request on a tenant host: no Bridge session redirects to
`APP_BASE_URL/signin` (htmx requests get `401` with `HX-Redirect`); a session for
another tenant, or a suspended tenant, is `404`; a bot that is not running gets a
plain `503` page from the control plane ("Your bot is starting" or "not
running"). Otherwise the request is forwarded to the allocation's address from
Nomad (cached ten seconds, looked up again after a connection failure) with a
fresh `X-SanHolo-Identity`: Ed25519 over `"sanholo-identity/v1." + payload`,
payload `base64url({"tenant","aud","exp"})`, tenant and audience the tenant id,
expiry two minutes ahead (the client accepts at most five). Any identity or
`X-SanHolo-*` header from the browser is dropped first. The Bridge's own CSRF
cookie and `X-CSRF-Token` pass through untouched. Responses carry the proxy's
CSP, `X-Frame-Options: DENY`, `Referrer-Policy: same-origin`, HSTS and
`nosniff` in place of the Bridge's.

Key rotation: set `TENANT_PROXY_PUBLIC_KEYS` to the new public key and roll the
tenant jobs (the key file is part of the job, so the updater does it); then make
the new key `PROXY_SIGNING_KEY` and drop the old public key. Tenants accept
both during the overlap.

### Notifications (Web Push)

`internal/push`, `internal/workload`, `internal/server/push.go`.

- **Subscribing.** The Bridge's "Enable notifications" posts the browser's
  subscription to `POST /push/subscribe` on the tenant host. The control plane
  answers it itself, before the proxy: the instance has no such route and never
  sees it. It needs the tenant's Bridge session (no session is `401`, another
  tenant's session or a suspended tenant is `404`) and the Bridge's own CSRF
  scheme, verified here because the control plane answers in the Bridge's place:
  the 64 hex character `__Host-sanholo_csrf` cookie repeated in `X-CSRF-Token`.
  `Origin` (if sent) must be the tenant's origin, `Sec-Fetch-Site` (if sent)
  `same-origin`, and the body JSON of at most 4 KiB. The endpoint must be `https`
  on port 443 with a public DNS name (no IP literal, `localhost`, single-label or
  `.internal`/`.local` name, no credentials), at most 2048 bytes; `p256dh` must
  be a valid P-256 point (65 bytes) and `auth` 16 bytes, both unpadded base64url.
  At send time the HTTP client also refuses to connect to a non-public address,
  uses no proxy and follows no redirect, so a name that resolves to an internal
  address is refused. A tenant holds at most 10 subscriptions (`409` beyond; the
  same browser subscribing again updates its keys), the endpoint is unique across
  tenants (`409`), and `POST /push/unsubscribe` with `{"endpoint": ...}` removes
  one of the tenant's own. The table is `push_subscriptions` (migration 0014:
  `tenant_id`, `account_id`, `account_session_id` informational, set null when the
  session ends). A subscription outlives its session and goes when the person
  unsubscribes, the push service answers `404`/`410`, or the tenant is deleted.
- **Alerts.** `POST /internal/alerts` on the app host: the bot sends
  `Authorization: Bearer <Nomad workload JWT>` and the JSON the client builds
  (`kind, severity, text, key, at, link`). The token is verified against Nomad's
  JWKS (RS256 or EdDSA; keys cached ten minutes and refetched when a key id is
  unknown, at most every 30 seconds): audience exactly `sanholo.cloud`, `exp`,
  `nbf` and `iat` present and valid with 30 seconds of skew, `nomad_namespace`
  equal to `NOMAD_NAMESPACE`, `nomad_job_id` of the form `sanholo-tenant-<id>`
  with a valid tenant id. The tenant is that id, nothing else; `X-SanHolo-Tenant`
  is ignored. Every refusal is `401` with no reason; unreachable keys are `503`
  so the bot can tell. The kind must be one the bot sends, the text at most 280
  characters, the body at most 8 KiB; the tenant must exist and not be deleting
  (`403`); 300 posts per tenant per hour are accepted. The answer is `202` once
  the alert is queued: delivery runs on a bounded queue (256) with four workers
  and never delays the post.
- **Delivery.** Web Push with TydeWhatMay's VAPID keys through
  `github.com/SherClockHolmes/webpush-go` (RFC 8291 `aes128gcm`, RFC 8292), to
  every subscription of the tenant. The payload is exactly the service worker's
  `{title, body, link, key, kind}`: the title is `SanHolo: <kind>`, the body the
  alert text cut to 240 characters without control characters, the link rebuilt
  on the tenant's own origin from the bot's link keeping only `alert`, `job` and
  `sys`. `404`/`410` deletes the subscription; `429` and `5xx` retry after
  5 s, 10 s (longer if `Retry-After` says so, at most 5 minutes), three attempts
  in all; other refusals are logged and the row is kept.
- **Cap.** 30 alerts per tenant per fixed one-hour window, opened by the first
  counted alert. The 31st gets one push, "More alerts are waiting in your
  Bridge.", and nothing more until the hour is over, then the count starts again.
  Counting happens when there is at least one subscription to deliver to. The
  window lives in memory: a restart resets it.
- **Preferences.** Snooze-style routing, quiet hours and which kinds are sent
  are applied by the bot (`internal/alert` in the client, before the hosted sink
  is called), so the control plane does not apply them again.
- **Operators.** `push.Service.NotifyOperator(kind, text)` pushes to every
  subscription of a tenant that has an operator account, with a cap bucket of its
  own. Task 3.4 calls it.

### The gate

```sh
scripts/gate.sh
```

It runs gofmt (must print nothing), build, vet, the tests and gosec, prints
each result and exits non-zero on any failure. Docker is required: the tests
run through `scripts/test-db.sh`, which starts a throwaway `postgres:17`
container on a free port, exports `TEST_DATABASE_URL`, runs the command and
always removes the container. Each test works in its own schema. Without
`TEST_DATABASE_URL` the database tests skip, so run tests as:

```sh
scripts/test-db.sh go test ./... -count=1
```
