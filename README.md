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
| `BETA_MAX_GAME_ACCOUNTS_PER_TENANT` | Default `1`. |
| `APP_BASE_URL` | Public origin, used to build sign-in links. `https` unless the host is loopback. Default `http://127.0.0.1:8090`. |
| `TRUSTED_PROXIES` | Comma-separated CIDRs or addresses of the edge. `X-Forwarded-For` is believed only from these peers; otherwise the peer address is the client address for rate limits. Default empty. |
| `APP_ENV` | `production` or `development` (default). In production `SMTP_HOST` is required. |
| `SMTP_HOST` | Mail server (production: `send.ahasend.com`, EU). Unset in development: mail is logged as dropped and never sent. |
| `SMTP_PORT` | Default `587` (STARTTLS). `465` is implicit TLS. |
| `SMTP_USERNAME`, `SMTP_PASSWORD` | When the username is set, STARTTLS and AUTH are mandatory; the service refuses to send without them. The password comes from qm and is never logged. |
| `SMTP_FROM` | Sender. Default `no-reply@<SMTP_HOST>`. |

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
`/signin/verify` (the emailed link), `/account` (sessions), `/signout`.
The emailed link opens a page with one button; the button signs in. Mail goes through the `mail_outbox` table and a worker that retries with
backoff. A sign-in request stores only a hash of the address; a worker turns
it into a link and a queued email when an account exists, so the HTTP answer
is the same for every address. Issue an invite from Go with
`auth.Service.IssueInvite` (the admin page will call it).

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
