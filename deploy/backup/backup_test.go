// Package backup holds the tests of backup.sh, the nightly backup job's
// script. They run it with sh and a stub restic (a shell function), so they
// need no repository, no database and no network.
package backup

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runner defines restic as a function (it takes precedence over PATH), then
// sources the script as "backup.sh postgres". The stub logs each call and
// answers backup and forget with the exit codes in the environment.
const runner = `restic() {
	echo "$1" >>"$STUB_LOG"
	case "$1" in
	backup) return "$STUB_BACKUP_RC" ;;
	forget) return "$STUB_FORGET_RC" ;;
	esac
	return 0
}
set -- postgres
. ./backup.sh`

type run struct {
	exit    int
	calls   []string
	metrics string
}

func runPostgres(t *testing.T, backupRC, forgetRC string) run {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	cmd := exec.Command("sh", "-c", runner)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"RESTIC_REPOSITORY=s3:example.invalid/x", "RESTIC_PASSWORD_FILE=/nonexistent",
		"TEXTFILE_DIR=" + dir, "NOMAD_TASK_DIR=" + dir, "BACKUP_HOST=cp",
		"PGHOST=db.invalid", "PGDATABASE=sanholo",
		"STUB_LOG=" + log, "STUB_BACKUP_RC=" + backupRC, "STUB_FORGET_RC=" + forgetRC,
	}
	out, err := cmd.CombinedOutput()
	var r run
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		r.exit = ee.ExitCode()
	case err != nil:
		t.Fatalf("run: %v\n%s", err, out)
	}
	if b, err := os.ReadFile(log); err == nil {
		r.calls = strings.Fields(string(b))
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.prom"))
	for _, f := range files {
		b, _ := os.ReadFile(f)
		r.metrics += string(b)
	}
	return r
}

func TestRetentionRunsAndIsReportedOnItsOwn(t *testing.T) {
	cases := []struct {
		name               string
		backupRC, forgetRC string
		exit               int
		want, notWant      []string
	}{
		{"both succeed", "0", "0", 0,
			[]string{`sanholo_backup_last_attempt_ok{target="postgres"} 1`, `sanholo_backup_last_retention_ok{target="postgres"} 1`, "sanholo_backup_last_success_timestamp_seconds"}, nil},
		{"the dump fails, retention still runs", "1", "0", 1,
			[]string{`sanholo_backup_last_attempt_ok{target="postgres"} 0`, `sanholo_backup_last_retention_ok{target="postgres"} 1`},
			[]string{"sanholo_backup_last_success_timestamp_seconds"}},
		{"retention fails, the backup still counts", "0", "1", 3,
			[]string{`sanholo_backup_last_attempt_ok{target="postgres"} 1`, `sanholo_backup_last_retention_ok{target="postgres"} 0`, "sanholo_backup_last_success_timestamp_seconds"}, nil},
		{"both fail", "1", "1", 1,
			[]string{`sanholo_backup_last_attempt_ok{target="postgres"} 0`, `sanholo_backup_last_retention_ok{target="postgres"} 0`},
			[]string{"sanholo_backup_last_success_timestamp_seconds"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := runPostgres(t, tc.backupRC, tc.forgetRC)
			if r.exit != tc.exit {
				t.Errorf("exit = %d, want %d", r.exit, tc.exit)
			}
			if strings.Join(r.calls, ",") != "cat,backup,forget" {
				t.Errorf("restic calls = %v, want cat, backup, forget", r.calls)
			}
			for _, w := range tc.want {
				if !strings.Contains(r.metrics, w) {
					t.Errorf("metrics lack %q:\n%s", w, r.metrics)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(r.metrics, w) {
					t.Errorf("metrics hold %q:\n%s", w, r.metrics)
				}
			}
		})
	}
}
