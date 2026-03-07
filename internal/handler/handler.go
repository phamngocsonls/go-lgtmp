// Package handler contains HTTP handlers for the demo endpoints.
// Each handler demonstrates different telemetry patterns:
//
//   - /ping        — minimal span + log (baseline)
//   - /rolldice    — random latency + 10% error rate (latency/error distribution)
//   - /fibonacci   — CPU-bound computation (profiling flamegraph, slow spans)
//
// # RED Metrics
//
// Rate, Errors, and Duration are provided automatically by otelhttp at the
// router level via the http_server_request_duration_seconds histogram with
// labels: http_request_method, http_route, http_response_status_code.
//
// This file only defines domain-specific metrics that otelhttp does not cover.
package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"strconv"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/go-lgtmp/go-lgtmp/internal/store"
)

var tracer = otel.Tracer("github.com/go-lgtmp/go-lgtmp/handler")

// Metrics holds domain-specific OTel metric instruments.
// RED metrics (rate, errors, duration) are handled automatically by otelhttp.
// These instruments cover application-level semantics otelhttp cannot infer.
type Metrics struct {
	activeRequests metric.Int64UpDownCounter // in-flight request gauge
	fibComputed    metric.Int64Counter       // fibonacci computations (domain metric)
}

// NewMetrics creates and registers domain-specific metric instruments.
//
// Prometheus metric names exposed at /metrics:
//
//	demo_active_requests             — gauge: in-flight requests
//	demo_fibonacci_computed_total    — counter: fibonacci calls by n value
//
// RED metrics come from otelhttp automatically:
//
//	http_server_request_duration_seconds{http_request_method, http_route, http_response_status_code, ...}
func NewMetrics() (*Metrics, error) {
	meter := otel.Meter("github.com/go-lgtmp/go-lgtmp")

	activeRequests, err := meter.Int64UpDownCounter("demo_active_requests",
		metric.WithDescription("Number of in-flight HTTP requests"),
		metric.WithUnit("{request}"),
	)
	if err != nil {
		return nil, err
	}

	fibComputed, err := meter.Int64Counter("demo_fibonacci_computed_total",
		metric.WithDescription("Total fibonacci computations by input size"),
		metric.WithUnit("{computation}"),
	)
	if err != nil {
		return nil, err
	}

	return &Metrics{
		activeRequests: activeRequests,
		fibComputed:    fibComputed,
	}, nil
}

// Handler holds shared dependencies for all HTTP handlers.
type Handler struct {
	metrics *Metrics
	db      *store.DB    // nil if DATABASE_DSN not set
	cache   *store.Cache // nil if REDIS_ADDR not set
}

// New creates a Handler. db and cache may be nil — endpoints that require them
// return 503 with a descriptive message when their dependency is absent.
func New(m *Metrics, db *store.DB, cache *store.Cache) *Handler {
	return &Handler{metrics: m, db: db, cache: cache}
}

// Ping handles GET /ping.
// Demonstrates: minimal span, structured log.
func (h *Handler) Ping(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "ping")
	defer span.End()

	h.metrics.activeRequests.Add(ctx, 1)
	defer h.metrics.activeRequests.Add(ctx, -1)

	slog.InfoContext(ctx, "ping called")

	writeJSON(w, http.StatusOK, map[string]string{"message": "pong"})
}

// RollDice handles GET /rolldice.
// Demonstrates: variable latency (0–200ms), 10% synthetic error rate,
// span attributes, span status for errors.
//
// Good for demoing RED metrics in Grafana:
//   - Rate:     rate(http_server_request_duration_seconds_count{http_route="/rolldice"}[1m])
//   - Errors:   rate(...{http_response_status_code="500"}[1m])
//   - Duration: histogram_quantile(0.99, rate(..._bucket{http_route="/rolldice"}[5m]))
func (h *Handler) RollDice(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "roll_dice")
	defer span.End()

	h.metrics.activeRequests.Add(ctx, 1)
	defer h.metrics.activeRequests.Add(ctx, -1)

	// Simulate realistic variable latency
	delay := time.Duration(rand.Intn(200)) * time.Millisecond
	time.Sleep(delay)

	roll := rand.Intn(6) + 1
	span.SetAttributes(
		attribute.Int("dice.roll", roll),
		attribute.Int64("dice.delay_ms", delay.Milliseconds()),
	)

	// Simulate 10% error rate for demo purposes
	if rand.Float64() < 0.10 {
		err := errors.New("dice rolled off the table")
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())

		slog.ErrorContext(ctx, "dice roll failed", "roll", roll, "delay_ms", delay.Milliseconds())

		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	slog.InfoContext(ctx, "dice rolled", "roll", roll, "delay_ms", delay.Milliseconds())

	writeJSON(w, http.StatusOK, map[string]any{
		"roll":     roll,
		"delay_ms": delay.Milliseconds(),
	})
}

// Fibonacci handles GET /fibonacci?n=<int>.
// Demonstrates: CPU-intensive work visible in Pyroscope flamegraphs,
// slow spans visible in Tempo, span events for computation steps.
// Max n=40 to prevent excessive load.
func (h *Handler) Fibonacci(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "fibonacci")
	defer span.End()

	h.metrics.activeRequests.Add(ctx, 1)
	defer h.metrics.activeRequests.Add(ctx, -1)

	nStr := r.URL.Query().Get("n")
	if nStr == "" {
		nStr = "10"
	}

	n, err := strconv.Atoi(nStr)
	if err != nil || n < 0 {
		span.SetStatus(codes.Error, "invalid n parameter")
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "n must be a non-negative integer",
		})
		return
	}

	if n > 40 {
		span.SetStatus(codes.Error, "n too large")
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "n must be <= 40 to prevent excessive CPU load",
		})
		return
	}

	span.SetAttributes(attribute.Int("fibonacci.n", n))
	span.AddEvent("computation_start", trace.WithAttributes(attribute.Int("fibonacci.n", n)))

	start := time.Now()

	// Intentional recursive computation to generate CPU profile data
	result := fib(n)

	duration := time.Since(start)
	span.AddEvent("computation_complete", trace.WithAttributes(
		attribute.Int("fibonacci.n", n),
		attribute.Int64("fibonacci.result", result),
	))
	span.SetAttributes(attribute.Int64("fibonacci.result", result))

	slog.InfoContext(ctx, "fibonacci computed",
		"n", n,
		"result", result,
		"duration_ms", duration.Milliseconds(),
	)

	// Domain-specific counter — tracks which input sizes are being computed.
	h.metrics.fibComputed.Add(ctx, 1, metric.WithAttributes(attribute.Int("n", n)))

	writeJSON(w, http.StatusOK, map[string]any{
		"n":           n,
		"result":      fmt.Sprintf("%d", result),
		"duration_ms": duration.Milliseconds(),
	})
}

// fib computes fibonacci(n) recursively (intentionally not memoized
// so it burns CPU and shows up clearly in Pyroscope flamegraphs).
func fib(n int) int64 {
	if n <= 1 {
		return int64(n)
	}
	return fib(n-1) + fib(n-2)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
