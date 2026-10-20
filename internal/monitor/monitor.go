// Package monitor holds the domain model: what is checked, what a check produced,
// and how a series of checks turns into UP/DOWN status and incidents.
package monitor

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

type Status string

const (
	StatusUnknown Status = "UNKNOWN"
	StatusUp      Status = "UP"
	StatusDown    Status = "DOWN"
)

const (
	MinInterval = 30 * time.Second
	MaxInterval = 24 * time.Hour
	MinTimeout  = 1 * time.Second
	MaxTimeout  = 30 * time.Second
)

// Monitor is one URL checked on a schedule.
type Monitor struct {
	ID                int64
	Name              string
	URL               string
	Method            string
	Interval          time.Duration
	Timeout           time.Duration
	ExpectedStatusMin int
	ExpectedStatusMax int
	// FailureThreshold is how many failed checks in a row mark the monitor DOWN,
	// so a single network blip does not page anyone.
	FailureThreshold int
	Paused           bool

	Status              Status
	ConsecutiveFailures int
	LastCheckedAt       *time.Time
	CreatedAt           time.Time
}

// Validate checks user-supplied settings. The database repeats the same rules as CHECK constraints.
func (m Monitor) Validate() error {
	var errs []error
	if name := strings.TrimSpace(m.Name); name == "" || len(name) > 100 {
		errs = append(errs, errors.New("name must be 1-100 characters"))
	}
	if u, err := url.Parse(m.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		errs = append(errs, errors.New("url must be an absolute http or https URL"))
	}
	if m.Method != "GET" && m.Method != "HEAD" {
		errs = append(errs, errors.New("method must be GET or HEAD"))
	}
	if m.Interval < MinInterval || m.Interval > MaxInterval {
		errs = append(errs, fmt.Errorf("interval must be between %s and %s", MinInterval, MaxInterval))
	}
	if m.Timeout < MinTimeout || m.Timeout > MaxTimeout {
		errs = append(errs, fmt.Errorf("timeout must be between %s and %s", MinTimeout, MaxTimeout))
	}
	if m.Timeout >= m.Interval {
		errs = append(errs, errors.New("timeout must be shorter than interval"))
	}
	if m.ExpectedStatusMin < 100 || m.ExpectedStatusMax > 599 || m.ExpectedStatusMin > m.ExpectedStatusMax {
		errs = append(errs, errors.New("expected status range must be within 100-599 and min <= max"))
	}
	if m.FailureThreshold < 1 || m.FailureThreshold > 10 {
		errs = append(errs, errors.New("failureThreshold must be between 1 and 10"))
	}
	return errors.Join(errs...)
}

// Result is the outcome of one check.
type Result struct {
	MonitorID  int64
	CheckedAt  time.Time
	OK         bool
	StatusCode int // 0 when no HTTP response was received
	Latency    time.Duration
	Error      string // why the check failed, empty when OK
}

// Incident is a period during which a monitor was DOWN.
type Incident struct {
	ID         int64
	MonitorID  int64
	OpenedAt   time.Time
	ResolvedAt *time.Time
	Cause      string
}

func (i Incident) Duration(now time.Time) time.Duration {
	if i.ResolvedAt != nil {
		return i.ResolvedAt.Sub(i.OpenedAt)
	}
	return now.Sub(i.OpenedAt)
}
