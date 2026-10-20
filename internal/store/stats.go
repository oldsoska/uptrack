package store

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/oldsoska/uptrack/internal/monitor"
)

// Stats summarises checks in a period. Uptime and latencies are nil when there were no checks,
// so "no data" is not confused with "0% up".
type Stats struct {
	Checks       int64
	Uptime       *float64 // share of successful checks, 0..1
	LatencyP50Ms *float64 // over successful checks only: a timeout says nothing about speed
	LatencyP95Ms *float64
}

type MonitorWithStats struct {
	monitor.Monitor
	Stats Stats
}

// ListWithStats returns every monitor with its stats since `since`, in one query: the LATERAL
// subquery runs per monitor and reads only that monitor's slice of the (monitor_id, checked_at) key.
func (s *Store) ListWithStats(ctx context.Context, since time.Time) ([]MonitorWithStats, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+prefixed("m.", monitorColumns)+`, st.checks, st.uptime, st.p50, st.p95
		FROM monitors m
		LEFT JOIN LATERAL (
			SELECT count(*)                                                                   AS checks,
			       avg(r.ok::int)::float8                                                     AS uptime,
			       percentile_cont(0.5)  WITHIN GROUP (ORDER BY r.latency_ms) FILTER (WHERE r.ok) AS p50,
			       percentile_cont(0.95) WITHIN GROUP (ORDER BY r.latency_ms) FILTER (WHERE r.ok) AS p95
			FROM check_results r
			WHERE r.monitor_id = m.id AND r.checked_at >= $1
		) st ON true
		ORDER BY m.name, m.id`, since)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (MonitorWithStats, error) {
		var out MonitorWithStats
		var interval, timeout int
		var status string
		err := r.Scan(&out.ID, &out.Name, &out.URL, &out.Method, &interval, &timeout,
			&out.ExpectedStatusMin, &out.ExpectedStatusMax, &out.FailureThreshold, &out.Paused,
			&status, &out.ConsecutiveFailures, &out.LastCheckedAt, &out.CreatedAt,
			&out.Stats.Checks, &out.Stats.Uptime, &out.Stats.LatencyP50Ms, &out.Stats.LatencyP95Ms)
		out.Interval = time.Duration(interval) * time.Second
		out.Timeout = time.Duration(timeout) * time.Second
		out.Status = monitor.Status(status)
		return out, err
	})
}

func prefixed(prefix, columns string) string {
	parts := strings.Split(columns, ",")
	for i, p := range parts {
		parts[i] = prefix + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}

// DayUptime is one bar on the status page.
type DayUptime struct {
	Day    time.Time
	Checks int64
	Uptime *float64
}

// DailyUptime returns, per active monitor, one entry for each of the last `days` UTC days ending
// with the day of `now`, including days without checks (generate_series fills the gaps).
func (s *Store) DailyUptime(ctx context.Context, days int, now time.Time) (map[int64][]DayUptime, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT m.id, d.day, count(r.ok), avg(r.ok::int)::float8
		FROM monitors m
		CROSS JOIN generate_series(
			date_trunc('day', $1::timestamptz AT TIME ZONE 'UTC') - ($2::int - 1) * INTERVAL '1 day',
			date_trunc('day', $1::timestamptz AT TIME ZONE 'UTC'),
			INTERVAL '1 day') AS d(day)
		LEFT JOIN check_results r
		       ON r.monitor_id = m.id
		      AND r.checked_at >= d.day AT TIME ZONE 'UTC'
		      AND r.checked_at <  (d.day + INTERVAL '1 day') AT TIME ZONE 'UTC'
		WHERE NOT m.paused
		GROUP BY m.id, d.day
		ORDER BY m.id, d.day`, now, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]DayUptime{}
	for rows.Next() {
		var id int64
		var d DayUptime
		if err := rows.Scan(&id, &d.Day, &d.Checks, &d.Uptime); err != nil {
			return nil, err
		}
		out[id] = append(out[id], d)
	}
	return out, rows.Err()
}

func (s *Store) RecentResults(ctx context.Context, monitorID int64, limit int) ([]monitor.Result, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT monitor_id, checked_at, ok, coalesce(status_code, 0), latency_ms, coalesce(error, '')
		FROM check_results WHERE monitor_id = $1 ORDER BY checked_at DESC LIMIT $2`, monitorID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (monitor.Result, error) {
		var res monitor.Result
		var latencyMs int64
		err := r.Scan(&res.MonitorID, &res.CheckedAt, &res.OK, &res.StatusCode, &latencyMs, &res.Error)
		res.Latency = time.Duration(latencyMs) * time.Millisecond
		return res, err
	})
}

type IncidentView struct {
	monitor.Incident
	MonitorName string
}

func (s *Store) ListIncidents(ctx context.Context, openOnly bool, limit int) ([]IncidentView, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT i.id, i.monitor_id, i.opened_at, i.resolved_at, i.cause, m.name
		FROM incidents i JOIN monitors m ON m.id = i.monitor_id
		WHERE NOT $1 OR i.resolved_at IS NULL
		ORDER BY i.opened_at DESC LIMIT $2`, openOnly, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (IncidentView, error) {
		var v IncidentView
		err := r.Scan(&v.ID, &v.MonitorID, &v.OpenedAt, &v.ResolvedAt, &v.Cause, &v.MonitorName)
		return v, err
	})
}

// MonitorStats summarises one monitor's checks since `since`.
func (s *Store) MonitorStats(ctx context.Context, monitorID int64, since time.Time) (Stats, error) {
	var st Stats
	err := s.pool.QueryRow(ctx, `
		SELECT count(*),
		       avg(ok::int)::float8,
		       percentile_cont(0.5)  WITHIN GROUP (ORDER BY latency_ms) FILTER (WHERE ok),
		       percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms) FILTER (WHERE ok)
		FROM check_results WHERE monitor_id = $1 AND checked_at >= $2`, monitorID, since).
		Scan(&st.Checks, &st.Uptime, &st.LatencyP50Ms, &st.LatencyP95Ms)
	return st, err
}
