# Nightly backup of the control plane database and the deletion log
# (US-022, deploy/README.md section 9). Periodic batch job in the operations
# namespace, out of reach of the control plane's Nomad token.
#
# Secrets come from Nomad Variables, readable by this job's workload identity
# only (namespace sanholo-ops):
#   nomad/jobs                       restic_repository, restic_password,
#                                    s3_access_key_id, s3_secret_access_key
#                                    (shared with sanholo-backup-volumes)
#   nomad/jobs/sanholo-backup-db     pg_host, pg_port, pg_user, pg_password,
#                                    pg_database, pg_sslmode
job "sanholo-backup-db" {
  namespace   = "sanholo-ops"
  type        = "batch"
  datacenters = ["dc1"]

  # Both groups run on the control plane's host, so each backup target's
  # textfile metrics always come from the same node_exporter (a target that
  # moved between nodes would leave a stale file behind and a false alert).
  constraint {
    attribute = "${meta.sanholo_control_plane}"
    value     = "true"
  }

  periodic {
    crons            = ["15 3 * * *"]
    time_zone        = "UTC"
    prohibit_overlap = true
  }

  # pg_dump, then retention (restic forget --prune over the whole
  # repository). Runs after the volume job (02:30) so the prune rarely
  # waits for its lock.
  group "postgres" {
    restart {
      attempts = 2
      delay    = "5m"
      mode     = "fail"
    }

    volume "textfile" {
      type      = "host"
      source    = "node-exporter-textfile"
      read_only = false
    }

    task "backup" {
      driver = "docker"

      config {
        # Pin to the digest of the pushed image (deploy/backup/Dockerfile).
        image = "ghcr.io/tydewhatmay/sanholo-backup:1"
        args  = ["postgres"]
      }

      volume_mount {
        volume      = "textfile"
        destination = "/textfile"
      }

      env {
        TEXTFILE_DIR         = "/textfile"
        BACKUP_HOST          = "sanholo-postgres"
        RESTIC_PASSWORD_FILE = "/secrets/restic-password"
        PGPASSFILE           = "/secrets/pgpass"
        RETENTION            = "30d"
      }

      template {
        destination = "${NOMAD_SECRETS_DIR}/restic.env"
        env         = true
        data        = <<-EOT
        {{ with nomadVar "nomad/jobs" }}
        RESTIC_REPOSITORY={{ .restic_repository }}
        AWS_ACCESS_KEY_ID={{ .s3_access_key_id }}
        AWS_SECRET_ACCESS_KEY={{ .s3_secret_access_key }}
        {{ end }}
        {{ with nomadVar "nomad/jobs/sanholo-backup-db" }}
        PGHOST={{ .pg_host }}
        PGPORT={{ .pg_port }}
        PGUSER={{ .pg_user }}
        PGDATABASE={{ .pg_database }}
        PGSSLMODE={{ .pg_sslmode }}
        {{ end }}
        EOT
      }

      template {
        destination = "${NOMAD_SECRETS_DIR}/restic-password"
        perms       = "0400"
        data        = "{{ with nomadVar \"nomad/jobs\" }}{{ .restic_password }}{{ end }}"
      }

      template {
        destination = "${NOMAD_SECRETS_DIR}/pgpass"
        perms       = "0400"
        data        = "{{ with nomadVar \"nomad/jobs/sanholo-backup-db\" }}*:*:*:*:{{ .pg_password }}{{ end }}"
      }

      resources {
        cpu    = 300
        memory = 512
      }
    }
  }

  # The deletion log (DELETION_LOG_PATH, section 8) lives outside the
  # database on the control plane's host; it is copied off with the rest.
  group "deletion-log" {
    restart {
      attempts = 2
      delay    = "5m"
      mode     = "fail"
    }

    volume "deletion-log" {
      type      = "host"
      source    = "sanholo-deletion-log"
      read_only = true
    }

    volume "textfile" {
      type      = "host"
      source    = "node-exporter-textfile"
      read_only = false
    }

    task "backup" {
      driver = "docker"

      config {
        image = "ghcr.io/tydewhatmay/sanholo-backup:1"
        args  = ["files"]
      }

      volume_mount {
        volume      = "deletion-log"
        destination = "/deletion-log"
        read_only   = true
      }

      volume_mount {
        volume      = "textfile"
        destination = "/textfile"
      }

      env {
        TEXTFILE_DIR         = "/textfile"
        BACKUP_HOST          = "sanholo-control-plane"
        BACKUP_FILES         = "/deletion-log"
        RESTIC_PASSWORD_FILE = "/secrets/restic-password"
      }

      template {
        destination = "${NOMAD_SECRETS_DIR}/restic.env"
        env         = true
        data        = <<-EOT
        {{ with nomadVar "nomad/jobs" }}
        RESTIC_REPOSITORY={{ .restic_repository }}
        AWS_ACCESS_KEY_ID={{ .s3_access_key_id }}
        AWS_SECRET_ACCESS_KEY={{ .s3_secret_access_key }}
        {{ end }}
        EOT
      }

      template {
        destination = "${NOMAD_SECRETS_DIR}/restic-password"
        perms       = "0400"
        data        = "{{ with nomadVar \"nomad/jobs\" }}{{ .restic_password }}{{ end }}"
      }

      resources {
        cpu    = 100
        memory = 256
      }
    }
  }
}
