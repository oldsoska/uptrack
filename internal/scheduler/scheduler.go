// Package scheduler runs every active monitor on its interval and turns results into alerts.
//
// Each monitor gets its own goroutine with a ticker; a semaphore caps how many HTTP checks run
// at once, so a thousand monitors firing together cannot open a thousand connections. A monitor's
// in-memory state is touched only by its own goroutine, which is why no lock guards it.
package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/oldsoska/uptrack/internal/monitor"
	"github.com/oldsoska/uptrack/internal/notify"
)

type Store interface {
	ListMonitors(ctx context.Context) ([]monitor.Monitor, error)
	RecordCheck(ctx context.Context, r monitor.Result, st monitor.State, tr monitor.Transition) (*monitor.Incident, error)
}

type Checker interface {
	Check(ctx context.Context, m monitor.Monitor) monitor.Result
}

// Recorder receives check outcomes for metrics.
type Recorder interface {
	ObserveCheck(m monitor.Monitor, r monitor.Result, st monitor.State)
	Forget(monitorID int64)
}

type Scheduler struct {
	store    Store
	checker  Checker
	notifier notify.Notifier
	recorder Recorder
	log      *slog.Logger
	sem      chan struct{}
	now      func() time.Time
	// maxStartDelay spreads the first checks after a start, so monitors do not all fire at once
	maxStartDelay time.Duration

	mu      sync.Mutex
	ctx     context.Context
	jobs    map[int64]*job
	wg      sync.WaitGroup
	stopped bool
}

type job struct {
	cancel context.CancelFunc
}

type Options struct {
	MaxConcurrent int
	MaxStartDelay time.Duration
}

func New(store Store, checker Checker, notifier notify.Notifier, recorder Recorder, log *slog.Logger, opts Options) *Scheduler {
	if opts.MaxConcurrent <= 0 {
		opts.MaxConcurrent = 20
	}
	return &Scheduler{
		store: store, checker: checker, notifier: notifier, recorder: recorder, log: log,
		sem: make(chan struct{}, opts.MaxConcurrent), now: time.Now,
		maxStartDelay: opts.MaxStartDelay, jobs: map[int64]*job{},
	}
}

// Start loads all monitors and starts the active ones. The scheduler runs until ctx is cancelled;
// call Wait afterwards to let in-flight checks finish.
func (s *Scheduler) Start(ctx context.Context) error {
	monitors, err := s.store.ListMonitors(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()
	for _, m := range monitors {
		s.Upsert(m)
	}
	s.log.Info("scheduler started", "monitors", len(monitors))
	return nil
}

// Upsert (re)starts the job for a created or edited monitor, or stops it if the monitor is paused.
func (s *Scheduler) Upsert(m monitor.Monitor) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx == nil || s.stopped {
		return
	}
	s.stopLocked(m.ID)
	if m.Paused {
		s.recorder.Forget(m.ID)
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	j := &job{cancel: cancel}
	s.jobs[m.ID] = j
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.run(ctx, m)
	}()
}

// Remove stops the job of a deleted monitor.
func (s *Scheduler) Remove(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked(id)
	s.recorder.Forget(id)
}

func (s *Scheduler) stopLocked(id int64) {
	if j, ok := s.jobs[id]; ok {
		j.cancel()
		delete(s.jobs, id)
	}
}

// Wait blocks until every job has exited; jobs exit once the Start context is cancelled.
func (s *Scheduler) Wait() {
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *Scheduler) run(ctx context.Context, m monitor.Monitor) {
	state := monitor.State{Status: m.Status, ConsecutiveFailures: m.ConsecutiveFailures}
	if state.Status == "" {
		state.Status = monitor.StatusUnknown
	}

	if s.maxStartDelay > 0 {
		delay := time.Duration(rand.Int64N(int64(min(s.maxStartDelay, m.Interval))))
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return
		}
	}

	ticker := time.NewTicker(m.Interval)
	defer ticker.Stop()
	for {
		s.checkOnce(ctx, m, &state)
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

func (s *Scheduler) checkOnce(ctx context.Context, m monitor.Monitor, state *monitor.State) {
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return
	}
	result := s.checker.Check(ctx, m)
	<-s.sem
	if ctx.Err() != nil {
		return // stopped mid-check (edit, delete or shutdown): the result is not trustworthy
	}

	transition := state.Apply(result, m.FailureThreshold)
	s.recorder.ObserveCheck(m, result, *state)

	incident, err := s.store.RecordCheck(ctx, result, *state, transition)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			s.log.Error("record check", "monitor", m.ID, "err", err)
		}
		return
	}
	if transition == monitor.NoChange || incident == nil {
		return
	}

	event := notify.Event{Monitor: m, Incident: *incident, Result: result, At: s.now()}
	if transition == monitor.WentDown {
		event.Kind = notify.KindDown
	} else {
		event.Kind = notify.KindRecovered
	}
	s.log.Info("status changed", "monitor", m.ID, "name", m.Name, "transition", transition.String(), "cause", incident.Cause)
	// Delivery retries with backoff; it must not delay the next check, so it runs on its own goroutine.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		notifyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		if err := s.notifier.Notify(notifyCtx, event); err != nil {
			s.log.Error("alert delivery failed", "monitor", m.ID, "kind", event.Kind, "err", err)
		}
	}()
}
