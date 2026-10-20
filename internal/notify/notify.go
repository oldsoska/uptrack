// Package notify delivers incident alerts: Telegram, a generic JSON webhook, or both.
package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/oldsoska/uptrack/internal/monitor"
)

type Kind string

const (
	KindDown      Kind = "down"
	KindRecovered Kind = "recovered"
)

// Event is one status change worth telling someone about.
type Event struct {
	Kind     Kind
	Monitor  monitor.Monitor
	Incident monitor.Incident
	Result   monitor.Result
	At       time.Time
}

func (e Event) Summary() string {
	switch e.Kind {
	case KindDown:
		return fmt.Sprintf("%s is DOWN: %s", e.Monitor.Name, e.Incident.Cause)
	default:
		return fmt.Sprintf("%s is back UP after %s", e.Monitor.Name, e.Incident.Duration(e.At).Round(time.Second))
	}
}

type Notifier interface {
	Notify(ctx context.Context, e Event) error
}

// Multi sends to every notifier; one failing channel does not stop the others.
type Multi []Notifier

func (m Multi) Notify(ctx context.Context, e Event) error {
	var errs []error
	for _, n := range m {
		if err := n.Notify(ctx, e); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Log only writes the event to the log; used when no channel is configured.
type Log struct{ Logger *slog.Logger }

func (l Log) Notify(_ context.Context, e Event) error {
	l.Logger.Info("alert", "kind", e.Kind, "monitor", e.Monitor.Name, "summary", e.Summary())
	return nil
}

// Retry retries a notifier with exponential backoff: base, 2*base, 4*base...
type Retry struct {
	Next     Notifier
	Attempts int
	Base     time.Duration
}

func (r Retry) Notify(ctx context.Context, e Event) error {
	var err error
	delay := r.Base
	for attempt := 1; attempt <= r.Attempts; attempt++ {
		if err = r.Next.Notify(ctx, e); err == nil {
			return nil
		}
		if attempt == r.Attempts {
			break
		}
		select {
		case <-time.After(delay):
			delay *= 2
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		}
	}
	return fmt.Errorf("after %d attempts: %w", r.Attempts, err)
}
