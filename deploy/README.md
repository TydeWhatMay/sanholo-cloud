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
    capabilities = ["submit-job", "read-job", "alloc-lifecycle"]
  }
  host_volume "sanholo-tenant-*" {
    policy = "write"
  }
  ```

  `submit-job` covers register and stop/purge, `read-job` covers the job, its
  allocations, its latest deployment and the service listing, `alloc-lifecycle`
  covers restarting an allocation (admin restart and token rotation). A job
  that mounts a host volume needs the `host_volume` rule.
- Pass the token as `NOMAD_TOKEN`, or as `NOMAD_TOKEN_FILE` (re-read on every
  request, so it can be rotated). `NOMAD_ADDR` must be `https` (plain `http` is
  accepted only on a loopback address).
- Workload identity must be enabled (Nomad 1.7+). The job has two identity
  blocks, each written to a file in the task's secrets directory:

  | Identity name | Audience | File the SanHolo client reads |
  | --- | --- | --- |
  | `qm.sanholo` | `qm.sanholo` | `${NOMAD_SECRETS_DIR}/nomad_qm.sanholo.jwt` |
  | `sanholo.cloud` | `sanholo.cloud` | `${NOMAD_SECRETS_DIR}/nomad_sanholo.cloud.jwt` |

The tenant logs in to qm at `auth/nomad` (the jwt method the operator mounts, qm contract Appendix A.1) with role `sanholo-tenant`.

  qm's JWT auth role `sanholo-tenant` must accept audience `qm.sanholo` from
  Nomad's JWKS and map `nomad_job_id` (`sanholo-tenant-<id>`) to the tenant
  prefix. The control plane verifies the second token against the same JWKS
  (task 3.1).

## 2. Host volumes

One host volume per tenant, created before the tenant's job is registered:

- Name: `sanholo-tenant-<tenant id>` (the job id), `type = "host"`.
- Mounted at `/data` in the task; the client keeps its SQLite catalog in
  `/data/catalog.db`. The image runs as `nonroot` (uid/gid 65532), so the
  directory must be writable by that user.
- Task 3.3 (account deletion) must remove the volume and verify it is gone.
- Until volumes are created automatically, a tenant whose volume is missing
  gets a job that cannot be placed: the instance shows as failed after the
  provisioning timeout (`PROVISION_TIMEOUT`, default 15 minutes).

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

## 4. Image

`ghcr.io/tydewhatmay/sanholo:vX.Y.Z` is public, so Nomad pulls without registry
credentials. `SANHOLO_IMAGE_VERSION` (for example `0.7.1`) is the version new
instances start at. An existing instance keeps its own version; the updater
(task 2.5) moves it.

## 5. Control plane environment

| Variable | Meaning |
| --- | --- |
| `NOMAD_ADDR`, `NOMAD_TOKEN` or `NOMAD_TOKEN_FILE` | Required in production. |
| `NOMAD_NAMESPACE` | Default `sanholo`. |
| `NOMAD_REGION`, `NOMAD_DATACENTERS` | Optional region; datacenters default `dc1`. |
| `NOMAD_HOST_NETWORK` | Host network for the port, see section 3. |
| `SANHOLO_IMAGE_VERSION` | Required with `NOMAD_ADDR`. |
| `TENANT_PROXY_PUBLIC_KEYS` or `TENANT_PROXY_PUBLIC_KEYS_FILE` | The proxy's Ed25519 public key(s) (PEM, or base64 one per line; two during a rotation). Public values, written into each job as a template. Required with `NOMAD_ADDR`. Task 2.4 owns the matching private key. |
| `TENANT_DOMAIN` | Default `sanholo.space`; each bot gets `-public-url https://<id>.<domain>`. |
| `TENANT_ALERT_URL` | Where bots post alerts. Default `APP_BASE_URL/internal/alerts`. |
| `VAPID_PUBLIC_KEY` | Optional Web Push public key passed to the bots. |
| `TENANT_MEMORY_MB`, `TENANT_CPU_MHZ` | Default 384 and 300. |
| `PROVISION_TIMEOUT` | How long a bot may take to become healthy. Default 15m. |
| `QM_ADDR` | Also given to each bot as `-qm-addr`. |

No variable that holds a secret is ever copied into a job.
