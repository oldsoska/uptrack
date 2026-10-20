// Package httpapi serves the admin REST API, the public status page, health checks and metrics.
package httpapi

import (
	"crypto/subtle"
	"embed"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/oldsoska/uptrack/internal/monitor"
	"github.com/oldsoska/uptrack/internal/store"
)

// Jobs is the part of the scheduler the API drives: it must learn about every change to monitors.
type Jobs interface {
	Upsert(m monitor.Monitor)
	Remove(id int64)
}

type Server struct {
	store      *store.Store
	jobs       Jobs
	adminToken []byte
	log        *slog.Logger
	now        func() time.Time
	statusTmpl *template.Template
	handler    http.Handler
}

//go:embed templates/*.html
var templates embed.FS

func New(st *store.Store, jobs Jobs, adminToken string, registry *prometheus.Registry, log *slog.Logger) *Server {
	s := &Server{
		store: st, jobs: jobs, adminToken: []byte(adminToken), log: log, now: time.Now,
		statusTmpl: template.Must(template.New("status.html").Funcs(templateFuncs).ParseFS(templates, "templates/status.html")),
	}

	mux := http.NewServeMux()
	// Public
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/status", http.StatusFound)
	})
	mux.HandleFunc("GET /status", s.statusPage)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", s.ready)
	mux.Handle("GET /metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))

	// Admin API, bearer token required
	api := http.NewServeMux()
	api.HandleFunc("GET /api/v1/monitors", s.listMonitors)
	api.HandleFunc("POST /api/v1/monitors", s.createMonitor)
	api.HandleFunc("GET /api/v1/monitors/{id}", s.getMonitor)
	api.HandleFunc("PUT /api/v1/monitors/{id}", s.updateMonitor)
	api.HandleFunc("DELETE /api/v1/monitors/{id}", s.deleteMonitor)
	api.HandleFunc("GET /api/v1/monitors/{id}/results", s.listResults)
	api.HandleFunc("GET /api/v1/incidents", s.listIncidents)
	mux.Handle("/api/", s.requireAdmin(api))

	s.handler = s.recoverPanics(s.logRequests(securityHeaders(mux)))
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(r.Context()); err != nil {
		writeProblem(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	w.WriteHeader(http.StatusOK)
}

// requireAdmin checks "Authorization: Bearer <ADMIN_TOKEN>". The comparison takes the same time
// whatever the input, so the token cannot be guessed byte by byte from response timings.
func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(token), s.adminToken) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="uptrack"`)
			writeProblem(w, http.StatusUnauthorized, "missing or invalid admin token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if r.URL.Path == "/healthz" || r.URL.Path == "/metrics" {
			return // probed every few seconds; logging them would drown everything else
		}
		s.log.Info("http", "method", r.Method, "path", r.URL.Path, "status", rec.status,
			"duration_ms", time.Since(started).Milliseconds())
	})
}

func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.log.Error("panic", "path", r.URL.Path, "value", v)
				writeProblem(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
