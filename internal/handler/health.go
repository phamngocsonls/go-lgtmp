package handler

import "net/http"

// Healthz handles GET /healthz (liveness probe).
// Always returns 200 — the process is alive.
// Not instrumented to avoid telemetry noise from k8s probes.
func Healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Readyz handles GET /readyz (readiness probe).
// Returns 200 when the service is ready to accept traffic.
// Extend this to check DB connections, upstream dependencies, etc.
func Readyz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}
