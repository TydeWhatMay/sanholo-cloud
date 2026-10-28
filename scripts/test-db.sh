#!/usr/bin/env bash
# Runs a command against a throwaway postgres:17 container.
# Usage: scripts/test-db.sh <cmd...>
# Exports TEST_DATABASE_URL for the command and always removes the container.
set -euo pipefail

if [ "$#" -eq 0 ]; then
	echo "usage: $0 <cmd...>" >&2
	exit 2
fi

name="sanholo-test-db-$$-$RANDOM"
password="$(head -c 18 /dev/urandom | base64 | tr -dc 'A-Za-z0-9')"
cleanup() { docker rm -fv "$name" >/dev/null 2>&1 || true; }
trap cleanup EXIT INT TERM

# Publishing 127.0.0.1::5432 lets Docker pick a free host port.
docker run -d --name "$name" \
	-e POSTGRES_PASSWORD="$password" -e POSTGRES_DB=sanholo_test \
	-p 127.0.0.1::5432 postgres:17 >/dev/null

port="$(docker port "$name" 5432/tcp | head -n1 | sed 's/.*://')"

# The image restarts the server once after init, so pg_isready can answer
# early; require a real query to succeed twice in a row.
ok=0
for _ in $(seq 1 60); do
	if docker exec "$name" psql -U postgres -d sanholo_test -tAc 'select 1' >/dev/null 2>&1; then
		ok=$((ok + 1))
		[ "$ok" -ge 2 ] && break
	else
		ok=0
	fi
	sleep 1
done
if [ "$ok" -lt 2 ]; then
	echo "test-db: postgres did not become ready" >&2
	docker logs "$name" >&2 || true
	exit 1
fi

export TEST_DATABASE_URL="postgres://postgres:${password}@127.0.0.1:${port}/sanholo_test?sslmode=disable"
# Not exec: the trap must run after the command.
"$@"
