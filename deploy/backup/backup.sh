#!/bin/sh
# sanholo-backup: nightly encrypted backups to the restic repository in the
# second Hetzner location (deploy/README.md, section 9).
#
#   sanholo-backup postgres   pg_dump of the control plane database, then
#                             the 30-day retention (forget + prune), which
#                             runs even when the dump failed
#   sanholo-backup volumes    every tenant volume on this node (SQLite copied
#                             with the online backup API, not as a raw file)
#   sanholo-backup files      the files named in BACKUP_FILES (the deletion log)
#
# Environment (secrets come from files rendered by Nomad, never from the job):
#   RESTIC_REPOSITORY, RESTIC_PASSWORD_FILE, AWS_ACCESS_KEY_ID,
#   AWS_SECRET_ACCESS_KEY   restic's own variables
#   TEXTFILE_DIR            node_exporter textfile collector directory
#   BACKUP_HOST             restic --host for this run (default: hostname)
#   postgres: PGHOST, PGPORT, PGUSER, PGDATABASE, PGPASSFILE, PGSSLMODE
#   volumes:  VOLUMES_DIR (the Nomad client's host_volumes_dir). With the
#             Nomad Task API socket and NOMAD_TOKEN (the workload identity)
#             present, the host volume list of VOLUME_NAMESPACE (default
#             sanholo) is saved as volumes.json: the volume id -> tenant
#             job name manifest a restore needs.
#   files:    BACKUP_FILES (space separated absolute paths)
#   RETENTION (default 30d)
#
# On exit it writes sanholo_backup_last_attempt_* for its target and, on
# success only, sanholo_backup_last_success_timestamp_seconds, which the
# BackupFailed and BackupStale alerts read. The postgres run also writes
# sanholo_backup_last_retention_ok (BackupRetentionFailed): a failed
# retention is reported there, not as a failed backup, and makes the run
# exit 3.
set -eu

target="${1:-}"
case "$target" in
postgres | volumes | files) ;;
*)
	echo "usage: sanholo-backup postgres|volumes|files" >&2
	exit 2
	;;
esac

: "${RESTIC_REPOSITORY:?RESTIC_REPOSITORY is required}"
: "${RESTIC_PASSWORD_FILE:?RESTIC_PASSWORD_FILE is required}"
: "${TEXTFILE_DIR:?TEXTFILE_DIR is required}"
host="${BACKUP_HOST:-$(hostname)}"
retention="${RETENTION:-30d}"
work="${NOMAD_TASK_DIR:-/tmp}/sanholo-backup"
export RESTIC_CACHE_DIR="$work/cache"
mkdir -p "$work" "$RESTIC_CACHE_DIR"

# write_metric NAME VALUE FILE: an atomic write the collector never sees half
# done (rename within the same directory).
write_metrics() {
	file="$TEXTFILE_DIR/sanholo_backup_${target}_$1.prom"
	tmp="$file.$$"
	cat >"$tmp"
	chmod 0644 "$tmp"
	mv "$tmp" "$file"
}

# Set when the backup itself succeeded and only the retention failed.
retention_only_failed=0

finish() {
	rc=$?
	now=$(date +%s)
	ok=0
	if [ "$rc" -eq 0 ] || [ "$retention_only_failed" -eq 1 ]; then
		ok=1
	fi
	write_metrics attempt <<METRICS
# HELP sanholo_backup_last_attempt_timestamp_seconds Unix time the last backup run of this target ended.
# TYPE sanholo_backup_last_attempt_timestamp_seconds gauge
sanholo_backup_last_attempt_timestamp_seconds{target="$target"} $now
# HELP sanholo_backup_last_attempt_ok 1 when the last backup run of this target succeeded.
# TYPE sanholo_backup_last_attempt_ok gauge
sanholo_backup_last_attempt_ok{target="$target"} $ok
METRICS
	if [ "$ok" -eq 1 ]; then
		write_metrics success <<METRICS
# HELP sanholo_backup_last_success_timestamp_seconds Unix time of the last successful backup of this target.
# TYPE sanholo_backup_last_success_timestamp_seconds gauge
sanholo_backup_last_success_timestamp_seconds{target="$target"} $now
METRICS
	fi
	rm -rf /stage/volumes
	echo "sanholo-backup $target: exit $rc"
}
trap finish EXIT

# The repository must exist: an automatic init would turn a mistyped
# RESTIC_REPOSITORY into a new, empty repository that nobody restores from.
restic cat config >/dev/null

case "$target" in
postgres)
	: "${PGHOST:?PGHOST is required}"
	: "${PGDATABASE:?PGDATABASE is required}"
	# --stdin-from-command fails the backup when pg_dump fails, which a
	# pipe would hide.
	backup_rc=0
	restic backup --host "$host" --tag postgres --retry-lock 30m \
		--stdin-filename "$PGDATABASE.dump" --stdin-from-command -- \
		pg_dump --format=custom --no-owner --no-privileges "$PGDATABASE" || backup_rc=$?
	[ "$backup_rc" -eq 0 ] || echo "sanholo-backup postgres: backup failed (exit $backup_rc)" >&2
	# Retention runs whatever the dump did, so a dump that keeps failing
	# does not keep data past the window. It covers every snapshot in one
	# group, measured from the newest: a node that left keeps no tenant
	# data past the window. Its result is reported on its own.
	retention_rc=0
	restic forget --group-by '' --keep-within "$retention" --prune --retry-lock 30m || retention_rc=$?
	retention_ok=0
	if [ "$retention_rc" -eq 0 ]; then
		retention_ok=1
	else
		echo "sanholo-backup postgres: retention failed (exit $retention_rc)" >&2
	fi
	write_metrics retention <<METRICS
# HELP sanholo_backup_last_retention_ok 1 when the last retention run (restic forget --prune) of this target succeeded.
# TYPE sanholo_backup_last_retention_ok gauge
sanholo_backup_last_retention_ok{target="$target"} $retention_ok
METRICS
	[ "$backup_rc" -eq 0 ] || exit "$backup_rc"
	if [ "$retention_rc" -ne 0 ]; then
		retention_only_failed=1
		exit 3
	fi
	;;
volumes)
	: "${VOLUMES_DIR:?VOLUMES_DIR is required}"
	# A fixed path, so every snapshot restores to the same place:
	# /stage/volumes/<volume id>/...
	stage=/stage/volumes
	rm -rf "$stage"
	mkdir -p "$stage"
	sock="${NOMAD_SECRETS_DIR:-/secrets}/api.sock"
	if [ -S "$sock" ] && [ -n "${NOMAD_TOKEN:-}" ]; then
		if ! curl -fsS --max-time 30 --unix-socket "$sock" -H "X-Nomad-Token: $NOMAD_TOKEN" \
			"http://localhost/v1/volumes?type=host&namespace=${VOLUME_NAMESPACE:-sanholo}" -o "$stage/volumes.json"; then
			rm -f "$stage/volumes.json"
			echo "sanholo-backup volumes: warning: no volume manifest (Task API refused or unreachable)" >&2
		fi
	else
		echo "sanholo-backup volumes: warning: no Task API socket; no volume manifest" >&2
	fi
	n=0
	for dir in "$VOLUMES_DIR"/*/; do
		[ -d "$dir" ] || continue
		id=$(basename "$dir")
		mkdir -p "$stage/$id"
		# Everything as it is, then the catalog replaced by a copy made
		# through SQLite's online backup: a raw copy of a database the bot
		# is writing (and its WAL) is not restorable.
		cp -a "$dir/." "$stage/$id/"
		rm -f "$stage/$id/catalog.db" "$stage/$id/catalog.db-wal" "$stage/$id/catalog.db-shm"
		if [ -f "$dir/catalog.db" ]; then
			sqlite3 "$dir/catalog.db" ".timeout 30000" ".backup '$stage/$id/catalog.db'"
		fi
		n=$((n + 1))
	done
	echo "sanholo-backup volumes: $n volume(s) staged"
	restic backup --host "$host" --tag volumes --retry-lock 30m "$stage"
	;;
files)
	: "${BACKUP_FILES:?BACKUP_FILES is required}"
	# shellcheck disable=SC2086 # BACKUP_FILES is a space separated list
	restic backup --host "$host" --tag files --retry-lock 30m $BACKUP_FILES
	;;
esac
