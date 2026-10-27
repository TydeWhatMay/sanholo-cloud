# CLAUDE.md — SanHolo Hosted (sanholo-cloud)

Guidance for Claude Code in this repository. Read this first, then
`prds/backlog/prd-sanholo-saas.md` (the PRD both repositories share).

## What this is

The hosted service around the SanHolo client (`TydeWhatMay/san-holo`). It owns:

- the control plane (`cmd/sanholo-cloud`): waitlist and invites, magic-link
  and passkey sign-in, the account pages on `app.sanholo.space`, game-account
  onboarding, the authenticating proxy to each customer's Bridge on
  `<tenant-id>.sanholo.space`, Web Push delivery, export and delete, admin;
- tenant provisioning: one Nomad job per customer (`sanholo-tenant-<id>`, one
  task: the customer's bot, image `ghcr.io/tydewhatmay/sanholo:X.Y.Z`), registered
  through the Nomad HTTP API from the sign-up flow;
- the updater: rolls each client release to every tenant in stages (canary,
  then batches) and reverts on failed health;
- infrastructure (`deploy/`): Nomad jobs, edge, backups.

The client stays in `TydeWhatMay/san-holo`; its hosted-mode work (Phase 86 there)
sits behind `-hosted`. This repository consumes the client only as a released
container image.

Story ownership: this repository builds the **[cloud]** stories of the PRD
(US-002, US-009 to US-015, the delivery side of US-016, US-017 to US-025).

## Commands

```sh
go build ./...
go vet ./...
go test ./... -count=1
gofmt -l .        # must print nothing
gosec -quiet ./...
scripts/gate.sh   # full gate (needs Docker)
DATABASE_URL=postgres://... go run ./cmd/sanholo-cloud
```

## Conventions

- Go + Fiber v2 (never gorilla/mux, chi, gin or echo), htmx + minimal JS,
  static assets vendored under `static/` (no CDN). PostgreSQL with
  parameterised queries and migrations in the repository.
- `fiber.Config{Immutable: true}` and `recover` first in the chain (both
  already set in `internal/server`). Any GET with a side effect must ignore
  HEAD and prefetch requests.
- Tests first: a failing test for each acceptance criterion, table-driven
  where it fits; mutation-test every negative assertion.
- Conventional commits, no emojis, no Claude co-author or session trailers.
- Company-facing copy (sign-up pages, emails, privacy notice, terms) is
  written in the TydeWhatMay company register: plain facts, the company as
  subject, one fact per sentence.

## Secrets and security (non-negotiable)

- Customer game tokens live only in the dedicated production qm under
  `secret/data/sanholo/tenants/<id>/game`. The sign-up service writes and
  purges; it never reads or lists. Tokens are never stored in PostgreSQL, never
  logged, never shown back after entry.
- Tenant ids follow qm's rule: 1 to 63 characters, lowercase letters, digits
  and hyphens, starting and ending with a letter or digit (they become part
  of the job id and the qm path).
- qm health: check `GET /qm/v1/sys/health` (always 200; `initialized` and
  `sealed` flags). The control plane authenticates with an AppRole.
- Every deletion goes through one code path (`secrets.PurgeTenant`);
  a rotation is a plain `secrets.Put`.
- Each tenant job authenticates to qm with its Nomad workload identity
  (audience `qm.sanholo`); no secret is ever placed in a job. The qm
  integration contract is recorded in `prds/research/qm-contract.md`; tests
  run against the fake qm in `internal/secrets/qmfake`.
- Rate-limit keys behind the edge use the forwarded address only when it
  comes from the edge.
- Use `gh-qm`, never `gh`. Never configure git credentials. The development
  qm on this machine must never hold SanHolo Hosted secrets.

## Launch gates

The beta opens only when `prds/research/saas-launch-checklist.md` (US-025) is
fully ticked: developer permission, isolation tests, restore drill, a norman
security-skill review with no open HIGH or MEDIUM, privacy pages, operator
alerting, and seven clean days on the operator's canary tenant.
