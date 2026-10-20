package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/oldsoska/uptrack/internal/monitor"
	"github.com/oldsoska/uptrack/internal/store"
)

// monitorRequest is the body of create and update. Optional fields get sensible defaults.
type monitorRequest struct {
	Name              string `json:"name"`
	URL               string `json:"url"`
	Method            string `json:"method"`
	IntervalSeconds   int    `json:"intervalSeconds"`
	TimeoutSeconds    int    `json:"timeoutSeconds"`
	ExpectedStatusMin *int   `json:"expectedStatusMin"`
	ExpectedStatusMax *int   `json:"expectedStatusMax"`
	FailureThreshold  *int   `json:"failureThreshold"`
	Paused            bool   `json:"paused"`
}

func (req monitorRequest) toMonitor() monitor.Monitor {
	m := monitor.Monitor{
		Name: strings.TrimSpace(req.Name), URL: strings.TrimSpace(req.URL), Method: strings.ToUpper(req.Method),
		Interval: time.Duration(req.IntervalSeconds) * time.Second, Timeout: time.Duration(req.TimeoutSeconds) * time.Second,
		ExpectedStatusMin: 200, ExpectedStatusMax: 399, FailureThreshold: 2, Paused: req.Paused,
	}
	if m.Method == "" {
		m.Method = "GET"
	}
	if req.IntervalSeconds == 0 {
		m.Interval = time.Minute
	}
	if req.TimeoutSeconds == 0 {
		m.Timeout = 10 * time.Second
	}
	if req.ExpectedStatusMin != nil {
		m.ExpectedStatusMin = *req.ExpectedStatusMin
	}
	if req.ExpectedStatusMax != nil {
		m.ExpectedStatusMax = *req.ExpectedStatusMax
	}
	if req.FailureThreshold != nil {
		m.FailureThreshold = *req.FailureThreshold
	}
	return m
}

type statsResponse struct {
	Checks       int64    `json:"checks"`
	Uptime       *float64 `json:"uptime"`
	LatencyP50Ms *float64 `json:"latencyP50Ms"`
	LatencyP95Ms *float64 `json:"latencyP95Ms"`
}

type monitorResponse struct {
	ID                  int64          `json:"id"`
	Name                string         `json:"name"`
	URL                 string         `json:"url"`
	Method              string         `json:"method"`
	IntervalSeconds     int            `json:"intervalSeconds"`
	TimeoutSeconds      int            `json:"timeoutSeconds"`
	ExpectedStatusMin   int            `json:"expectedStatusMin"`
	ExpectedStatusMax   int            `json:"expectedStatusMax"`
	FailureThreshold    int            `json:"failureThreshold"`
	Paused              bool           `json:"paused"`
	Status              monitor.Status `json:"status"`
	ConsecutiveFailures int            `json:"consecutiveFailures"`
	LastCheckedAt       *time.Time     `json:"lastCheckedAt"`
	CreatedAt           time.Time      `json:"createdAt"`
	Last24h             *statsResponse `json:"last24h,omitempty"`
	Last7d              *statsResponse `json:"last7d,omitempty"`
}

func toResponse(m monitor.Monitor) monitorResponse {
	return monitorResponse{
		ID: m.ID, Name: m.Name, URL: m.URL, Method: m.Method,
		IntervalSeconds: int(m.Interval.Seconds()), TimeoutSeconds: int(m.Timeout.Seconds()),
		ExpectedStatusMin: m.ExpectedStatusMin, ExpectedStatusMax: m.ExpectedStatusMax,
		FailureThreshold: m.FailureThreshold, Paused: m.Paused, Status: m.Status,
		ConsecutiveFailures: m.ConsecutiveFailures, LastCheckedAt: m.LastCheckedAt, CreatedAt: m.CreatedAt,
	}
}

func toStats(st store.Stats) *statsResponse {
	return &statsResponse{Checks: st.Checks, Uptime: st.Uptime, LatencyP50Ms: st.LatencyP50Ms, LatencyP95Ms: st.LatencyP95Ms}
}

func (s *Server) listMonitors(w http.ResponseWriter, r *http.Request) {
	monitors, err := s.store.ListWithStats(r.Context(), s.now().Add(-24*time.Hour))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := make([]monitorResponse, 0, len(monitors))
	for _, m := range monitors {
		resp := toResponse(m.Monitor)
		resp.Last24h = toStats(m.Stats)
		out = append(out, resp)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createMonitor(w http.ResponseWriter, r *http.Request) {
	var req monitorRequest
	if err := readJSON(w, r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, err.Error())
		return
	}
	m := req.toMonitor()
	if err := m.Validate(); err != nil {
		writeProblem(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	created, err := s.store.CreateMonitor(r.Context(), m)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.jobs.Upsert(created)
	w.Header().Set("Location", "/api/v1/monitors/"+strconv.FormatInt(created.ID, 10))
	writeJSON(w, http.StatusCreated, toResponse(created))
}

func (s *Server) getMonitor(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	m, err := s.store.GetMonitor(r.Context(), id)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	day, err := s.store.MonitorStats(r.Context(), id, s.now().Add(-24*time.Hour))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	week, err := s.store.MonitorStats(r.Context(), id, s.now().Add(-7*24*time.Hour))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	resp := toResponse(m)
	resp.Last24h, resp.Last7d = toStats(day), toStats(week)
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) updateMonitor(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req monitorRequest
	if err := readJSON(w, r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, err.Error())
		return
	}
	m := req.toMonitor()
	m.ID = id
	if err := m.Validate(); err != nil {
		writeProblem(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	updated, err := s.store.UpdateMonitor(r.Context(), m)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	s.jobs.Upsert(updated)
	writeJSON(w, http.StatusOK, toResponse(updated))
}

func (s *Server) deleteMonitor(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteMonitor(r.Context(), id); err != nil {
		s.storeError(w, r, err)
		return
	}
	s.jobs.Remove(id)
	w.WriteHeader(http.StatusNoContent)
}

type resultResponse struct {
	CheckedAt  time.Time `json:"checkedAt"`
	OK         bool      `json:"ok"`
	StatusCode int       `json:"statusCode,omitempty"`
	LatencyMs  int64     `json:"latencyMs"`
	Error      string    `json:"error,omitempty"`
}

func (s *Server) listResults(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	limit, ok := queryLimit(w, r, 50, 500)
	if !ok {
		return
	}
	if _, err := s.store.GetMonitor(r.Context(), id); err != nil {
		s.storeError(w, r, err)
		return
	}
	results, err := s.store.RecentResults(r.Context(), id, limit)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := make([]resultResponse, 0, len(results))
	for _, res := range results {
		out = append(out, resultResponse{CheckedAt: res.CheckedAt, OK: res.OK, StatusCode: res.StatusCode,
			LatencyMs: res.Latency.Milliseconds(), Error: res.Error})
	}
	writeJSON(w, http.StatusOK, out)
}

type incidentResponse struct {
	ID              int64      `json:"id"`
	MonitorID       int64      `json:"monitorId"`
	MonitorName     string     `json:"monitorName"`
	OpenedAt        time.Time  `json:"openedAt"`
	ResolvedAt      *time.Time `json:"resolvedAt"`
	Cause           string     `json:"cause"`
	DurationSeconds int64      `json:"durationSeconds"`
}

func (s *Server) listIncidents(w http.ResponseWriter, r *http.Request) {
	limit, ok := queryLimit(w, r, 50, 500)
	if !ok {
		return
	}
	incidents, err := s.store.ListIncidents(r.Context(), r.URL.Query().Get("open") == "true", limit)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	now := s.now()
	out := make([]incidentResponse, 0, len(incidents))
	for _, i := range incidents {
		out = append(out, incidentResponse{ID: i.ID, MonitorID: i.MonitorID, MonitorName: i.MonitorName,
			OpenedAt: i.OpenedAt, ResolvedAt: i.ResolvedAt, Cause: i.Cause,
			DurationSeconds: int64(i.Duration(now).Seconds())})
	}
	writeJSON(w, http.StatusOK, out)
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeProblem(w, http.StatusBadRequest, "id must be a positive integer")
		return 0, false
	}
	return id, true
}

func queryLimit(w http.ResponseWriter, r *http.Request, def, max int) (int, bool) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return def, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > max {
		writeProblem(w, http.StatusBadRequest, "limit must be between 1 and "+strconv.Itoa(max))
		return 0, false
	}
	return n, true
}

func (s *Server) storeError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "monitor not found")
		return
	}
	s.internalError(w, r, err)
}

// internalError logs the cause and returns a generic message: database errors can reveal schema details.
func (s *Server) internalError(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	writeProblem(w, http.StatusInternalServerError, "internal error")
}
