# Nightly backup of every tenant volume (US-022, deploy/README.md section 9).
# A periodic sysbatch job: one run on every tenant node, because a dynamic
# host volume lives on the node it was created on.
#
# Each node needs two static host volumes in its client config:
#   host_volume "sanholo-tenant-volumes" { path = "<host_volumes_dir>" }
#   host_volume "node-exporter-textfile" { path = "<node_exporter textfile dir>" }
# The first is the parent directory of the tenants' dynamic host volumes
# (one directory per volume id). It is mounted read-write because SQLite's
# online backup needs the -shm file of a WAL database; the script only reads.
#
# The volume manifest (volume id -> sanholo-tenant-<id>) is read through the
# Task API with this job's workload identity. Attach a policy to the job:
#   namespace "sanholo" { capabilities = ["host-volume-read"] }
#   nomad acl policy apply -namespace sanholo-ops -job sanholo-backup-volumes \
#     sanholo-backup-volumes-read volumes-read.hcl
#
# Secrets: Nomad Variable nomad/jobs in namespace sanholo-ops (restic_repository,
# restic_password, s3_access_key_id, s3_secret_access_key).
job "sanholo-backup-volumes" {
  namespace   = "sanholo-ops"
  type        = "sysbatch"
  datacenters = ["dc1"]

  periodic {
    crons            = ["30 2 * * *"]
    time_zone        = "UTC"
    prohibit_overlap = true
  }

  # The same nodes TENANT_VOLUME_CONSTRAINT selects (section 2).
  constraint {
    attribute = "${meta.sanholo_tenants}"
    value     = "true"
  }

  group "volumes" {
    restart {
      attempts = 2
      delay    = "5m"
      mode     = "fail"
    }

    volume "tenant-volumes" {
      type      = "host"
      source    = "sanholo-tenant-volumes"
      read_only = false
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
        args  = ["volumes"]
      }

      # NOMAD_TOKEN in the environment authenticates to the Task API socket
      # (${NOMAD_SECRETS_DIR}/api.sock).
      identity {
        env = true
      }

      volume_mount {
        volume      = "tenant-volumes"
        destination = "/volumes"
      }

      volume_mount {
        volume      = "textfile"
        destination = "/textfile"
      }

      env {
        TEXTFILE_DIR         = "/textfile"
        VOLUMES_DIR          = "/volumes"
        VOLUME_NAMESPACE     = "sanholo"
        BACKUP_HOST          = "${node.unique.name}"
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
        cpu    = 300
        memory = 512
      }
    }
  }
}
