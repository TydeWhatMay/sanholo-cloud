// Package metrics writes the control plane's Prometheus text exposition
// (US-023). Every value comes from the database at scrape time or from
// counters already kept in the process; nothing here changes state.
//
// The text format is written by hand rather than with
// prometheus/client_golang: the values are a few gauges read with one query
// each per scrape plus counters other packages already keep, which is what
// the client repository does too, and the library would add half a dozen
// modules (client_model, common, procfs, protobuf, ...) for no gain.
//
// No label ever carries a tenant id, an email, a token or a remote system's
// text: every label value is one of a fixed set (a CHECK-constrained enum).
package metrics

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/lhaig/sanholo-cloud/internal/push"
)

// Querier is the part of *pgxpool.Pool the collector needs.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// AlertStats are the operator alert intake's counters.
type AlertStats struct {
	Pushed, Deduped, Failed, Truncated, Capped uint64
}

// Source is everything one scrape reads. Any nil field leaves its metrics
// out.
type Source struct {
	DB Querier
	// Push returns the Web Push delivery counters.
	Push func() push.Stats
	// QM is the qm health probe.
	QM *QMProbe
	// Alerts returns the operator alert intake counters.
	Alerts func() AlertStats
	// Version is the control plane's build version.
	Version string
}

// The enum values every gauge is zero-filled with, so a state with no rows
// still has a series (an alert on "> 0" then sees 0, not nothing). They
// mirror the CHECK constraints in internal/db/migrations.
var (
	tenantStatuses = []string{"pending", "active", "suspended", "deleting"}
	instanceStates = []string{"pending", "running", "failed", "stopped"}
	instanceHealth = []string{"unknown", "starting", "healthy", "unhealthy"}
	releaseStates  = []string{"available", "rolling", "halted", "done"}
	rolloutStates  = []string{"pending", "updating", "revert_pending", "reverting", "done", "skipped", "reverted", "failed"}
	cleanupKinds   = []string{"secret", "volume"}
	deletionChecks = []string{"unchecked", "pending", "passed", "failed"}
)

const (
	queryTimeout     = 5 * time.Second
	maxLabelValueLen = 64
)

// ContentType is the exposition's media type.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// Write writes one scrape to w. A database failure does not fail the
// scrape: sanholo_cloud_database_up is 0 and the database families are left
// out, so qm and push metrics still arrive while the database is down.
func (s *Source) Write(ctx context.Context, w io.Writer) error {
	var b bytes.Buffer
	e := &enc{w: &b}

	e.family("sanholo_cloud_build_info", "gauge", "Always 1; the version label is the control plane's build.")
	e.sample("sanholo_cloud_build_info", 1, "version", s.Version)

	if s.DB != nil {
		var db bytes.Buffer
		err := s.writeDB(ctx, &enc{w: &db})
		e.family("sanholo_cloud_database_up", "gauge", "1 when every database query of this scrape succeeded.")
		if err != nil {
			e.sample("sanholo_cloud_database_up", 0)
		} else {
			e.sample("sanholo_cloud_database_up", 1)
			b.Write(db.Bytes())
		}
	}
	if s.QM != nil {
		s.QM.write(e)
	}
	if s.Push != nil {
		st := s.Push()
		e.family("sanholo_cloud_push_messages_total", "counter", "Web Push messages by outcome: delivered, gone (subscription removed), refused, gave_up (after retries).")
		e.sample("sanholo_cloud_push_messages_total", float64(st.Delivered), "result", "delivered")
		e.sample("sanholo_cloud_push_messages_total", float64(st.Gone), "result", "gone")
		e.sample("sanholo_cloud_push_messages_total", float64(st.Refused), "result", "refused")
		e.sample("sanholo_cloud_push_messages_total", float64(st.GaveUp), "result", "gave_up")
		e.family("sanholo_cloud_push_alerts_dropped_total", "counter", "Alerts not pushed: over the hourly cap, or the delivery queue was full.")
		e.sample("sanholo_cloud_push_alerts_dropped_total", float64(st.CapDropped), "reason", "cap")
		e.sample("sanholo_cloud_push_alerts_dropped_total", float64(st.QueueFull), "reason", "queue_full")
		e.family("sanholo_cloud_push_cap_hits_total", "counter", "Times a tenant (or the operator) hit the hourly push cap.")
		e.sample("sanholo_cloud_push_cap_hits_total", float64(st.CapNotices))
	}
	if s.Alerts != nil {
		st := s.Alerts()
		e.family("sanholo_cloud_operator_alerts_total", "counter", "Alertmanager notifications by outcome: pushed, deduped, failed (queue full), truncated (over the per-request limit), capped (over the operator's hourly budget; pushed on a later send).")
		e.sample("sanholo_cloud_operator_alerts_total", float64(st.Pushed), "result", "pushed")
		e.sample("sanholo_cloud_operator_alerts_total", float64(st.Deduped), "result", "deduped")
		e.sample("sanholo_cloud_operator_alerts_total", float64(st.Failed), "result", "failed")
		e.sample("sanholo_cloud_operator_alerts_total", float64(st.Truncated), "result", "truncated")
		e.sample("sanholo_cloud_operator_alerts_total", float64(st.Capped), "result", "capped")
	}

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	e.family("go_goroutines", "gauge", "Number of goroutines.")
	e.sample("go_goroutines", float64(runtime.NumGoroutine()))
	e.family("go_memstats_heap_inuse_bytes", "gauge", "Bytes in in-use heap spans.")
	e.sample("go_memstats_heap_inuse_bytes", float64(mem.HeapInuse))
	e.family("go_memstats_sys_bytes", "gauge", "Bytes obtained from the system.")
	e.sample("go_memstats_sys_bytes", float64(mem.Sys))

	_, err := w.Write(b.Bytes())
	return err
}

// writeDB writes every database family, or returns the first error.
func (s *Source) writeDB(ctx context.Context, e *enc) error {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	counts := func(name, help, label, sql string, values []string) error {
		m, err := countBy(ctx, s.DB, sql)
		if err != nil {
			return err
		}
		e.family(name, "gauge", help)
		for _, v := range values {
			e.sample(name, float64(m[v]), label, v)
		}
		return nil
	}
	for _, q := range []struct {
		name, help, label, sql string
		values                 []string
	}{
		{"sanholo_cloud_tenants", "Tenants by status.", "status",
			`SELECT status, count(*) FROM tenants GROUP BY status`, tenantStatuses},
		{"sanholo_cloud_instances", "Tenant instances (Nomad jobs) by state.", "state",
			`SELECT state, count(*) FROM tenant_instances GROUP BY state`, instanceStates},
		{"sanholo_cloud_instances_health", "Tenant instances by the last health the control plane read.", "health",
			`SELECT last_health, count(*) FROM tenant_instances GROUP BY last_health`, instanceHealth},
		{"sanholo_cloud_releases", "Client releases by rollout state.", "state",
			`SELECT state, count(*) FROM releases GROUP BY state`, releaseStates},
		{"sanholo_cloud_rollout_tenants", "Tenants of the release being rolled out (rolling or halted), by state.", "state",
			`SELECT rt.state, count(*) FROM rollout_tenants rt JOIN releases r ON r.version = rt.release_version
			 WHERE r.state IN ('rolling', 'halted') GROUP BY rt.state`, rolloutStates},
		{"sanholo_cloud_tenant_deletions", "Account deletions by the state of their completeness check.", "check_state",
			`SELECT check_state, count(*) FROM tenant_deletions GROUP BY check_state`, deletionChecks},
	} {
		if err := counts(q.name, q.help, q.label, q.sql, q.values); err != nil {
			return err
		}
	}

	// A deletion that stopped before the tenant's rows were deleted (the bot
	// could not be stopped, the deletion log could not be written): the
	// tenant is 'deleting', the deletion never reached a check. The resume
	// pass retries it after a few minutes, so an hour means it is stuck.
	var stalled int64
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM tenant_deletions d JOIN tenants t ON t.id = d.tenant_key
		WHERE d.completed_at IS NULL AND d.check_state = 'unchecked' AND t.status = 'deleting'
		  AND d.requested_at < now() - interval '1 hour'`).Scan(&stalled); err != nil {
		return err
	}
	e.family("sanholo_cloud_tenant_deletions_stalled", "gauge", "Account deletions that stopped before the tenant's rows were deleted, requested over an hour ago.")
	e.sample("sanholo_cloud_tenant_deletions_stalled", float64(stalled))

	type cleanup struct {
		n        int64
		oldest   float64
		attempts int64
	}
	cl := map[string]cleanup{}
	rows, err := s.DB.Query(ctx, `SELECT kind, count(*), COALESCE(EXTRACT(EPOCH FROM now() - min(requested_at)), 0)::float8,
		COALESCE(max(attempts), 0)::bigint FROM pending_cleanups GROUP BY kind`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var k string
		var c cleanup
		if err := rows.Scan(&k, &c.n, &c.oldest, &c.attempts); err != nil {
			rows.Close()
			return err
		}
		cl[k] = c
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	e.family("sanholo_cloud_pending_cleanups", "gauge", "Deletions (qm secret, Nomad volume) waiting to be retried, by kind.")
	for _, k := range cleanupKinds {
		e.sample("sanholo_cloud_pending_cleanups", float64(cl[k].n), "kind", k)
	}
	e.family("sanholo_cloud_pending_cleanup_oldest_seconds", "gauge", "Age of the oldest waiting cleanup of each kind; 0 when none waits.")
	for _, k := range cleanupKinds {
		e.sample("sanholo_cloud_pending_cleanup_oldest_seconds", cl[k].oldest, "kind", k)
	}
	e.family("sanholo_cloud_pending_cleanup_max_attempts", "gauge", "Most attempts any waiting cleanup of each kind has had.")
	for _, k := range cleanupKinds {
		e.sample("sanholo_cloud_pending_cleanup_max_attempts", float64(cl[k].attempts), "kind", k)
	}

	var queued, failed int64
	var oldest float64
	if err := s.DB.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE sent_at IS NULL AND failed_at IS NULL),
		count(*) FILTER (WHERE failed_at IS NOT NULL),
		COALESCE(EXTRACT(EPOCH FROM now() - min(created_at) FILTER (WHERE sent_at IS NULL AND failed_at IS NULL)), 0)::float8
		FROM mail_outbox`).Scan(&queued, &failed, &oldest); err != nil {
		return err
	}
	e.family("sanholo_cloud_mail_outbox", "gauge", "Mail outbox rows: queued (not yet sent) and failed (given up).")
	e.sample("sanholo_cloud_mail_outbox", float64(queued), "state", "queued")
	e.sample("sanholo_cloud_mail_outbox", float64(failed), "state", "failed")
	e.family("sanholo_cloud_mail_outbox_oldest_queued_seconds", "gauge", "Age of the oldest queued mail; 0 when none is queued.")
	e.sample("sanholo_cloud_mail_outbox_oldest_queued_seconds", oldest)
	return nil
}

// countBy runs a two-column (text, count) query into a map.
func countBy(ctx context.Context, db Querier, sql string) (map[string]int64, error) {
	rows, err := db.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]int64{}
	for rows.Next() {
		var k string
		var n int64
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		m[k] = n
	}
	return m, rows.Err()
}

// enc writes the text format. HELP and TYPE once per family, then samples.
type enc struct{ w io.Writer }

func (e *enc) family(name, typ, help string) {
	fmt.Fprintf(e.w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

// sample writes name{k="v",...} value. labels are key, value pairs.
func (e *enc) sample(name string, v float64, labels ...string) {
	var b strings.Builder
	b.WriteString(name)
	if len(labels) > 0 {
		b.WriteByte('{')
		for i := 0; i+1 < len(labels); i += 2 {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(labels[i])
			b.WriteString(`="`)
			b.WriteString(escape(labels[i+1]))
			b.WriteByte('"')
		}
		b.WriteByte('}')
	}
	b.WriteByte(' ')
	b.WriteString(formatValue(v))
	b.WriteByte('\n')
	_, _ = io.WriteString(e.w, b.String())
}

// escape applies the text format's label value escaping (backslash, double
// quote, line feed) and bounds the length.
func escape(v string) string {
	if len(v) > maxLabelValueLen {
		v = v[:maxLabelValueLen]
	}
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

func formatValue(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}
