package httpapi

import (
	"fmt"
	"html/template"
	"net/http"
	"time"

	"github.com/oldsoska/uptrack/internal/monitor"
	"github.com/oldsoska/uptrack/internal/store"
)

const statusDays = 30

type statusPageData struct {
	AllUp     bool
	Monitors  []statusMonitor
	UpdatedAt time.Time
	Days      int
}

type statusMonitor struct {
	Name     string
	Status   monitor.Status
	Uptime24 *float64
	Bars     []store.DayUptime
}

// statusPage is public, so it shows names, status and history only — never URLs, which may be internal.
func (s *Server) statusPage(w http.ResponseWriter, r *http.Request) {
	now := s.now()
	monitors, err := s.store.ListWithStats(r.Context(), now.Add(-24*time.Hour))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	daily, err := s.store.DailyUptime(r.Context(), statusDays, now)
	if err != nil {
		s.internalError(w, r, err)
		return
	}

	data := statusPageData{AllUp: true, UpdatedAt: now.UTC(), Days: statusDays}
	for _, m := range monitors {
		if m.Paused {
			continue
		}
		if m.Status == monitor.StatusDown {
			data.AllUp = false
		}
		data.Monitors = append(data.Monitors, statusMonitor{
			Name: m.Name, Status: m.Status, Uptime24: m.Stats.Uptime, Bars: daily[m.ID],
		})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=30")
	if err := s.statusTmpl.Execute(w, data); err != nil {
		s.log.Error("render status page", "err", err)
	}
}

var templateFuncs = template.FuncMap{
	"percent": func(v *float64) string {
		if v == nil {
			return "—"
		}
		return fmt.Sprintf("%.2f%%", *v*100)
	},
	// barClass buckets a day's uptime into a colour; days without checks are grey.
	"barClass": func(d store.DayUptime) string {
		switch {
		case d.Uptime == nil:
			return "none"
		case *d.Uptime >= 0.999:
			return "good"
		case *d.Uptime >= 0.95:
			return "warn"
		default:
			return "bad"
		}
	},
	"barTitle": func(d store.DayUptime) string {
		if d.Uptime == nil {
			return d.Day.Format("Jan 2") + ": no data"
		}
		return fmt.Sprintf("%s: %.2f%% of %d checks", d.Day.Format("Jan 2"), *d.Uptime*100, d.Checks)
	},
	"statusClass": func(s monitor.Status) string {
		switch s {
		case monitor.StatusUp:
			return "good"
		case monitor.StatusDown:
			return "bad"
		default:
			return "none"
		}
	},
}
