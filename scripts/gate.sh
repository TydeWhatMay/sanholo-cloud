#!/usr/bin/env bash
# The per-task gate: every check runs, every result prints, any failure
# makes the exit status non-zero.
cd "$(dirname "$0")/.."
fail=0

run() {
	local name="$1"
	shift
	if out="$("$@" 2>&1)"; then
		echo "PASS $name"
	else
		echo "FAIL $name"
		echo "$out" | sed 's/^/    /'
		fail=1
	fi
}

fmt_check() {
	local out
	out="$(gofmt -l .)"
	[ -z "$out" ] || { echo "$out"; return 1; }
}

run "gofmt -l (empty)" fmt_check
run "go build ./..." go build ./...
run "go vet ./..." go vet ./...
run "go test ./... (test database)" scripts/test-db.sh go test ./... -count=1
run "gosec" gosec -quiet ./...

if [ "$fail" -ne 0 ]; then
	echo "GATE FAILED"
	exit 1
fi
echo "GATE PASSED"
