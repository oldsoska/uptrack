// Package store keeps monitors, check results and incidents in PostgreSQL.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsoska/uptrack/internal/monitor"
)

var ErrNotFound = errors.New("not found")

type Store struct {
	pool *pgxpool.Pool
}

func Open(ctx context.Context, databaseURL string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = 10
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close()                         { s.pool.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

const monitorColumns = `id, name, url, method, interval_seconds, timeout_seconds,
	expected_status_min, expected_status_max, failure_threshold, paused,
	status, consecutive_failures, last_checked_at, created_at`

func scanMonitor(row pgx.Row) (monitor.Monitor, error) {
	var m monitor.Monitor
	var interval, timeout int
	var status string
	err := row.Scan(&m.ID, &m.Name, &m.URL, &m.Method, &interval, &timeout,
		&m.ExpectedStatusMin, &m.ExpectedStatusMax, &m.FailureThreshold, &m.Paused,
		&status, &m.ConsecutiveFailures, &m.LastCheckedAt, &m.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, ErrNotFound
	}
	m.Interval = time.Duration(interval) * time.Second
	m.Timeout = time.Duration(timeout) * time.Second
	m.Status = monitor.Status(status)
	return m, err
}

func (s *Store) ListMonitors(ctx context.Context) ([]monitor.Monitor, error) {
	rows, err := s.pool.Query(ctx, "SELECT "+monitorColumns+" FROM monitors ORDER BY name, id")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (monitor.Monitor, error) { return scanMonitor(r) })
}

func (s *Store) GetMonitor(ctx context.Context, id int64) (monitor.Monitor, error) {
	return scanMonitor(s.pool.QueryRow(ctx, "SELECT "+monitorColumns+" FROM monitors WHERE id = $1", id))
}

func (s *Store) CreateMonitor(ctx context.Context, m monitor.Monitor) (monitor.Monitor, error) {
	return scanMonitor(s.pool.QueryRow(ctx, `
		INSERT INTO monitors (name, url, method, interval_seconds, timeout_seconds,
		                      expected_status_min, expected_status_max, failure_threshold, paused)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING `+monitorColumns,
		m.Name, m.URL, m.Method, int(m.Interval.Seconds()), int(m.Timeout.Seconds()),
		m.ExpectedStatusMin, m.ExpectedStatusMax, m.FailureThreshold, m.Paused))
}

// UpdateMonitor replaces the settings. Status survives an edit unless the URL changes:
// a different URL is a different service, so its state starts over as UNKNOWN.
func (s *Store) UpdateMonitor(ctx context.Context, m monitor.Monitor) (monitor.Monitor, error) {
	return scanMonitor(s.pool.QueryRow(ctx, `
		UPDATE monitors SET
			name = $2, url = $3, method = $4, interval_seconds = $5, timeout_seconds = $6,
			expected_status_min = $7, expected_status_max = $8, failure_threshold = $9, paused = $10,
			status = CASE WHEN url = $3 THEN status ELSE 'UNKNOWN' END,
			consecutive_failures = CASE WHEN url = $3 THEN consecutive_failures ELSE 0 END
		WHERE id = $1
		RETURNING `+monitorColumns,
		m.ID, m.Name, m.URL, m.Method, int(m.Interval.Seconds()), int(m.Timeout.Seconds()),
		m.ExpectedStatusMin, m.ExpectedStatusMax, m.FailureThreshold, m.Paused))
}

func (s *Store) DeleteMonitor(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, "DELETE FROM monitors WHERE id = $1", id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RecordCheck stores a result, the monitor's new state and, on a transition, opens or resolves the
// incident — all in one transaction, so status, history and incidents never disagree. It returns
// the incident affected by the transition, or nil.
func (s *Store) RecordCheck(ctx context.Context, r monitor.Result, st monitor.State, tr monitor.Transition) (*monitor.Incident, error) {
	var incident *monitor.Incident
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE monitors SET status = $2, consecutive_failures = $3, last_checked_at = $4 WHERE id = $1`,
			r.MonitorID, string(st.Status), st.ConsecutiveFailures, r.CheckedAt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound // deleted while the check was running
		}
		var statusCode *int
		if r.StatusCode != 0 {
			statusCode = &r.StatusCode
		}
		var errText *string
		if r.Error != "" {
			errText = &r.Error
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO check_results (monitor_id, checked_at, ok, status_code, latency_ms, error)
			VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT DO NOTHING`,
			r.MonitorID, r.CheckedAt, r.OK, statusCode, r.Latency.Milliseconds(), errText); err != nil {
			return err
		}

		switch tr {
		case monitor.WentDown:
			incident, err = openIncident(ctx, tx, r)
		case monitor.Recovered:
			incident, err = resolveIncident(ctx, tx, r)
		}
		return err
	})
	return incident, err
}

func openIncident(ctx context.Context, tx pgx.Tx, r monitor.Result) (*monitor.Incident, error) {
	// The partial unique index allows one open incident per monitor; if one is somehow open already
	// (e.g. state lost in a crash), keep it instead of failing.
	inc, err := scanIncident(tx.QueryRow(ctx, `
		INSERT INTO incidents (monitor_id, opened_at, cause) VALUES ($1, $2, $3)
		ON CONFLICT (monitor_id) WHERE resolved_at IS NULL DO NOTHING
		RETURNING id, monitor_id, opened_at, resolved_at, cause`, r.MonitorID, r.CheckedAt, r.Error))
	if errors.Is(err, ErrNotFound) {
		return scanIncident(tx.QueryRow(ctx, `
			SELECT id, monitor_id, opened_at, resolved_at, cause FROM incidents
			WHERE monitor_id = $1 AND resolved_at IS NULL`, r.MonitorID))
	}
	return inc, err
}

func resolveIncident(ctx context.Context, tx pgx.Tx, r monitor.Result) (*monitor.Incident, error) {
	inc, err := scanIncident(tx.QueryRow(ctx, `
		UPDATE incidents SET resolved_at = $2 WHERE monitor_id = $1 AND resolved_at IS NULL
		RETURNING id, monitor_id, opened_at, resolved_at, cause`, r.MonitorID, r.CheckedAt))
	if errors.Is(err, ErrNotFound) {
		return nil, nil // nothing was open, e.g. history was purged; the recovery itself is still valid
	}
	return inc, err
}

func scanIncident(row pgx.Row) (*monitor.Incident, error) {
	var i monitor.Incident
	err := row.Scan(&i.ID, &i.MonitorID, &i.OpenedAt, &i.ResolvedAt, &i.Cause)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &i, nil
}

// DeleteResultsBefore removes old check history; incidents are kept.
func (s *Store) DeleteResultsBefore(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, "DELETE FROM check_results WHERE checked_at < $1", before)
	return tag.RowsAffected(), err
}
