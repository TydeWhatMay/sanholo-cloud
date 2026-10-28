package metrics

import (
	"bufio"
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/lhaig/sanholo-cloud/internal/dbtest"
	"github.com/lhaig/sanholo-cloud/internal/push"
	"github.com/lhaig/sanholo-cloud/internal/secrets"
)

// parse reads an exposition into "name{labels}" -> value, and checks that
// every sample's family was declared with HELP and TYPE before it.
func parse(t *testing.T, text string) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	typed := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := sc.Text()
		if f, ok := strings.CutPrefix(line, "# TYPE "); ok {
			typed[strings.Fields(f)[0]] = true
			continue
		}
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		key, val := line[:i], line[i+1:]
		name, _, _ := strings.Cut(key, "{")
		if !typed[name] {
			t.Errorf("sample %q has no TYPE line before it", key)
		}
		if _, dup := out[key]; dup {
			t.Errorf("duplicate sample %q", key)
		}
		v, err := strconv.ParseFloat(val, 64)
		if err != nil {
			t.Fatalf("value of %q: %v", key, err)
		}
		out[key] = v
	}
	return out
}

func scrape(t *testing.T, s *Source) map[string]float64 {
	t.Helper()
	var b strings.Builder
	if err := s.Write(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	return parse(t, b.String())
}

func exec(t *testing.T, db Querier, sql string, args ...any) {
	t.Helper()
	rows, err := db.Query(context.Background(), sql, args...)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func TestDatabaseFamilies(t *testing.T) {
	pool := dbtest.Migrated(t)
	for _, q := range []string{
		`INSERT INTO tenants (id, status) VALUES ('a1','active'),('a2','active'),('a3','active'),('p1','pending'),('s1','suspended')`,
		`INSERT INTO tenant_instances (tenant_id, job_id, image_version, state, last_health) VALUES
			('a1','sanholo-tenant-a1','0.7.1','running','healthy'),
			('a2','sanholo-tenant-a2','0.7.1','failed','unhealthy'),
			('a3','sanholo-tenant-a3','0.7.1','running','unhealthy')`,
		`INSERT INTO releases (version, state) VALUES ('0.7.1','done'),('0.7.2','halted'),('0.7.0','done')`,
		`INSERT INTO rollout_tenants (release_version, tenant_id, batch, state, previous_version) VALUES
			('0.7.2','a1',0,'done','0.7.1'),('0.7.2','a2',1,'reverted','0.7.1'),('0.7.2','a3',1,'pending','0.7.1'),
			('0.7.1','a1',0,'done','0.7.0')`,
		`INSERT INTO pending_cleanups (tenant_key, kind, requested_at, attempts) VALUES
			('gone1','volume', now() - interval '2 hours', 7),('gone2','volume', now() - interval '5 minutes', 1)`,
		`INSERT INTO mail_outbox (to_email, subject, body, created_at) VALUES ('x@example.org','s','b', now() - interval '10 minutes')`,
		`INSERT INTO mail_outbox (to_email, subject, body, failed_at) VALUES ('y@example.org','s','b', now()),('z@example.org','s','b', now())`,
		`INSERT INTO mail_outbox (to_email, subject, body, sent_at) VALUES ('w@example.org','s','b', now())`,
		`INSERT INTO tenant_deletions (tenant_key, requested_at, check_state) VALUES ('d1', now(), 'failed'),('d2', now(), 'passed'),('d3', now(), 'passed')`,
	} {
		exec(t, pool, q)
	}
	m := scrape(t, &Source{DB: pool, Version: "1.2.3"})

	want := map[string]float64{
		`sanholo_cloud_build_info{version="1.2.3"}`:          1,
		`sanholo_cloud_database_up`:                          1,
		`sanholo_cloud_tenants{status="active"}`:             3,
		`sanholo_cloud_tenants{status="pending"}`:            1,
		`sanholo_cloud_tenants{status="suspended"}`:          1,
		`sanholo_cloud_tenants{status="deleting"}`:           0, // zero-filled
		`sanholo_cloud_instances{state="running"}`:           2,
		`sanholo_cloud_instances{state="failed"}`:            1,
		`sanholo_cloud_instances{state="stopped"}`:           0,
		`sanholo_cloud_instances_health{health="unhealthy"}`: 2,
		`sanholo_cloud_releases{state="halted"}`:             1,
		`sanholo_cloud_releases{state="done"}`:               2,
		`sanholo_cloud_releases{state="rolling"}`:            0,
		// Only the halted release's tenants; 0.7.1 is done and left out.
		`sanholo_cloud_rollout_tenants{state="done"}`:                 1,
		`sanholo_cloud_rollout_tenants{state="reverted"}`:             1,
		`sanholo_cloud_rollout_tenants{state="pending"}`:              1,
		`sanholo_cloud_rollout_tenants{state="failed"}`:               0,
		`sanholo_cloud_pending_cleanups{kind="volume"}`:               2,
		`sanholo_cloud_pending_cleanups{kind="secret"}`:               0,
		`sanholo_cloud_pending_cleanup_max_attempts{kind="volume"}`:   7,
		`sanholo_cloud_pending_cleanup_oldest_seconds{kind="secret"}`: 0,
		`sanholo_cloud_mail_outbox{state="queued"}`:                   1,
		`sanholo_cloud_mail_outbox{state="failed"}`:                   2,
		`sanholo_cloud_tenant_deletions{check_state="failed"}`:        1,
		`sanholo_cloud_tenant_deletions{check_state="passed"}`:        2,
		`sanholo_cloud_tenant_deletions{check_state="pending"}`:       0,
	}
	for k, v := range want {
		got, ok := m[k]
		if !ok {
			t.Errorf("missing %s", k)
		} else if got != v {
			t.Errorf("%s = %v, want %v", k, got, v)
		}
	}
	// Ages: the oldest volume cleanup is two hours old, not five minutes;
	// the queued mail ten minutes.
	if age := m[`sanholo_cloud_pending_cleanup_oldest_seconds{kind="volume"}`]; age < 7100 || age > 7300 {
		t.Errorf("oldest volume cleanup age = %v, want about 7200", age)
	}
	if age := m[`sanholo_cloud_mail_outbox_oldest_queued_seconds`]; age < 590 || age > 700 {
		t.Errorf("oldest queued mail age = %v, want about 600", age)
	}
	// No tenant id ever becomes a label.
	for k := range m {
		for _, id := range []string{"a1", "a2", "gone1", "d1", "example.org"} {
			if strings.Contains(k, `"`+id+`"`) || strings.Contains(k, id+"@") {
				t.Errorf("label carries an identifier: %s", k)
			}
		}
	}
}

// failingDB answers every query with an error.
type failingDB struct{}

var errDown = errors.New("connection refused")

func (failingDB) Query(context.Context, string, ...any) (pgx.Rows, error) { return nil, errDown }
func (failingDB) QueryRow(context.Context, string, ...any) pgx.Row        { return errRow{} }

type errRow struct{}

func (errRow) Scan(...any) error { return errDown }

// lateFailDB is a real database whose last query (the mail outbox) fails,
// so the families before it have already been written when it does.
type lateFailDB struct{ Querier }

func (l lateFailDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "mail_outbox") {
		return errRow{}
	}
	return l.Querier.QueryRow(ctx, sql, args...)
}

func TestDatabaseDownStillScrapes(t *testing.T) {
	for _, tc := range []struct {
		name string
		db   func(t *testing.T) Querier
	}{
		{"down", func(*testing.T) Querier { return failingDB{} }},
		{"fails part way", func(t *testing.T) Querier { return lateFailDB{dbtest.Migrated(t)} }},
	} {
		t.Run(tc.name, func(t *testing.T) { databaseDown(t, tc.db(t)) })
	}
}

func databaseDown(t *testing.T, db Querier) {
	qm := NewQMProbe(func(context.Context) error { return nil }, nil)
	qm.Probe(context.Background())
	m := scrape(t, &Source{DB: db, QM: qm})
	if v, ok := m["sanholo_cloud_database_up"]; !ok || v != 0 {
		t.Fatalf("database_up = %v (present %v), want 0", v, ok)
	}
	for k := range m {
		if strings.HasPrefix(k, "sanholo_cloud_tenants") {
			t.Fatalf("database family %s written while the database is down", k)
		}
	}
	if m["sanholo_cloud_qm_up"] != 1 {
		t.Fatal("qm metrics missing while the database is down")
	}
}

func TestQMProbe(t *testing.T) {
	at := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name       string
		err        error
		up, sealed float64
	}{
		{"healthy", nil, 1, 0},
		{"sealed", &secrets.Error{Op: "ready", Err: secrets.ErrSealed}, 0, 1},
		{"unreachable", &secrets.Error{Op: "ready", Err: secrets.ErrUnavailable}, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := NewQMProbe(func(context.Context) error { return tc.err }, func() time.Time { return at })
			// Before the first probe nothing is reported.
			if m := scrape(t, &Source{QM: p}); len(m) == 0 {
				t.Fatal("empty scrape")
			} else if _, ok := m["sanholo_cloud_qm_up"]; ok {
				t.Fatal("qm_up reported before any probe")
			}
			p.Probe(context.Background())
			m := scrape(t, &Source{QM: p})
			if m["sanholo_cloud_qm_up"] != tc.up || m["sanholo_cloud_qm_sealed"] != tc.sealed {
				t.Fatalf("up=%v sealed=%v, want %v %v", m["sanholo_cloud_qm_up"], m["sanholo_cloud_qm_sealed"], tc.up, tc.sealed)
			}
			if m["sanholo_cloud_qm_last_probe_timestamp_seconds"] != float64(at.Unix()) {
				t.Fatal("probe time not reported")
			}
		})
	}
}

func TestCountersFromOtherPackages(t *testing.T) {
	m := scrape(t, &Source{
		Push: func() push.Stats {
			return push.Stats{Delivered: 11, Gone: 2, Refused: 3, GaveUp: 4, CapNotices: 5, CapDropped: 6, QueueFull: 7}
		},
		Alerts: func() AlertStats { return AlertStats{Pushed: 8, Deduped: 9, Failed: 10, Truncated: 12, Capped: 13} },
	})
	want := map[string]float64{
		`sanholo_cloud_push_messages_total{result="delivered"}`:        11,
		`sanholo_cloud_push_messages_total{result="gone"}`:             2,
		`sanholo_cloud_push_messages_total{result="refused"}`:          3,
		`sanholo_cloud_push_messages_total{result="gave_up"}`:          4,
		`sanholo_cloud_push_cap_hits_total`:                            5,
		`sanholo_cloud_push_alerts_dropped_total{reason="cap"}`:        6,
		`sanholo_cloud_push_alerts_dropped_total{reason="queue_full"}`: 7,
		`sanholo_cloud_operator_alerts_total{result="pushed"}`:         8,
		`sanholo_cloud_operator_alerts_total{result="deduped"}`:        9,
		`sanholo_cloud_operator_alerts_total{result="failed"}`:         10,
		`sanholo_cloud_operator_alerts_total{result="truncated"}`:      12,
		`sanholo_cloud_operator_alerts_total{result="capped"}`:         13,
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %v, want %v", k, m[k], v)
		}
	}
}

func TestEscape(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{`a"b`, `a\"b`},
		{`a\b`, `a\\b`},
		{"a\nb", `a\nb`},
		{strings.Repeat("x", 65), strings.Repeat("x", 64)},
	}
	for _, tc := range cases {
		if got := escape(tc.in); got != tc.want {
			t.Errorf("escape(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A deletion stuck before the rows were deleted (tenant 'deleting', check
// 'unchecked', not completed) counts once it is over an hour old; a recent
// one, a checked one and a finished one do not.
func TestStalledDeletions(t *testing.T) {
	pool := dbtest.Migrated(t)
	for _, q := range []string{
		`INSERT INTO tenants (id, status) VALUES ('x1','deleting'),('x2','deleting'),('x3','deleting'),('x4','active')`,
		`INSERT INTO tenant_deletions (tenant_key, requested_at, check_state) VALUES
			('x1', now() - interval '2 hours', 'unchecked'),
			('x2', now() - interval '30 minutes', 'unchecked'),
			('x3', now() - interval '2 hours', 'pending'),
			('x4', now() - interval '2 hours', 'unchecked')`,
		`INSERT INTO tenant_deletions (tenant_key, requested_at, check_state, completed_at) VALUES
			('gone', now() - interval '3 hours', 'unchecked', now())`,
	} {
		exec(t, pool, q)
	}
	m := scrape(t, &Source{DB: pool})
	if got := m[`sanholo_cloud_tenant_deletions_stalled`]; got != 1 {
		t.Errorf("stalled = %v, want 1 (only x1)", got)
	}
}
