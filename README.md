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
| `QM_TOKEN`, `QM_TOKEN_FILE` | The control plane's own qm client token, directly or as a file path (re-read on every request, so renewal needs no restart). Exactly one when `QM_ADDR` is set. The principal is write-only; the token is never logged. |
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
a tenant's game credentials (`Put`), list names (`List`) and destroy
(`PurgeTenant`, `Rotate`); it has no method that returns a value, and
`TestAPISurface` fails if one is added. Deletion goes only through
`PurgeTenant` and `Rotate`. Every wire detail is in `secrets/wire.go`
(provisional until qm's integration contract lands); `secrets/qmfake` is the
in-memory qm the tests use.

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
game account again with a new token uses `secrets.Rotate`. Without `QM_ADDR`
the section says connecting is not available. "Create a new account" is not
built: it waits on the developer's answer about registrations per IP.

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
