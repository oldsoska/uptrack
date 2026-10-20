package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/oldsoska/uptrack/internal/monitor"
)

var testStore *Store

// TestMain starts one PostgreSQL container for the package and applies the migrations.
// Each test creates its own monitors, so tests do not depend on each other.
func TestMain(m *testing.M) {
	ctx := context.Background()
	pg, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("uptrack"), postgres.WithUsername("uptrack"), postgres.WithPassword("uptrack"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).WithStartupTimeout(60*time.Second)))
	if err != nil {
		panic(err)
	}
	url, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		panic(err)
	}
	testStore, err = Open(ctx, url)
	if err != nil {
		panic(err)
	}
	if err := testStore.Migrate(ctx); err != nil {
		panic(err)
	}
	code := m.Run()
	testStore.Close()
	_ = pg.Terminate(ctx)
	os.Exit(code)
}

func newMonitor(t *testing.T, name string) monitor.Monitor {
	t.Helper()
	m, err := testStore.CreateMonitor(context.Background(), monitor.Monitor{
		Name: name, URL: "https://example.com/" + name, Method: "GET",
		Interval: time.Minute, Timeout: 10 * time.Second,
		ExpectedStatusMin: 200, ExpectedStatusMax: 399, FailureThreshold: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestMigrateIsIdempotent(t *testing.T) {
	if err := testStore.Migrate(context.Background()); err != nil {
		t.Fatalf("second run: %v", err)
	}
}

func TestCreateAndUpdateMonitor(t *testing.T) {
	ctx := context.Background()
	m := newMonitor(t, "create-update")
	if m.ID == 0 || m.Status != monitor.StatusUnknown || m.Interval != time.Minute {
		t.Fatalf("created %+v", m)
	}

	if _, err := testStore.RecordCheck(ctx, monitor.Result{MonitorID: m.ID, CheckedAt: time.Now(), OK: true},
		monitor.State{Status: monitor.StatusUp}, monitor.NoChange); err != nil {
		t.Fatal(err)
	}
	m.Name = "renamed"
	renamed, err := testStore.UpdateMonitor(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Name != "renamed" || renamed.Status != monitor.StatusUp {
		t.Errorf("rename should keep status: %+v", renamed)
	}

	renamed.URL = "https://example.com/elsewhere"
	moved, err := testStore.UpdateMonitor(ctx, renamed)
	if err != nil {
		t.Fatal(err)
	}
	if moved.Status != monitor.StatusUnknown {
		t.Errorf("new URL should reset status, got %s", moved.Status)
	}
}

func TestDatabaseRejectsInvalidMonitor(t *testing.T) {
	_, err := testStore.CreateMonitor(context.Background(), monitor.Monitor{
		Name: "bad", URL: "https://example.com", Method: "GET",
		Interval: 30 * time.Second, Timeout: 30 * time.Second, // timeout must be below interval
		ExpectedStatusMin: 200, ExpectedStatusMax: 399, FailureThreshold: 2,
	})
	if err == nil {
		t.Fatal("CHECK constraint should reject timeout >= interval")
	}
}

func TestIncidentLifecycle(t *testing.T) {
	ctx := context.Background()
	m := newMonitor(t, "lifecycle")
	t0 := time.Now().UTC().Truncate(time.Millisecond)
	fail := monitor.Result{MonitorID: m.ID, CheckedAt: t0, Error: "connection refused", Latency: 3 * time.Millisecond}

	opened, err := testStore.RecordCheck(ctx, fail, monitor.State{Status: monitor.StatusDown, ConsecutiveFailures: 2}, monitor.WentDown)
	if err != nil {
		t.Fatal(err)
	}
	if opened == nil || opened.Cause != "connection refused" || opened.ResolvedAt != nil {
		t.Fatalf("opened %+v", opened)
	}

	// A second "went down" (e.g. after a restart lost the in-memory state) must reuse the open incident
	again, err := testStore.RecordCheck(ctx, monitor.Result{MonitorID: m.ID, CheckedAt: t0.Add(time.Minute), Error: "timeout"},
		monitor.State{Status: monitor.StatusDown, ConsecutiveFailures: 3}, monitor.WentDown)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != opened.ID {
		t.Errorf("second open incident created: %d vs %d", again.ID, opened.ID)
	}

	resolved, err := testStore.RecordCheck(ctx, monitor.Result{MonitorID: m.ID, CheckedAt: t0.Add(5 * time.Minute), OK: true, StatusCode: 200},
		monitor.State{Status: monitor.StatusUp}, monitor.Recovered)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ID != opened.ID || resolved.ResolvedAt == nil || resolved.Duration(time.Now()) != 5*time.Minute {
		t.Fatalf("resolved %+v", resolved)
	}

	stored, err := testStore.GetMonitor(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != monitor.StatusUp || stored.ConsecutiveFailures != 0 || stored.LastCheckedAt == nil {
		t.Errorf("monitor state not saved: %+v", stored)
	}
	open, err := testStore.ListIncidents(ctx, true, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range open {
		if i.MonitorID == m.ID {
			t.Errorf("incident still open: %+v", i)
		}
	}
}

func TestRecordCheckForDeletedMonitor(t *testing.T) {
	ctx := context.Background()
	m := newMonitor(t, "deleted")
	if err := testStore.DeleteMonitor(ctx, m.ID); err != nil {
		t.Fatal(err)
	}

	_, err := testStore.RecordCheck(ctx, monitor.Result{MonitorID: m.ID, CheckedAt: time.Now(), OK: true},
		monitor.State{Status: monitor.StatusUp}, monitor.NoChange)

	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestStatsAndDailyUptime(t *testing.T) {
	ctx := context.Background()
	m := newMonitor(t, "stats")
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	// 4 checks today: 3 OK with 100/200/300 ms, 1 failure with a 10 s timeout that must not skew latency
	for i, r := range []monitor.Result{
		{OK: true, Latency: 100 * time.Millisecond},
		{OK: true, Latency: 200 * time.Millisecond},
		{OK: true, Latency: 300 * time.Millisecond},
		{OK: false, Latency: 10 * time.Second, Error: "timeout"},
	} {
		r.MonitorID, r.CheckedAt = m.ID, now.Add(-time.Duration(i+1)*time.Minute)
		if _, err := testStore.RecordCheck(ctx, r, monitor.State{Status: monitor.StatusUp}, monitor.NoChange); err != nil {
			t.Fatal(err)
		}
	}

	all, err := testStore.ListWithStats(ctx, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var st *Stats
	for i := range all {
		if all[i].ID == m.ID {
			st = &all[i].Stats
		}
	}
	if st == nil || st.Checks != 4 || *st.Uptime != 0.75 || *st.LatencyP50Ms != 200 || *st.LatencyP95Ms != 290 {
		t.Fatalf("stats = %+v (uptime %v, p50 %v, p95 %v)", st, deref(st.Uptime), deref(st.LatencyP50Ms), deref(st.LatencyP95Ms))
	}

	daily, err := testStore.DailyUptime(ctx, 3, now)
	if err != nil {
		t.Fatal(err)
	}
	days := daily[m.ID]
	if len(days) != 3 {
		t.Fatalf("days = %+v", days)
	}
	if days[0].Checks != 0 || days[0].Uptime != nil || days[2].Checks != 4 || *days[2].Uptime != 0.75 {
		t.Errorf("days = %+v", days)
	}
	if !days[2].Day.Equal(time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("last day = %s", days[2].Day)
	}
}

func TestRetentionKeepsRecentResults(t *testing.T) {
	ctx := context.Background()
	m := newMonitor(t, "retention")
	old, recent := time.Now().Add(-40*24*time.Hour), time.Now().Add(-time.Hour)
	for _, at := range []time.Time{old, recent} {
		if _, err := testStore.RecordCheck(ctx, monitor.Result{MonitorID: m.ID, CheckedAt: at, OK: true},
			monitor.State{Status: monitor.StatusUp}, monitor.NoChange); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := testStore.DeleteResultsBefore(ctx, time.Now().Add(-30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	left, err := testStore.RecentResults(ctx, m.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || !left[0].CheckedAt.Equal(recent.Truncate(time.Microsecond)) {
		t.Errorf("left = %+v", left)
	}
}

func deref(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}
