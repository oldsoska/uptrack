package scheduler

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oldsoska/uptrack/internal/checker"
	"github.com/oldsoska/uptrack/internal/monitor"
	"github.com/oldsoska/uptrack/internal/notify"
)

// fakeStore records what the scheduler writes, and opens/resolves incidents like the real store.
type fakeStore struct {
	mu        sync.Mutex
	monitors  []monitor.Monitor
	results   []monitor.Result
	incidents []monitor.Incident
}

func (f *fakeStore) ListMonitors(context.Context) ([]monitor.Monitor, error) {
	return f.monitors, nil
}

func (f *fakeStore) RecordCheck(_ context.Context, r monitor.Result, _ monitor.State, tr monitor.Transition) (*monitor.Incident, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results = append(f.results, r)
	switch tr {
	case monitor.WentDown:
		f.incidents = append(f.incidents, monitor.Incident{ID: int64(len(f.incidents) + 1), MonitorID: r.MonitorID, OpenedAt: r.CheckedAt, Cause: r.Error})
		inc := f.incidents[len(f.incidents)-1]
		return &inc, nil
	case monitor.Recovered:
		last := &f.incidents[len(f.incidents)-1]
		at := r.CheckedAt
		last.ResolvedAt = &at
		inc := *last
		return &inc, nil
	}
	return nil, nil
}

func (f *fakeStore) resultCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.results)
}

type recordingNotifier struct {
	mu     sync.Mutex
	events []notify.Event
}

func (r *recordingNotifier) Notify(_ context.Context, e notify.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	return nil
}

func (r *recordingNotifier) kinds() []notify.Kind {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []notify.Kind
	for _, e := range r.events {
		out = append(out, e.Kind)
	}
	return out
}

type noopRecorder struct{}

func (noopRecorder) ObserveCheck(monitor.Monitor, monitor.Result, monitor.State) {}
func (noopRecorder) Forget(int64)                                                {}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func testMonitor(url string) monitor.Monitor {
	return monitor.Monitor{
		ID: 1, Name: "api", URL: url, Method: "GET", Interval: 20 * time.Millisecond, Timeout: 500 * time.Millisecond,
		ExpectedStatusMin: 200, ExpectedStatusMax: 299, FailureThreshold: 2, Status: monitor.StatusUnknown,
	}
}

func TestOutageAndRecoveryAreAlertedOnce(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !healthy.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()

	store := &fakeStore{monitors: []monitor.Monitor{testMonitor(srv.URL)}}
	notifier := &recordingNotifier{}
	s := New(store, checker.New(false), notifier, noopRecorder{}, quietLogger(), Options{MaxConcurrent: 2})
	ctx, cancel := context.WithCancel(context.Background())
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}

	eventually(t, "first checks", func() bool { return store.resultCount() >= 2 })
	healthy.Store(false)
	eventually(t, "down alert", func() bool { return len(notifier.kinds()) == 1 })
	before := store.resultCount()
	eventually(t, "more failed checks", func() bool { return store.resultCount() >= before+3 })
	healthy.Store(true)
	eventually(t, "recovery alert", func() bool { return len(notifier.kinds()) == 2 })

	cancel()
	s.Wait()

	got := notifier.kinds()
	if len(got) != 2 || got[0] != notify.KindDown || got[1] != notify.KindRecovered {
		t.Fatalf("alerts = %v, want exactly [down recovered] despite repeated failures", got)
	}
	if cause := notifier.events[0].Incident.Cause; cause != "unexpected status 503 (expected 200-299)" {
		t.Errorf("cause = %q", cause)
	}
}

func TestPausedMonitorIsNotChecked(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()

	m := testMonitor(srv.URL)
	m.Paused = true
	s := New(&fakeStore{monitors: []monitor.Monitor{m}}, checker.New(false), &recordingNotifier{}, noopRecorder{}, quietLogger(), Options{})
	ctx, cancel := context.WithCancel(context.Background())
	_ = s.Start(ctx)
	time.Sleep(100 * time.Millisecond)
	cancel()
	s.Wait()

	if hits.Load() != 0 {
		t.Errorf("paused monitor was checked %d times", hits.Load())
	}
}

func TestRemoveStopsChecks(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()

	s := New(&fakeStore{monitors: []monitor.Monitor{testMonitor(srv.URL)}}, checker.New(false), &recordingNotifier{}, noopRecorder{}, quietLogger(), Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = s.Start(ctx)
	eventually(t, "a check", func() bool { return hits.Load() > 0 })

	s.Remove(1)
	time.Sleep(50 * time.Millisecond) // let an in-flight check finish
	after := hits.Load()
	time.Sleep(100 * time.Millisecond)

	if hits.Load() != after {
		t.Errorf("checks continued after Remove: %d -> %d", after, hits.Load())
	}
}

func TestConcurrencyIsCapped(t *testing.T) {
	var inFlight, peak atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-release
		inFlight.Add(-1)
	}))
	defer srv.Close()

	var monitors []monitor.Monitor
	for i := 1; i <= 10; i++ {
		m := testMonitor(srv.URL)
		m.ID, m.Interval, m.Timeout = int64(i), time.Second, 900*time.Millisecond
		monitors = append(monitors, m)
	}
	s := New(&fakeStore{monitors: monitors}, checker.New(false), &recordingNotifier{}, noopRecorder{}, quietLogger(), Options{MaxConcurrent: 3})
	ctx, cancel := context.WithCancel(context.Background())
	_ = s.Start(ctx)
	eventually(t, "requests in flight", func() bool { return inFlight.Load() == 3 })
	time.Sleep(50 * time.Millisecond)
	close(release)
	cancel()
	s.Wait()

	if peak.Load() != 3 {
		t.Errorf("peak concurrent checks = %d, want 3", peak.Load())
	}
}
