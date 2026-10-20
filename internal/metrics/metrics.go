// Package metrics exposes check results in Prometheus format at /metrics.
package metrics

import (
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/oldsoska/uptrack/internal/monitor"
)

// Metrics are labelled by monitor id rather than name or URL: ids are stable and few, which keeps
// label cardinality bounded, and names can be joined in Grafana from the API if needed.
type Metrics struct {
	Registry *prometheus.Registry

	checks   *prometheus.CounterVec
	duration *prometheus.HistogramVec
	up       *prometheus.GaugeVec
}

func New() *Metrics {
	m := &Metrics{
		Registry: prometheus.NewRegistry(),
		checks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "uptrack_checks_total",
			Help: "Checks performed, by monitor and outcome.",
		}, []string{"monitor_id", "result"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "uptrack_check_duration_seconds",
			Help:    "Time from request start to response headers, for every check.",
			Buckets: []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 30},
		}, []string{"monitor_id"}),
		up: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "uptrack_monitor_up",
			Help: "1 if the monitor is UP, 0 if DOWN; absent until the first check.",
		}, []string{"monitor_id"}),
	}
	m.Registry.MustRegister(m.checks, m.duration, m.up,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return m
}

func (m *Metrics) ObserveCheck(mon monitor.Monitor, r monitor.Result, st monitor.State) {
	id := strconv.FormatInt(mon.ID, 10)
	result := "success"
	if !r.OK {
		result = "failure"
	}
	m.checks.WithLabelValues(id, result).Inc()
	m.duration.WithLabelValues(id).Observe(r.Latency.Seconds())
	switch st.Status {
	case monitor.StatusUp:
		m.up.WithLabelValues(id).Set(1)
	case monitor.StatusDown:
		m.up.WithLabelValues(id).Set(0)
	}
}

// Forget drops the series of a deleted or paused monitor so dashboards do not show it as stale.
func (m *Metrics) Forget(monitorID int64) {
	id := strconv.FormatInt(monitorID, 10)
	m.checks.DeletePartialMatch(prometheus.Labels{"monitor_id": id})
	m.duration.DeleteLabelValues(id)
	m.up.DeleteLabelValues(id)
}
