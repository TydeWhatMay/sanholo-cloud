# Norman Config

## Session Limits
max_tasks_per_session: 15
warn_at_tasks: 12

## Project
name: SanHolo Hosted (sanholo-cloud)
repo: /home/stokvis/dev/TydeWhatMay/sanholo-cloud
created: 2026-10-26

## Commands
build: go build ./...
vet: go vet ./...
test: scripts/test-db.sh go test ./... -count=1   # starts a throwaway Postgres container
fmt: test -z "$(gofmt -l .)"
lint: gosec -quiet ./...

## Subagent Defaults
default_subagent: golang-pro

## Model Strategy
advisor_mode: auto
advisor_model: opus
default_model: sonnet
quick_model: haiku
progress_compress_after: 10
verify_rigor: single

## Cleanup
sweep_mode: offer
