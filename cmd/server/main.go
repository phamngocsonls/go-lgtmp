// Command server is the entry point for the go-lgtmp demo service.
// It wires together OpenTelemetry (traces, metrics, logs) and Pyroscope profiling,
// then starts an HTTP server with demo endpoints.
//
// See README.md for environment variable reference and Grafana query examples.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	chi "github.com/go-chi/chi/v5"
	chiMiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/go-lgtmp/go-lgtmp/internal/config"
	"github.com/go-lgtmp/go-lgtmp/internal/handler"
	internalmw "github.com/go-lgtmp/go-lgtmp/internal/middleware"
	"github.com/go-lgtmp/go-lgtmp/internal/store"
	"github.com/go-lgtmp/go-lgtmp/internal/telemetry"
)

func main() {
	cfg := config.Load()

	// ── OpenTelemetry ─────────────────────────────────────────────────────────
	// Must be initialised before creating any metrics or starting handlers so
	// the global MeterProvider and TracerProvider are set before use.
	otelShutdown, err := telemetry.InitOTel(context.Background(), cfg)
	if err != nil {
		slog.Error("failed to initialize OTel", "error", err)
		os.Exit(1)
	}

	// ── Pyroscope ─────────────────────────────────────────────────────────────
	profilerStop, err := telemetry.InitProfiler(cfg)
	if err != nil {
		slog.Warn("Pyroscope profiler not available — continuing without profiling", "error", err)
		profilerStop = func() {}
	}

	// ── Custom metrics ────────────────────────────────────────────────────────
	metrics, err := handler.NewMetrics()
	if err != nil {
		slog.Error("failed to initialize metrics", "error", err)
		os.Exit(1)
	}

	// ── PostgreSQL ────────────────────────────────────────────────────────────
	// Optional: set DATABASE_DSN to enable /db/* endpoints.
	// otelsql wraps database/sql and auto-creates child spans per query.
	var db *store.DB
	if cfg.DatabaseDSN != "" {
		db, err = store.OpenDB(cfg.DatabaseDSN)
		if err != nil {
			slog.Warn("database unavailable, /db/* endpoints disabled", "error", err)
		} else {
			migrateCtx, migrateCancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := db.CreateUsersTable(migrateCtx); err != nil {
				slog.Warn("failed to create users table", "error", err)
			}
			migrateCancel()
			slog.Info("database connected")
		}
	}

	// ── Redis ─────────────────────────────────────────────────────────────────
	// Optional: set REDIS_ADDR to enable /cache/* endpoints.
	// redisotel hooks into go-redis and auto-creates child spans per command.
	var cache *store.Cache
	if cfg.RedisAddr != "" {
		cache, err = store.OpenCache(cfg.RedisAddr)
		if err != nil {
			slog.Warn("redis unavailable, /cache/* endpoints disabled", "error", err)
		} else {
			slog.Info("redis connected", "addr", cfg.RedisAddr)
		}
	}

	// ── Router ────────────────────────────────────────────────────────────────
	r := chi.NewRouter()
	r.Use(chiMiddleware.Recoverer)

	// RoutePattern middleware: after chi has matched the route, stamp the matched
	// pattern onto both the otelhttp Labeler (so http_server_request_duration_seconds
	// gets a proper http_route label in Mimir) and the root span name (so traces
	// in Tempo show "GET /rolldice" instead of "GET /rolldice?n=35").
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if rctx := chi.RouteContext(r.Context()); rctx != nil {
				if pattern := rctx.RoutePattern(); pattern != "" {
					// Stamp route on metrics label
					if labeler, ok := otelhttp.LabelerFromContext(r.Context()); ok {
						labeler.Add(attribute.String("http.route", pattern))
					}
					// Update root span name to use route pattern (not raw URL path)
					trace.SpanFromContext(r.Context()).
						SetName(fmt.Sprintf("%s %s", r.Method, pattern))
				}
			}
			next.ServeHTTP(w, r)
		})
	})

	h := handler.New(metrics, db, cache)

	// Health probes and /metrics are excluded from OTel instrumentation via
	// WithFilter below — registering them on the router is still needed for routing.
	r.Get("/healthz", handler.Healthz)
	// Readyz pings DB and Redis (if configured) to signal real readiness.
	r.Get("/readyz", h.Readyz)
	r.Handle("/metrics", promhttp.Handler())

	// Demo endpoints
	r.Group(func(r chi.Router) {
		r.Use(internalmw.Logging)

		// Baseline demos
		r.Get("/ping", h.Ping)
		r.Get("/rolldice", h.RollDice)
		r.Get("/fibonacci", h.Fibonacci)

		// Database demos — require DATABASE_DSN
		// Trace tree: HTTP span → handler span → otelsql db span (auto)
		r.Get("/db", h.DBInfo)
		r.Get("/db/users", h.DBListUsers)
		r.Post("/db/users", h.DBCreateUser)

		// Cache demos — require REDIS_ADDR (+ DATABASE_DSN for cache-aside)
		// Trace tree: HTTP span → handler span → redisotel redis span (auto)
		r.Post("/cache", h.CacheSet)
		r.Get("/cache/users/{id}", h.CacheGetUser)
	})

	// Wrap the entire router with OTel HTTP auto-instrumentation.
	//
	// otelhttp automatically records:
	//   http_server_request_duration_seconds{http_request_method, http_route,
	//     http_response_status_code, network_protocol_version, server_address, server_port}
	//
	// RED metrics for every route — zero per-handler boilerplate:
	//   Rate:     rate(http_server_request_duration_seconds_count{http_route="/rolldice"}[1m])
	//   Errors:   rate(...{http_response_status_code=~"5.."}[1m])
	//   Duration: histogram_quantile(0.99, rate(..._bucket{http_route="/rolldice"}[5m]))
	httpHandler := otelhttp.NewHandler(r, "http.server",
		// Initial span name; overwritten to route pattern by the RoutePattern
		// middleware above once chi has matched the route.
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return fmt.Sprintf("%s %s", r.Method, r.URL.Path)
		}),
		// Exclude health probes and /metrics from tracing + metrics to avoid
		// noise from k8s liveness checks and Prometheus scrapes.
		otelhttp.WithFilter(func(r *http.Request) bool {
			switch r.URL.Path {
			case "/healthz", "/readyz", "/metrics":
				return false
			}
			return true
		}),
	)

	srv := &http.Server{
		Addr:         fmt.Sprintf(":%s", cfg.Port),
		Handler:      httpHandler,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// ── Start ─────────────────────────────────────────────────────────────────
	go func() {
		slog.Info("server starting",
			"port", cfg.Port,
			"service", cfg.ServiceName,
			"version", cfg.ServiceVersion,
			"env", cfg.Environment,
			"otlp_endpoint", cfg.OTLPEndpoint,
			"pyroscope", cfg.PyroscopeServerAddress,
		)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	// ── Graceful shutdown ─────────────────────────────────────────────────────
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	slog.Info("shutdown signal received — draining in-flight requests")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("HTTP server forced to shutdown", "error", err)
	}

	profilerStop()

	// Close store connections before flushing OTel so any final
	// db_client_connections metrics are exported before shutdown.
	if db != nil {
		if err := db.Close(); err != nil {
			slog.Error("db close error", "error", err)
		}
	}
	if cache != nil {
		if err := cache.Close(); err != nil {
			slog.Error("cache close error", "error", err)
		}
	}

	if err := otelShutdown(shutdownCtx); err != nil {
		slog.Error("OTel shutdown error", "error", err)
	}

	slog.Info("server stopped cleanly")
}
