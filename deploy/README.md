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
  (task 3.1).

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
  `0.0.0.0:8080` inside it (`-hosted-allow-any-bind`) and exposes nothing else.
- Nomad maps the port (label `http`) onto the host network named by
  `NOMAD_HOST_NETWORK`. Define that host network on the tenant nodes as the
  private interface (`host_network "private" { cidr = "10.x.x.x/24" }`), so the
  port is never bound on a public address. Leave it empty to use the node's
  default address.
- Firewall (US-013), owned by the infra session:
  - inbound to the mapped ports (the dynamic range, 20000-32000 by default) only
    from the control plane's proxy and Prometheus;
  - tenant tasks may connect out only to `api.replicant.space:443`, qm, and the
    control plane (`/internal/alerts`, `TENANT_ALERT_URL`).
- The control plane finds a tenant's bot from the allocation's `http` host
  address (`nomad.Status.Address`); the service `sanholo-bot` is registered with
  the Nomad provider (tag `tenant=<id>`) as well.

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
| `VAPID_PUBLIC_KEY` | Optional Web Push public key passed to the bots. |
| `TENANT_MEMORY_MB`, `TENANT_CPU_MHZ` | Default 384 and 300. |
| `PROVISION_TIMEOUT` | How long a bot may take to become healthy. Default 15m. |
| `ROLLOUT_BATCH_SIZE`, `ROLLOUT_HEALTH_TIMEOUT` | Tenants per update batch (default 5) and how long one may take to be healthy on the new version (default `PROVISION_TIMEOUT`). The job's own update stanza (`auto_revert`, healthy deadline 10m) still applies per tenant. |
| `ROLLOUT_CANARY_SOAK`, `ROLLOUT_BATCH_SOAK` | Soak after a tenant's deployment turned healthy: 1h for the canary, 10m for other batches (`0` = none). Live check results, restarts and the allocation are watched; see the README. |
| `TENANT_VOLUME_SIZE_MB`, `TENANT_VOLUME_NODE_POOL`, `TENANT_VOLUME_CONSTRAINT` | The tenant volume, section 2. |
| `QM_ADDR` | Also given to each bot as `-qm-addr`. |

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
