# Deploy notes for the infra session

What the control plane (`sanholo-cloud`) needs from the platform so that
`internal/provision` can run one Nomad job per customer. The job itself is
built in Go (`internal/nomad/job.go`), checked against
`internal/nomad/testdata/job.golden.json`. Nothing here is a Nomad job file:
the control plane registers the tenant jobs through the HTTP API.

## 1. Nomad namespace and ACL

- Namespace `sanholo` (override with `NOMAD_NAMESPACE`), with a quota sized for
  the beta (20 tenants x 384 MB / 300 MHz, plus headroom for a canary).
- An ACL policy for the control plane's token, and nothing wider:

  ```hcl
  namespace "sanholo" {
    capabilities = ["submit-job", "read-job", "alloc-lifecycle", "host-volume-write", "host-volume-read"]
  }
  ```

  `submit-job` covers register and stop/purge, `read-job` covers the job, its
  allocations (task states, restart counts), its latest deployment and the live
  check results (`/v1/allocation/<id>/checks`), `alloc-lifecycle` covers
  restarting an allocation (admin restart and token rotation).
  `host-volume-write` covers creating and deleting the tenants' dynamic host
  volumes and `host-volume-read` listing them (Nomad 1.10+; the capability names
  come from the Nomad 2.0.6 binary, the ACL itself was not exercised: if your
  version names the delete capability separately, add it).
- **Nomad 1.10 or newer** (dynamic host volumes). The control plane was checked
  against Nomad 2.0.6 (`nomad agent -dev`).
- Pass the token as `NOMAD_TOKEN`, or as `NOMAD_TOKEN_FILE` (re-read on every
  request, so it can be rotated). `NOMAD_ADDR` must be `https` (plain `http` is
  accepted only on a loopback address).
- Workload identity must be enabled (Nomad 1.7+). The job has two identity
  blocks, each written to a file in the task's secrets directory:

  | Identity name | Audience | File the SanHolo client reads |
  | --- | --- | --- |
  | `qm_sanholo` | `qm.sanholo` | `${NOMAD_SECRETS_DIR}/nomad_qm_sanholo.jwt` |
  | `sanholo_cloud` | `sanholo.cloud` | `${NOMAD_SECRETS_DIR}/nomad_sanholo_cloud.jwt` |

  Nomad accepts identity names matching `^[a-zA-Z0-9-_]{1,128}$` only, so the
  names have no dots; the audiences keep theirs. The job passes both file names
  explicitly (`-qm-identity-file`, `-hosted-alert-identity-file`).

The tenant logs in to qm at `auth/nomad` (the jwt method the operator mounts, qm contract Appendix A.1) with role `sanholo-tenant`.

  qm's JWT auth role `sanholo-tenant` must accept audience `qm.sanholo` from
  Nomad's JWKS and map `nomad_job_id` (`sanholo-tenant-<id>`) to the tenant
  prefix. The control plane verifies the second token against the same JWKS
  (section 7).

## 2. Host volumes

The control plane creates one **dynamic host volume** per tenant before it
registers the tenant's job (`PUT /v1/volume/host/create`, Nomad 1.10+):

- Name `sanholo-tenant-<tenant id>` (the job id), plugin `mkdir` (built into
  Nomad), namespace `sanholo`, capability `single-node-writer` /
  `file-system`, capacity `TENANT_VOLUME_SIZE_MB` (the plugin does not enforce
  it), optional `TENANT_VOLUME_NODE_POOL` and `TENANT_VOLUME_CONSTRAINT`
  (`${meta.sanholo_tenants}=true`, say) to choose the nodes.
- Parameters `mode=0750`, `uid=65532`, `gid=65532`: the image runs as `nonroot`
  (65532) and the client keeps its SQLite catalog in `/data/catalog.db`. The
  `mkdir` plugin changes the owner itself, which works when the Nomad client
  runs as root (a normal production client). On a client that does not, the
  create fails and the instance shows "Nomad could not create the bot's data
  volume".
- The Nomad client config needs `client { host_volumes_dir = "..." }` on the
  tenant nodes (a disk with room for the volumes); volumes are directories under
  it.
- Creating is idempotent: the volume is looked up by name first, and an
  existing one is reused (Nomad refuses to create a second with the same name).
  The job mounts it as `volume "data" { type = "host", source = "<name>",
  access_mode = "single-node-writer", attachment_mode = "file-system" }` at
  `/data`.
- A volume is tied to the node it was created on, so the tenant's bot always
  runs there; a lost node loses the data unless it is restored from backup.
- Disconnecting a game account stops and purges the job, then deletes the volume
  (`DELETE /v1/volume/host/<id>/delete`; Nomad refuses while the job still holds
  it, so a refusal is kept in `pending_cleanups` and retried every minute).
  Deleting an account will do the same through the same path.

## 3. Network

- The group uses `network { mode = "bridge" }` (CNI plugins on the tenant
  nodes). Each task has its own network namespace, so the client listens on
  `0.0.0.0:8080` inside it (`-hosted-allow-any-bind`), the Bridge, and from
  client 0.8.0 (`nomad.MetricsSince`) also on `0.0.0.0:9090`
  (`-metrics-addr`), a listener that serves only `GET /metrics`, with no
  identity, for Prometheus. It exposes nothing else.
- Nomad maps both ports (labels `http` and `metrics`) onto the host network
  named by `NOMAD_HOST_NETWORK`. Define that host network on the tenant nodes as
  the private interface (`host_network "private" { cidr = "10.x.x.x/24" }`), so
  neither port is ever bound on a public address. Leave it empty to use the
  node's default address: then the metrics port is as reachable as that address,
  so set it in production. The metrics port carries no game data (no label holds
  a device code, location or token), but it has no authentication of its own;
  the private network and the firewall below are what keep it private.
- A job for a client older than 0.8.0 gets neither the `metrics` port, its
  service nor the flag (an unknown flag stops the client), and keeps its spec
  hash, so tenants are not re-registered for this change; the port arrives with
  the release that rolls them to 0.8.0 or later.
- Firewall (US-013), owned by the infra session:
  - inbound to the mapped ports (the dynamic range, 20000-32000 by default) only
    from the control plane's proxy (the `http` port) and Prometheus (the
    `metrics` port). Both are dynamic ports in the same range, so a rule that
    cannot tell them apart admits both callers to both: the proxy gains
    nothing (it dials `http` only), and Prometheus reaching `http` meets the
    identity check and gets `401`;
  - tenant tasks may connect out only to `api.replicant.space:443`, qm, and the
    control plane (`/internal/alerts`, `TENANT_ALERT_URL`).
- The control plane finds a tenant's bot from the allocation's `http` host
  address (`nomad.Status.Address`, which reads the port labelled `http` and
  no other); the service `sanholo-bot` is registered with the Nomad provider
  (tag `tenant=<id>`) as well. The metrics port has its own service,
  `sanholo-bot-metrics` (tag `tenant=<id>`, no check), which only Prometheus
  reads. Nothing at the edge or in the proxy is built from the Nomad service
  catalogue, and the edge routes only to the control plane (below), so neither
  service is ever an edge target. Keep it so: an edge that discovers routes
  from Nomad services must never pick up `sanholo-bot-metrics`.

### Edge routing for the Bridge proxy (task 2.4)

The control plane serves both the app and every Bridge, told apart by the
`Host` header. The edge must therefore:

- route `app.sanholo.space` and `*.sanholo.space` (one wildcard router) to the
  control plane, and pass the original `Host` header through unchanged;
- terminate TLS with a wildcard certificate for `*.sanholo.space` (DNS-01
  challenge; HTTP-01 cannot issue wildcards) plus `app.sanholo.space` if the
  apex wildcard does not cover it;
- send `X-Forwarded-For` with the client address, and list the edge's address in
  `TRUSTED_PROXIES`, as for the app today (the proxy believes the header only
  from there and rebuilds it for the Bridge);
- not log query strings on any host: sign-in links and invite codes
  (`?token=`, `?code=`) and the Bridge handoff (`/auth?code=`, valid 60 seconds,
  single use) travel in URLs. Log the path only;
- not cache, compress-and-rewrite or buffer responses from `*.sanholo.space`
  (the proxy streams; an edge that buffers delays any long-lived response);
- set HSTS if it likes; the control plane sets `max-age=31536000` on Bridge
  responses itself (the app host's HSTS remains the edge's, as before);
- let the control plane's allocation addresses be reachable: the control plane
  host must be allowed through the firewall rule in section 3 to the tenant
  ports (it is "the proxy" there);
- keep `/healthz` and `/readyz` checkable by address (any `Host`).

## 4. Image

`ghcr.io/tydewhatmay/sanholo:vX.Y.Z` is public, so Nomad pulls without registry
credentials. `SANHOLO_IMAGE_VERSION` (for example `0.7.1`) is the version new
instances start at until a rollout has finished; after that the fleet target (the
last release whose rollout is done) is the version a new instance starts at. An
existing instance keeps its own version; the updater moves it. Releases are added
by the operator (`sanholo-cloud release add vX.Y.Z --digest sha256:...`; the digest
is in the client release notes) and make the job run `image@digest`.

## 5. Control plane environment

| Variable | Meaning |
| --- | --- |
| `NOMAD_ADDR`, `NOMAD_TOKEN` or `NOMAD_TOKEN_FILE` | Required in production. |
| `NOMAD_NAMESPACE` | Default `sanholo`. |
| `NOMAD_REGION`, `NOMAD_DATACENTERS` | Optional region; datacenters default `dc1`. |
| `NOMAD_HOST_NETWORK` | Host network for the port, see section 3. |
| `SANHOLO_IMAGE_VERSION` | Required with `NOMAD_ADDR`. |
| `PROXY_SIGNING_KEY` or `PROXY_SIGNING_KEY_FILE` | The proxy's Ed25519 private key (PKCS#8 PEM, or base64 of the 32-byte seed). Required in production; a secret, so deliver it as a file from qm, not in the job spec. The control plane derives the public key written into every tenant job (`proxy-pubkey.pem`) from it. |
| `TENANT_PROXY_PUBLIC_KEYS` or `TENANT_PROXY_PUBLIC_KEYS_FILE` | Optional with `PROXY_SIGNING_KEY`: the next public key during a rotation, so tenants accept two. Public. |
| `TENANT_DOMAIN` | Default `sanholo.space`; each bot gets `-public-url https://<id>.<domain>`, and `<id>.<domain>` is where the control plane serves that bot's Bridge. |
| `TENANT_ALERT_URL` | Where bots post alerts. Default `APP_BASE_URL/internal/alerts`. |
| `VAPID_PUBLIC_KEY`, `VAPID_PRIVATE_KEY` or `_FILE`, `VAPID_SUBJECT` | Web Push keys, all or none (section 7). The public key is passed to the bots. |
| `NOMAD_JWKS_URL`, `NOMAD_JWT_ISSUER` | Section 7. |
| `TENANT_MEMORY_MB`, `TENANT_CPU_MHZ` | Default 384 and 300. |
| `PROVISION_TIMEOUT` | How long a bot may take to become healthy. Default 15m. |
| `ROLLOUT_BATCH_SIZE`, `ROLLOUT_HEALTH_TIMEOUT` | Tenants per update batch (default 5) and how long one may take to be healthy on the new version (default `PROVISION_TIMEOUT`). The job's own update stanza (`auto_revert`, healthy deadline 10m) still applies per tenant. |
| `ROLLOUT_CANARY_SOAK`, `ROLLOUT_BATCH_SOAK` | Soak after a tenant's deployment turned healthy: 1h for the canary, 10m for other batches (`0` = none). Live check results, restarts and the allocation are watched; see the README. |
| `TENANT_VOLUME_SIZE_MB`, `TENANT_VOLUME_NODE_POOL`, `TENANT_VOLUME_CONSTRAINT` | The tenant volume, section 2. |
| `QM_ADDR` | Also given to each bot as `-qm-addr`. |
| `INTERNAL_LISTEN_ADDR`, `METRICS_TOKEN_FILE`, `ALERTMANAGER_TOKEN_FILE` | Required in production. The platform listener for Prometheus and Alertmanager and its two bearer tokens (section 9). |
| `DELETION_LOG_PATH` | Required in production. The append-only file account deletions are recorded in (section 8). Put it on a volume that is not restored with the database and is copied off the host. |

No variable that holds a secret is ever copied into a job.

## 6. Runbook: rotating the proxy signing key

The tenants verify each request's identity against the public key(s) in their
job (`proxy-pubkey.pem`). Every tenant job therefore changes when the keys do,
and the reconcile worker does the changing: it compares each running job's spec
hash with the one the current settings render and registers the jobs that
differ, three per 30-second pass (a tenant with an unfinished rollout row is
left to the updater). To rotate without a moment of rejected requests:

1. Generate the new key. Put its public key in `TENANT_PROXY_PUBLIC_KEYS` (or
   `_FILE`) next to the current signing key, and restart the control plane.
   Tenants now accept two keys.
2. Wait until the reconcile worker has registered every tenant: each tenant's
   job meta `sanholo.spec` matches (`nomad job inspect sanholo-tenant-<id>`), and
   every bot is healthy again on the account page or `/admin`. About
   `tenants / 3 x 30 s` plus the time each bot takes to restart.
3. Switch `PROXY_SIGNING_KEY` (or `_FILE`) to the new private key and keep the
   old public key in `TENANT_PROXY_PUBLIC_KEYS`; restart. The proxy signs with
   the new key, every tenant still accepts both.
4. Wait for the reconcile pass again (the derived public key list changed
   order), then remove the old public key from `TENANT_PROXY_PUBLIC_KEYS`,
   restart, and wait for the pass once more. Only the new key remains.

## 7. Web Push and the alert intake (task 3.1)

- **The control plane must reach Nomad's JWKS**
  (`${NOMAD_ADDR}/.well-known/jwks.json`, or `NOMAD_JWKS_URL`). The endpoint
  needs no ACL token (checked on a Nomad 2.0.6 dev agent). If it is unreachable
  for longer than an hour the control plane stops accepting alerts (`503`); it
  keeps the keys it has for up to an hour.
- Nomad signs the workload tokens with RS256 (the dev agent's JWKS lists RSA keys
  with `alg: RS256`); EdDSA is accepted too. The tokens carry no `iss` unless the
  Nomad server sets `oidc_issuer`; if it does, set `NOMAD_JWT_ISSUER` to the same
  value. The claims used: `aud` (exactly `sanholo.cloud`), `exp`, `nbf`, `iat`,
  `nomad_namespace` and `nomad_job_id`.
- **Tenants must reach the control plane's `/internal/alerts`** (`TENANT_ALERT_URL`,
  default `APP_BASE_URL/internal/alerts`), which is the app host, so the firewall
  rule in section 3 already covers it. The endpoint is authenticated by the Nomad
  token alone; as a second layer the edge may restrict `/internal/` to the
  platform network. The tenant hosts do not serve it.
- **The control plane must reach the browsers' push services** on 443
  (`fcm.googleapis.com`, `updates.push.services.mozilla.com`,
  `web.push.apple.com`, `*.notify.windows.com`, and others by vendor). It uses no
  HTTP proxy from the environment and refuses private and loopback addresses.
- The VAPID private key is a secret like the proxy signing key: deliver it as a
  file from qm (`VAPID_PRIVATE_KEY_FILE`). Changing the key pair orphans every
  subscription (a browser's subscription is bound to the public key it was made
  with); do not rotate it casually. The tenant jobs get the public key from the
  same setting, so a change re-registers every job through the reconcile worker.

## 8. After a restore: deleted accounts must stay deleted

A restore from backup (US-022) brings back every account that was deleted after
the backup was taken, with its email, sessions and rows. The privacy notice
promises that a deleted account is not restored, so a restore is not finished
until those deletions have been applied again.

The control plane keeps a **deletion log**, `DELETION_LOG_PATH`: one JSON line
per account deletion, `{"at":..., "tenant":"<id>", "account":"<uuid>"}`, written
and fsynced before the bot is stopped or any row is deleted. It holds two
random identifiers and a time, no email address. It lives outside PostgreSQL and
is not part of the database backup: put it on a volume that the restore does not
touch and copy it off the host with the other backups (it is tiny and
append-only). The audit log was not chosen because it is a table: a restore
rolls it back together with the deletions it records.

Procedure, with the control plane stopped and the restored database in place:

1. Restore the database, then (if the log lived on the restored volume) put the
   latest copy of the deletion log back at `DELETION_LOG_PATH`. Never restore an
   older log over a newer one.
2. Run `sanholo-cloud deletions reapply` with the production environment (the
   same variables as the server; it reads `DELETION_LOG_PATH`, or take the file
   as an argument). It deletes again, through the same code as the web
   deletion, every tenant in the log whose account the restored database still
   holds: stops and purges the job, deletes the volume, purges qm, deletes the
   rows and writes an audit row with actor `cli`. It sends no email. A tenant the
   database does not have is reported as "nothing to delete". Run it again until
   it reports no failure; it is safe to repeat.
3. Start the control plane. Its cleanup worker retries what could not finish
   (the qm purge, refused with 403 and pending until the route of the final
   contract, ADR 0024, is deployed on the production qm; a busy volume) and its check job reports the result
   in `/admin`, under "Account deletions".
4. Open `/admin` and confirm that no deletion is listed as failed.

Restored volumes of deleted tenants must also be removed (Nomad volumes are
restored with the host backup): `deletions reapply` does this for every tenant it
re-deletes, through `DeleteVolume`.


## 9. Monitoring and operator alerts (US-007, US-023)

Files: `deploy/monitoring/prometheus.yml`, `sanholo.rules.yml` (with
`sanholo.rules.test.yml`), `alertmanager.yml`, `grafana-dashboard.json`. Addresses marked `EXAMPLE` are
placeholders. Every secret is a file (`credentials_file`), never inline.

### The platform listener

The control plane serves two platform endpoints on a second address,
`INTERNAL_LISTEN_ADDR`, never on the app address:

| Route | Caller | Bearer |
| --- | --- | --- |
| `GET /metrics` | Prometheus | contents of `METRICS_TOKEN_FILE` |
| `POST /internal/operator-alerts` | Alertmanager | contents of `ALERTMANAGER_TOKEN_FILE` |

- Bind it to the control plane's private address (`10.x.x.x:9464`, say). The
  edge must not route to it. Each route also needs its own token, so the scrape
  token cannot send pushes.
- The tokens: `openssl rand -base64 32`, one each, stored in qm and delivered as
  files. The same values go to Prometheus
  (`/etc/prometheus/secrets/sanholo-metrics-token`) and Alertmanager
  (`/etc/alertmanager/secrets/sanholo-alerts-token`). Changing one means a
  restart of the control plane and a reload of the caller.
- The listener speaks plain HTTP. Keep it on the private network (or a
  WireGuard link); the bearer crosses it in clear.
- Firewall: Prometheus and Alertmanager to the control plane's internal port;
  Prometheus to node_exporter (9100) on every host and to every Nomad agent's
  HTTP port.

### What the control plane exports

All from the database at scrape time or from counters in the process; no label
carries a tenant id, an email or a token.

- `sanholo_cloud_tenants{status}`, `sanholo_cloud_instances{state}`,
  `sanholo_cloud_instances_health{health}`
- `sanholo_cloud_releases{state}`, `sanholo_cloud_rollout_tenants{state}` (the
  release being rolled out or halted)
- `sanholo_cloud_pending_cleanups{kind}`, `..._pending_cleanup_oldest_seconds{kind}`,
  `..._pending_cleanup_max_attempts{kind}`
- `sanholo_cloud_mail_outbox{state="queued|failed"}`,
  `sanholo_cloud_mail_outbox_oldest_queued_seconds`
- `sanholo_cloud_tenant_deletions{check_state}`,
  `sanholo_cloud_tenant_deletions_stalled` (deletions that stopped before the
  tenant's rows were deleted, requested over an hour ago)
- `sanholo_cloud_push_messages_total{result}`,
  `sanholo_cloud_push_alerts_dropped_total{reason="cap|queue_full"}`,
  `sanholo_cloud_push_cap_hits_total`, `sanholo_cloud_operator_alerts_total{result}`
- `sanholo_cloud_qm_up`, `sanholo_cloud_qm_sealed`,
  `sanholo_cloud_qm_last_probe_timestamp_seconds` (qm's health checked every 30
  seconds; absent before the first check)
- `sanholo_cloud_database_up` (0 when a query of the scrape failed; the
  database families are then left out, the rest still arrive)

### Other sources

1. **Nomad agents** (job `nomad`): per-allocation restarts and memory. Every
   agent needs:

   ```hcl
   telemetry {
     prometheus_metrics         = true
     publish_allocation_metrics = true
     publish_node_metrics       = true
   }
   ```

   The crash-loop alert reads `nomad_client_allocs_restart`; Nomad's own `job`
   label arrives as `exported_job` and the scrape config derives `tenant` from
   it. Check the metric name against a real agent
   (`curl $NOMAD_ADDR/v1/metrics?format=prometheus | grep allocs_restart`) and
   adjust the rule if your version names it differently.
2. **node_exporter** on every host (job `node`), with the textfile collector
   enabled (`--collector.textfile.directory=<dir>`). Disk alerts read it, and
   the backup jobs write `sanholo_backup_*` into that directory (section 10).
3. **Tenant bots** (job `sanholo-tenants`, Nomad service discovery of
   `sanholo-bot-metrics`, tag `tenant=<id>`, which becomes the `tenant` label).
   This is each client's private metrics port (section 3): plain HTTP,
   `GET /metrics` only, no identity and no bearer. Prometheus needs a Nomad
   token for the discovery:

   ```hcl
   namespace "sanholo" { capabilities = ["read-job"] }
   ```

   The Bridge port's `/metrics` (service `sanholo-bot`) still needs the
   proxy's per-request `X-SanHolo-Identity` and answers `401` to Prometheus;
   it is not scraped. A tenant still on a client older than 0.8.0 has no
   `sanholo-bot-metrics` service, so it is simply not a target until its
   release moves.

### Alerts

`sanholo.rules.yml` holds the US-023 alerts and a few from the control plane's
own metrics. Each alert has a `kind` label (lower case, hyphens, at most 40
characters), which becomes the push's kind.

| Alert | Fires when |
| --- | --- |
| `InstanceCrashLooping` | a tenant's bot restarted 3 or more times in the last hour, added up over all its allocations (Nomad); keeps firing 30 minutes past the last match, so a reschedule delayed by an hour does not resolve it |
| `TenantThrottled` | a tenant got more than 5 game 429s in 10 minutes (tenant scrape, client 0.8.0 and later) |
| `QMSealed` / `QMUnreachable` / `QMProbeStale` | qm sealed for 1 minute / not answering (not sealed) for 5 / not checked for 5 |
| `BackupFailed` / `BackupStale` / `BackupMissing` | a backup run failed / no success for 26 hours / no database backup ever reported (26 hours) |
| `BackupRetentionFailed` | the retention after the database backup failed (section 10) |
| `DeletionStalled` | an account deletion stopped before the tenant's rows were deleted (bot not stopped, deletion log not written) over an hour ago |
| `DiskAlmostFull` | a filesystem above 80% for 15 minutes |
| `InstanceFailed`, `ReleaseHalted`, `DeletionCheckFailed`, `CleanupStuck`, `MailStuck`, `ControlPlaneDatabaseDown`, `ControlPlaneDown` | control plane state, see the rule annotations |

### The alert path

Prometheus evaluates the rules, Alertmanager groups them and posts its webhook
to the control plane's `/internal/operator-alerts`, which pushes each alert to
the operator's own browsers through the same Web Push path as the bots' alerts
(every subscription of a tenant that has an operator account). No third-party
notification service is involved in the push; the email fallback below goes
through AhaSend, which already sends the control plane's mail.

- One push per alert per state change: a firing alert is pushed once and again
  at most every 3 hours while it keeps firing (Alertmanager's
  `repeat_interval` is 4 hours); a resolved alert is pushed once as
  "Resolved: ...", and only if its firing push went out.
- At most 50 alerts are read from one notification (Alertmanager sends 20).
  Operator pushes have a budget of their own, apart from every tenant's: 30
  non-critical alerts an hour (the rule's `severity` label), then a single
  operator notice. Alerts with `severity: critical` are never capped. An alert
  refused over the budget is not recorded as pushed (counted as
  `sanholo_cloud_operator_alerts_total{result="capped"}`); Alertmanager repeats
  a firing alert every hour (`repeat_interval: 1h`), so it is pushed within
  about an hour. A capped "resolved" is not sent again: Alertmanager sends a
  resolution once.
- A push that cannot be queued answers 503; Alertmanager retries and only the
  missed alerts are pushed.
- The control plane is the push path, so an alert about the control plane
  itself cannot be pushed while it fires. Two alerts therefore also go by email
  (receiver `operator-email`, route with `continue: true`, so they are pushed as
  well once the push path is back):
  - `ControlPlaneDown`: Prometheus cannot scrape the platform listener, the
    same listener the push intake is on;
  - `ControlPlaneDatabaseDown`: the intake answers, but the operator's push
    subscriptions are read from the database (`push.Store.forOperators`).
  Every other alert is push only. With the control plane down,
  `ControlPlaneDatabaseDown` cannot fire (its metric is absent) and is
  inhibited anyway; `ControlPlaneDown` is the email.

### Email from Alertmanager (AhaSend)

Alertmanager sends that email through AhaSend's SMTP relay, the provider the
control plane already uses (`SMTP_HOST` etc., README). The `global` block of
`alertmanager.yml` holds placeholders to fill in:

| Setting | Value |
| --- | --- |
| `smtp_smarthost` | the AhaSend SMTP host and port, as the control plane's `SMTP_HOST:SMTP_PORT` (`send.ahasend.com:587`, STARTTLS) |
| `smtp_from` | a sender address on a domain the AhaSend account may send from, as `SMTP_FROM` |
| `smtp_auth_username` | the username of an AhaSend SMTP credential; a credential of its own for Alertmanager is better than sharing the control plane's, so either can be revoked alone |
| `smtp_auth_password_file` | `/etc/alertmanager/secrets/ahasend-smtp-password`: that credential's password, stored in qm and delivered as a file, never inline |
| `receivers[operator-email].email_configs[].to` | the operator's own address |

`smtp_require_tls: true` refuses to send over a connection without STARTTLS.
Alertmanager reads the password file when it sends, so a rotated password needs
no reload. Firewall: Alertmanager out to the AhaSend host on 587.

Check the path once after deploying: stop the control plane (or block its
platform listener from Prometheus) for over 5 minutes and confirm the email
arrives, then that the "resolved" email follows when it is back.

### Dashboard

`grafana-dashboard.json` is a Grafana dashboard (import it; the Prometheus
data source is the `datasource` variable, the `tenant` variable filters the
tenant panels). Its queries, which also work in Prometheus' own expression
browser (replace `$tenant` with `.*`):

**Tenants**

- API requests per minute:
  `sum by (tenant) (rate(sanholo_api_requests_total{job="sanholo-tenants",tenant=~"$tenant"}[5m])) * 60`
- Game throttles (429) per 10 minutes:
  `sum by (tenant) (increase(sanholo_api_throttled_total{job="sanholo-tenants",tenant=~"$tenant"}[10m]))`
- Bot restarts in the last hour (all allocations):
  `sum by (tenant) (max_over_time(nomad_client_allocs_restart{exported_job=~"sanholo-tenant-.+",tenant=~"$tenant"}[1h]) - (last_over_time(nomad_client_allocs_restart{exported_job=~"sanholo-tenant-.+",tenant=~"$tenant"}[1h] offset 1h) or max_over_time(nomad_client_allocs_restart{exported_job=~"sanholo-tenant-.+",tenant=~"$tenant"}[1h]) * 0))`
- Bot memory (from the Go runtime):
  `sum by (tenant) (go_memstats_sys_bytes{job="sanholo-tenants",tenant=~"$tenant"})`
  `sum by (tenant) (go_memstats_heap_inuse_bytes{job="sanholo-tenants",tenant=~"$tenant"})`
- Bot action errors per hour:
  `sum by (tenant) (increase(sanholo_errors_total{job="sanholo-tenants",tenant=~"$tenant"}[1h]))`
- Bot ticks per minute:
  `sum by (tenant) (rate(sanholo_bot_ticks_total{job="sanholo-tenants",tenant=~"$tenant"}[5m])) * 60`

**Control plane**

- Tenants by status:
  `sanholo_cloud_tenants{job="sanholo-cloud"}`
- Instances by state:
  `sanholo_cloud_instances{job="sanholo-cloud"}`
- Instances by health:
  `sanholo_cloud_instances_health{job="sanholo-cloud"}`
- qm and database:
  `sanholo_cloud_qm_up{job="sanholo-cloud"}`
  `sanholo_cloud_qm_sealed{job="sanholo-cloud"}`
  `sanholo_cloud_database_up{job="sanholo-cloud"}`
- Web Push messages per minute:
  `sum by (result) (rate(sanholo_cloud_push_messages_total{job="sanholo-cloud"}[5m])) * 60`
- Alerts not pushed, last hour:
  `sum by (reason) (increase(sanholo_cloud_push_alerts_dropped_total{job="sanholo-cloud"}[1h]))`
  `increase(sanholo_cloud_push_cap_hits_total{job="sanholo-cloud"}[1h])`
- Operator alerts, last hour:
  `sum by (result) (increase(sanholo_cloud_operator_alerts_total{job="sanholo-cloud"}[1h]))`
- Releases and rollout:
  `sanholo_cloud_releases{job="sanholo-cloud"}`
  `sanholo_cloud_rollout_tenants{job="sanholo-cloud"}`
- Account deletions:
  `sanholo_cloud_tenant_deletions{job="sanholo-cloud"}`
  `sanholo_cloud_tenant_deletions_stalled{job="sanholo-cloud"}`
- Pending cleanups:
  `sanholo_cloud_pending_cleanups{job="sanholo-cloud"}`
  `sanholo_cloud_pending_cleanup_max_attempts{job="sanholo-cloud"}`
- Oldest waiting work:
  `sanholo_cloud_pending_cleanup_oldest_seconds{job="sanholo-cloud"}`
  `sanholo_cloud_mail_outbox_oldest_queued_seconds{job="sanholo-cloud"}`
- Mail outbox:
  `sanholo_cloud_mail_outbox{job="sanholo-cloud"}`
- Control plane memory:
  `go_memstats_sys_bytes{job="sanholo-cloud"}`
  `go_memstats_heap_inuse_bytes{job="sanholo-cloud"}`
- Control plane goroutines:
  `go_goroutines{job="sanholo-cloud"}`

The tenant panels need the tenant scrape (clients 0.8.0 and later) and, for
restarts, the Nomad scrape. Memory is the bot's Go runtime
(`go_memstats_*`), which the client exports; Nomad's per-allocation memory
metrics depend on the Nomad version and are not used.

Checks run on these files: `promtool check config` and `check rules`,
`promtool test rules sanholo.rules.test.yml` (each US-023 alert at and around
its threshold), `amtool check-config`, `amtool config routes test` (the two
control plane alerts reach `operator-email,sanholo-operator`, every other alert
`sanholo-operator` only), and a real Alertmanager 0.28 posting a firing and a
resolved alert to the handler.

## 10. Backups and restore (US-022)

### What is backed up, and where

Nightly, encrypted with restic (AES-256, the repository password), to one
restic repository in Hetzner Object Storage in **a different location** from
the servers (servers in Falkenstein or Nuremberg: a bucket in Helsinki, say).
Retention is 30 days (`restic forget --keep-within 30d --prune`, measured from
the newest snapshot, over the whole repository, so a node that left keeps no
tenant data past 30 days either). It runs after every database backup run,
also when the dump failed, so a dump that keeps failing does not keep data
past the window.

| Target (restic tag) | Job | When (UTC) | What |
| --- | --- | --- | --- |
| `volumes` | `sanholo-backup-volumes` (sysbatch, every tenant node) | 02:30 | every tenant volume; each SQLite catalog copied with SQLite's online backup; `volumes.json` (volume id to `sanholo-tenant-<id>`) |
| `postgres` | `sanholo-backup-db`, group `postgres` | 03:15 | `pg_dump --format=custom` of the control plane database, then retention |
| `files` | `sanholo-backup-db`, group `deletion-log` | 03:15 | the deletion log (`DELETION_LOG_PATH`, section 8) |

qm is not in this repository: it is backed up per qm's own procedure (below).

Files: `deploy/backup/Dockerfile` and `backup.sh` (the image),
`deploy/nomad/sanholo-backup-db.nomad.hcl`,
`deploy/nomad/sanholo-backup-volumes.nomad.hcl`.

### Setting it up (once)

1. Build and push the image: `docker build -t ghcr.io/tydewhatmay/sanholo-backup:1
   deploy/backup`, then pin both jobs to its digest.
2. Create the bucket in the second location and an access key for it only.
3. Create the repository once, by hand: `restic -r s3:https://<location>.your-objectstorage.com/<bucket>
   init`. The jobs never create it: a mistyped repository would otherwise
   become a new, empty one. Keep the repository password in qm **and** offline
   with the operator: without it no backup can be read.
4. Namespace `sanholo-ops` for the two jobs (the control plane's token, limited
   to `sanholo`, cannot change them). Nomad Variables in it:
   - `nomad/jobs`: `restic_repository`, `restic_password`,
     `s3_access_key_id`, `s3_secret_access_key` (both jobs);
   - `nomad/jobs/sanholo-backup-db`: `pg_host`, `pg_port`, `pg_user`,
     `pg_password`, `pg_database`, `pg_sslmode`, for a PostgreSQL role that can
     read every table (`pg_read_all_data`) and nothing else.
5. Node meta and host volumes:
   - control plane host: `meta { sanholo_control_plane = "true" }`, host
     volumes `sanholo-deletion-log` (the directory of `DELETION_LOG_PATH`) and
     `node-exporter-textfile`;
   - tenant nodes: `meta { sanholo_tenants = "true" }` (as in section 2), host
     volumes `sanholo-tenant-volumes` (the `host_volumes_dir`) and
     `node-exporter-textfile`.
6. The volume job reads the volume list through the Task API with its workload
   identity. Attach a policy to it: `namespace "sanholo" { capabilities =
   ["host-volume-read"] }`, applied with `nomad acl policy apply -namespace
   sanholo-ops -job sanholo-backup-volumes ...`. Without it the backup still
   runs and logs that `volumes.json` is missing.
7. Run each job once (`nomad job periodic force`) and confirm
   `sanholo_backup_last_success_timestamp_seconds` for `postgres`, `files` and
   every tenant node's `volumes` in Prometheus.

### How a failure is noticed

Each run writes `sanholo_backup_last_attempt_ok{target}` and
`..._last_attempt_timestamp_seconds{target}` to the node's textfile directory,
and `..._last_success_timestamp_seconds{target}` on success only.
`BackupFailed` fires on a failed run, `BackupStale` when a target's last
success is older than 26 hours (the job did not run), `BackupMissing` when no
database backup has ever reported. The retention is reported on its own:
`sanholo_backup_last_retention_ok{target="postgres"}` and the
`BackupRetentionFailed` alert. A run whose backup succeeded but whose
retention failed still counts as a successful backup and exits 3, so the job
shows the failure too.

### Restore

Order matters. **Deleted accounts must stay deleted** (section 8): the
database restore is followed by `sanholo-cloud deletions reapply` **before the
control plane takes traffic**, and tenant volumes are restored only after it.

1. **qm, if it was lost.** Follow qm's own procedure; this repository does not
   define it. What qm's `docs/OPERATIONS.md` (lhaig/qm) says applies: qm comes
   up sealed and needs the Shamir unseal shares ("Initialize and unseal"); a
   restored, older data directory is refused by anti-rollback until it is
   re-anchored on purpose with `qm operator reanchor` ("Anti-rollback
   enforcement"). The qm docs name no backup command yet: the qm session owns
   that. A restored qm holds the game tokens of tenants deleted since its
   backup; step 4 purges them again. When only qm was restored (the
   database is intact), run step 4 alone before letting tenants start.
2. **Keep the control plane stopped** (and the edge pointing elsewhere) until
   step 5.
3. **Database.** Into an empty database:

   ```sh
   restic snapshots --tag postgres                       # pick the snapshot
   restic dump <snapshot> /<database>.dump > sanholo.dump
   pg_restore --no-owner --no-privileges -d <database> sanholo.dump
   ```

   **Deletion log:** if the host that held `DELETION_LOG_PATH` was lost, put
   the newest copy back. The job backs up the log's directory as
   `/deletion-log`, so: `restic dump latest --tag files /deletion-log/<file
   name> > "$DELETION_LOG_PATH"`. Never put an older log over a newer one.
4. **Re-apply deletions** (section 8): `sanholo-cloud deletions reapply` with
   the production environment, repeated until it reports no failure. It stops
   and purges the jobs, deletes the volumes and purges qm for every account
   deleted after the backup. For a tenant in the log that the database no
   longer holds, it purges qm only (a restored qm may still hold its token).
   The purge contract is final (ADR 0024), but until the route is deployed
   on the production qm the purge is refused (403) and stays pending: this
   then reports a failure for each such tenant.
5. **Start the control plane**, open `/admin`: no deletion listed as failed,
   instances healthy. Let the edge send traffic again.
6. **Tenant volumes**, for tenants the restored database still holds, when a
   tenant node was lost (a running node keeps its volumes):
   1. `restic snapshots --tag volumes --host <lost node>`, then
      `restic restore <snapshot> --target /restore`. Files land in
      `/restore/stage/volumes/<old volume id>/`; `volumes.json` next to them
      maps each volume id to its name, `sanholo-tenant-<id>`.
   2. In `/admin`, stop the tenant's instance, and delete its old volume record
      (`nomad volume delete -type host -namespace sanholo <old id>`).
   3. Create the volume again on a healthy tenant node with the control plane's
      settings (name `sanholo-tenant-<id>`, plugin `mkdir`, `single-node-writer`
      / `file-system`, parameters `mode=0750`, `uid=65532`, `gid=65532`;
      section 2). The control plane reuses an existing volume of that name.
   4. Copy the restored files into `<host_volumes_dir>/<new volume id>/` and
      `chown -R 65532:65532` them.
   5. Start the instance from `/admin`.

### Restore drill (launch gate)

Run on staging before the beta opens (US-022, `saas-launch-checklist.md`):
restore qm per its procedure, then steps 2 to 6 above for the database, the
deletion log and one tenant volume, with at least one account deleted after the
backup was taken (it must stay deleted). Time it from "start of step 1" to
"Bridge of the restored tenant shows its fleet".

| Date | Who | qm | Database + reapply | Tenant volume | Total | Notes |
| --- | --- | --- | --- | --- | --- | --- |
| _TBD_ | _TBD_ | _TBD_ | _TBD_ | _TBD_ | _TBD_ | |

What was checked before hand-off: the image builds; `backup.sh` passes
shellcheck and ran end to end against a local restic repository and
PostgreSQL 17 (a missing repository fails and records `ok 0`; a failed
`pg_dump` fails the run and keeps the last success time; the restored dump
and the restored SQLite catalog read back their rows); `nomad fmt` and
`nomad job validate`/`plan` on a Nomad 2.0.6 dev agent. Not exercised: Hetzner
Object Storage, the host volumes and Task API on real nodes, and the
workload-associated ACL policy.
