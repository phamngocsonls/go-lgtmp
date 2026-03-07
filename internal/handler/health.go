package handler

import "net/http"

// Healthz handles GET /healthz (liveness probe).
// Always returns 200 — the process is alive.
// Not instrumented to avoid telemetry noise from k8s probes.
func Healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Readyz handles GET /readyz (readiness probe).
// Returns 200 only when all configured dependencies are reachable.
// A 503 response causes k8s to stop routing traffic until the check recovers.
//
// Checks performed:
//   - PostgreSQL: db.Ping (if DATABASE_DSN is set)
//   - Redis: cache.Ping (if REDIS_ADDR is set)
func (h *Handler) Readyz(w http.ResponseWriter, r *http.Request) {
	type check struct {
		Status string `json:"status"`
		Error  string `json:"error,omitempty"`
	}
	checks := map[string]check{}
	ready := true

	if h.db != nil {
		if err := h.db.Ping(r.Context()); err != nil {
			checks["postgres"] = check{Status: "error", Error: err.Error()}
			ready = false
		} else {
			checks["postgres"] = check{Status: "ok"}
		}
	}

	if h.cache != nil {
		if err := h.cache.Ping(r.Context()); err != nil {
			checks["redis"] = check{Status: "error", Error: err.Error()}
			ready = false
		} else {
			checks["redis"] = check{Status: "ok"}
		}
	}

	if !ready {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "not ready",
			"checks": checks,
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ready",
		"checks": checks,
	})
}
