# sanholo-cloud

The hosted SanHolo service: the control plane, tenant provisioning on Nomad,
the release updater and the infrastructure behind `sanholo.space`.

SanHolo itself (the Replicant Space client, bot and Bridge cockpit) is in
`lhaig/san-holo`. This repository runs released SanHolo container images,
one per customer.

Status: the free, invite-only beta is being built. The requirements are in
`prds/backlog/prd-sanholo-saas.md`.

## Development

```sh
go test ./... -count=1
go run ./cmd/sanholo-cloud -addr 127.0.0.1:8090
curl http://127.0.0.1:8090/healthz
```
