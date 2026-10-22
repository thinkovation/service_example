package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/thinkovation/service_example/internal/buildinfo"
)

// statusResponse is the body of GET /status: who this process is and how long
// it has been up. It never touches a dependency, so it stays cheap and always
// answers 200 while the process is alive.
type statusResponse struct {
	Service       string          `json:"service"`
	Env           string          `json:"env"`
	Status        string          `json:"status"`
	Build         buildinfo.Info  `json:"build"`
	StartedAt     time.Time       `json:"started_at"`
	UptimeSeconds float64         `json:"uptime_seconds"`
	Now           time.Time       `json:"now"`
	Store         storeDescriptor `json:"store"`
}

type storeDescriptor struct {
	Driver string `json:"driver"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	now := s.opts.Clock()
	writeJSON(w, r, http.StatusOK, statusResponse{
		Service:       s.opts.ServiceName,
		Env:           s.opts.Env,
		Status:        "ok",
		Build:         buildinfo.Get(),
		StartedAt:     s.started,
		UptimeSeconds: now.Sub(s.started).Seconds(),
		Now:           now,
		Store:         storeDescriptor{Driver: s.opts.Store.Driver()},
	})
}

// Health states reported by /zhealth.
const (
	healthOK   = "ok"
	healthFail = "fail"
)

// healthResponse is the body of GET /zhealth: a readiness probe that exercises
// every dependency the service needs to serve traffic.
type healthResponse struct {
	Status    string        `json:"status"`
	Service   string        `json:"service"`
	CheckedAt time.Time     `json:"checked_at"`
	Checks    []healthCheck `json:"checks"`
}

type healthCheck struct {
	Name      string  `json:"name"`
	Status    string  `json:"status"`
	Detail    string  `json:"detail,omitempty"`
	Error     string  `json:"error,omitempty"`
	LatencyMS float64 `json:"latency_ms"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), s.opts.HealthTimeout)
	defer cancel()

	check := healthCheck{Name: "database", Status: healthOK, Detail: s.opts.Store.Driver()}
	start := time.Now()
	if err := s.opts.Store.Ping(ctx); err != nil {
		check.Status = healthFail
		check.Error = err.Error()
	}
	check.LatencyMS = float64(time.Since(start).Microseconds()) / 1000

	body := healthResponse{
		Status:    healthOK,
		Service:   s.opts.ServiceName,
		CheckedAt: s.opts.Clock(),
		Checks:    []healthCheck{check},
	}
	status := http.StatusOK
	for _, c := range body.Checks {
		if c.Status != healthOK {
			body.Status = healthFail
			status = http.StatusServiceUnavailable
			break
		}
	}

	// Probes must never be answered from a cache.
	w.Header().Set("Cache-Control", "no-store")
	if status != http.StatusOK {
		loggerFrom(r.Context()).WarnContext(r.Context(), "health check failed",
			"checks", body.Checks)
	}
	writeJSON(w, r, status, body)
}
