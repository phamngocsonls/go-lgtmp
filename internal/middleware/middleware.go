// Package middleware provides HTTP middleware for structured logging with
// automatic trace/span correlation.
package middleware

import (
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/trace"
)

// responseWriter wraps http.ResponseWriter to capture the status code and
// bytes written. It also delegates http.Flusher so streaming responses (SSE,
// chunked transfer) work correctly through this middleware.
type responseWriter struct {
	http.ResponseWriter
	status int
	size   int
}

func (rw *responseWriter) WriteHeader(status int) {
	rw.status = status
	rw.ResponseWriter.WriteHeader(status)
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	n, err := rw.ResponseWriter.Write(b)
	rw.size += n
	return n, err
}

// Flush implements http.Flusher — delegates to the underlying writer if it
// supports flushing (e.g. for SSE or chunked streaming responses).
func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Logging is an HTTP middleware that emits a structured slog record for every
// request. It injects trace_id and span_id extracted from the active OTel span
// so that logs can be correlated with traces in Grafana.
//
// Mount this inside the chi router (after otelhttp.NewHandler wraps the router)
// so the span is already active when this middleware reads it.
//
//	httpHandler := otelhttp.NewHandler(r, ...)   // creates span
//	r.Use(middleware.Logging)                     // reads span → injects trace_id
func Logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rw, r)

		duration := time.Since(start)

		// Extract trace context for log-trace correlation in Grafana.
		// Loki datasource can jump to Tempo using the trace_id field.
		spanCtx := trace.SpanFromContext(r.Context()).SpanContext()

		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"duration_ms", duration.Milliseconds(),
			"bytes", rw.size,
			"user_agent", r.UserAgent(),
			"remote_addr", r.RemoteAddr,
		}

		if spanCtx.IsValid() {
			attrs = append(attrs,
				"trace_id", spanCtx.TraceID().String(),
				"span_id", spanCtx.SpanID().String(),
			)
		}

		level := slog.LevelInfo
		if rw.status >= 500 {
			level = slog.LevelError
		} else if rw.status >= 400 {
			level = slog.LevelWarn
		}

		slog.Log(r.Context(), level, "http request", attrs...)
	})
}
