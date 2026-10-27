# The backup image the Nomad backup jobs run (deploy/README.md, section 9).
# Build and push it with the version in the tag, then pin the jobs to its
# digest:  docker build -t ghcr.io/tydewhatmay/sanholo-backup:1 deploy/backup
FROM alpine:3.22
# restic 0.18 (--stdin-from-command, --retry-lock), sqlite (online backup of
# the tenant catalogs), the PostgreSQL 17 client (pg_dump matching the server),
# curl (the Nomad Task API, for the volume manifest).
RUN apk add --no-cache ca-certificates curl restic sqlite postgresql17-client \
	&& mkdir -m 1777 /stage
COPY backup.sh /usr/local/bin/sanholo-backup
# Root reads the tenant volumes (mode 0750, uid 65532) and writes the
# node_exporter textfile directory. The task runs with no network beyond the
# repository and the database.
ENTRYPOINT ["/usr/local/bin/sanholo-backup"]
