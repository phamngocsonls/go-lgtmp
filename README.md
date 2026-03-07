# go-lgtmp — LGTMP Observability Reference Service

> **For AI agents**: This README is the authoritative guide for implementing full
> LGTMP observability in a Go service. Follow the patterns in this file exactly.
> Every section includes copy-paste code blocks and an implementation checklist.

A production-ready Go HTTP service that demonstrates **all four observability pillars**
with the Grafana LGTMP stack:

| Pillar | Technology | Destination |
|---|---|---|
| Logs | `log/slog` + OTel log bridge | Loki (via Alloy OTLP) |
| Traces | OpenTelemetry SDK + OTLP gRPC | Tempo (via Alloy) |
| Metrics | OTel Prometheus exporter | Mimir (via Alloy scrape) |
| Profiling | Pyroscope Go SDK + otelpyroscope | Pyroscope (direct push) |

Also demonstrates **DB + cache instrumentation**:

| Dependency | Library | What's auto-instrumented |
|---|---|---|
| PostgreSQL | `github.com/XSAM/otelsql` | Child span per query with `db.statement`, `db.operation`; pool metrics |
| Redis | `github.com/redis/go-redis/extra/redisotel/v9` | Child span per command with `db.statement`; operation duration histogram |

---

## Quick Start

```bash
# Start infrastructure (PostgreSQL, Redis, Alloy, Tempo, Loki, Mimir, Pyroscope, Grafana)
docker compose up -d

# Run the service (uses env defaults pointing to docker compose services)
DATABASE_DSN="postgres://demo:demo@localhost:5432/demo?sslmode=disable" \
REDIS_ADDR="localhost:6379" \
go run ./cmd/server

# Generate telemetry
curl http://localhost:8080/ping
curl http://localhost:8080/rolldice
curl "http://localhost:8080/fibonacci?n=35"
curl http://localhost:8080/db
curl http://localhost:8080/db/users
curl -X POST http://localhost:8080/db/users \
     -H 'Content-Type: application/json' \
     -d '{"name":"Alice","email":"alice@example.com"}'
curl http://localhost:8080/cache/users/1    # cache miss → DB → write to cache
curl http://localhost:8080/cache/users/1    # cache hit (X-Cache: HIT)
```

Open Grafana at http://localhost:3000 and explore:
- **Logs**: Loki → `{service_name="go-lgtmp"}`
- **Traces**: Tempo → search by service `go-lgtmp`
- **Metrics**: Mimir → `http_server_request_duration_seconds`
- **Profiling**: Pyroscope → application `go-lgtmp`
- **Trace → Profile**: open any span in Tempo, click "Profiles" to see the flamegraph

---

## Architecture

```
HTTP Client
    │
    ▼
otelhttp.NewHandler           ← auto-instruments: span per request, RED metrics, W3C propagation
    │
    ▼
chi Router
    ├── middleware.Logging     ← slog with trace_id + span_id (log → trace correlation)
    │
    ├── GET  /ping             ← minimal span + log example
    ├── GET  /rolldice         ← error rate + latency distribution demo
    ├── GET  /fibonacci        ← CPU burn → Pyroscope flamegraph + trace-profile link
    │
    ├── GET  /db               ← PostgreSQL: diagnostic SELECT (otelsql child span)
    ├── GET  /db/users         ← PostgreSQL: SELECT users (otelsql child span)
    ├── POST /db/users         ← PostgreSQL: INSERT user (otelsql child span)
    │
    ├── POST /cache            ← Redis: SET key (redisotel child span)
    ├── GET  /cache/users/{id} ← cache-aside: Redis GET → miss → Postgres → Redis SET
    │
    ├── GET  /metrics          ← Prometheus text (Alloy scrapes → Mimir)
    ├── GET  /healthz          ← liveness (no telemetry)
    └── GET  /readyz           ← readiness (no telemetry)

Telemetry pipeline (Alloy):
    ├── OTLP gRPC :4317  ← receives traces + logs from service
    ├── Prometheus scrape :8080/metrics  ← pulls RED + DB pool metrics
    ├── → Tempo (traces)
    ├── → Loki (logs with trace_id correlation)
    └── → Mimir (metrics)

Pyroscope ← service pushes profiles directly, tagged with trace_id per span
```

---

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `PORT` | `8080` | HTTP listen port |
| `OTEL_SERVICE_NAME` | `go-lgtmp` | Service name in all telemetry |
| `OTEL_SERVICE_VERSION` | `0.1.0` | Service version tag |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `http://localhost:4317` | Alloy/Collector OTLP gRPC endpoint |
| `OTEL_EXPORTER_OTLP_INSECURE` | `true` | Disable TLS for local dev |
| `PYROSCOPE_SERVER_ADDRESS` | `http://localhost:4040` | Pyroscope server URL |
| `ENVIRONMENT` | `development` | Deployment environment tag |
| `LOG_LEVEL` | `info` | Log level (debug/info/warn/error) |
| `DATABASE_DSN` | _(empty)_ | PostgreSQL DSN — enables `/db/*` endpoints |
| `REDIS_ADDR` | _(empty)_ | Redis `host:port` — enables `/cache/*` endpoints |

---

## OTel Wiring Pattern

### 1. Init telemetry — `internal/telemetry/otel.go`

```go
// In main():
shutdown, err := telemetry.InitOTel(ctx, cfg)
if err != nil { log.Fatal(err) }
defer shutdown(ctx)

// InitOTel does:
// 1. Build resource (service.name, service.version, deployment.environment)
// 2. Trace provider: OTLP gRPC exporter → BatchSpanProcessor → global TracerProvider
//    wrapped with otelpyroscope.NewTracerProvider for trace-profile linking
// 3. Metric provider: Prometheus exporter → global MeterProvider (serves /metrics)
// 4. Log provider: OTLP gRPC exporter → global LoggerProvider
// 5. slog bridge: otelslog.NewHandler → slog.SetDefault (+ JSON stdout fan-out)
// 6. Set global propagator: W3C TraceContext + Baggage
```

### 2. RED metrics — zero boilerplate via `otelhttp`

```go
// Wrap the entire chi router — every route gets a histogram automatically:
//   http_server_request_duration_seconds{
//     http_request_method, http_route, http_response_status_code, ...}
//
// RED queries:
//   Rate:     rate(http_server_request_duration_seconds_count{http_route="/rolldice"}[1m])
//   Errors:   rate(...{http_response_status_code=~"5.."}[1m])
//   Duration: histogram_quantile(0.99, rate(..._bucket{http_route="/rolldice"}[5m]))
httpHandler := otelhttp.NewHandler(r, "http.server",
    otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
        return r.Method + " " + r.URL.Path
    }),
    otelhttp.WithFilter(func(r *http.Request) bool {
        // Exclude health probes + /metrics from tracing to avoid noise
        switch r.URL.Path {
        case "/healthz", "/readyz", "/metrics":
            return false
        }
        return true
    }),
)
```

### 3. Spans in handlers

```go
var tracer = otel.Tracer("your-package/name")

func (h *Handler) MyHandler(w http.ResponseWriter, r *http.Request) {
    ctx, span := tracer.Start(r.Context(), "operation_name")
    defer span.End()

    span.SetAttributes(attribute.String("key", "value"))

    if err != nil {
        span.RecordError(err)
        span.SetStatus(codes.Error, err.Error())
    }

    // Always use slog.*Context(ctx, ...) — ctx carries the active span,
    // so the OTel log bridge attaches trace_id/span_id automatically.
    slog.InfoContext(ctx, "event", "key", "value")
}
```

### 4. PostgreSQL tracing — `internal/store/db.go`

```go
import (
    "github.com/XSAM/otelsql"
    _ "github.com/jackc/pgx/v5/stdlib"   // "pgx" driver
    semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// otelsql.Open wraps database/sql: every QueryContext/ExecContext/BeginTx
// creates a child span automatically with:
//   db.system    = "postgresql"
//   db.statement = "SELECT ..."      (the full SQL)
//   db.operation = "SELECT"
//   server.address, server.port
db, err := otelsql.Open("pgx", dsn,
    otelsql.WithAttributes(semconv.DBSystemPostgreSQL),
    otelsql.WithSpanOptions(otelsql.SpanOptions{
        DisableQuery: false,  // set true in prod if SQL contains PII
        RecordError:  func(err error) bool { return err != nil },
    }),
)

// Pool stats → Prometheus: db_client_connections_usage, _max, _wait_duration
statsReg, err := otelsql.RegisterDBStatsMetrics(db,
    otelsql.WithAttributes(semconv.DBSystemPostgreSQL),
)
defer statsReg.Unregister()
```

**Trace tree for `GET /db/users`:**
```
[HTTP GET /db/users]          ← otelhttp (http_server_request_duration_seconds)
  └─ [list_users]             ← tracer.Start() in handler
       └─ [db.query]          ← otelsql AUTO — no code needed
            db.system    = postgresql
            db.statement = SELECT id, name, email, ...
            db.operation = SELECT
```

### 5. Redis tracing — `internal/store/cache.go`

```go
import (
    "github.com/redis/go-redis/extra/redisotel/v9"
    "github.com/redis/go-redis/v9"
)

client := redis.NewClient(&redis.Options{Addr: addr})

// Adds a hook: every command creates a child span with:
//   db.system    = "redis"
//   db.statement = "set go-lgtmp:user:1 <value> ex 60"
//   server.address, server.port
redisotel.InstrumentTracing(client)

// Adds db_client_operation_duration_seconds histogram to global MeterProvider
redisotel.InstrumentMetrics(client)
```

**Trace tree for `GET /cache/users/1` (cache miss):**
```
[HTTP GET /cache/users/1]     ← otelhttp
  └─ [cache_get_user]         ← tracer.Start() in handler
       ├─ [redis GET]         ← redisotel AUTO — cache miss
       │    db.system    = redis
       │    db.statement = get go-lgtmp:user:1
       ├─ [db.query]          ← otelsql AUTO — fallback SELECT by ID
       │    db.statement = SELECT ... WHERE id = $1
       └─ [redis SET]         ← redisotel AUTO — write through
            db.statement = set go-lgtmp:user:1 ... ex 60
```

### 6. Trace ↔ Profile linking — `internal/telemetry/otel.go`

```go
import otelpyroscope "github.com/grafana/otel-profiling-go"

// Wrap the TracerProvider: every span start/end sets pprof labels
// (profile_id, span_id, trace_id, span_name) on the current goroutine.
// Pyroscope captures CPU samples with these labels, so Grafana Tempo
// can open the matching flamegraph directly from any span.
otel.SetTracerProvider(otelpyroscope.NewTracerProvider(tp))
```

In Grafana: open a trace in Tempo → click any span → click **"Profiles"** tab.

### 7. Trace → Log correlation — `internal/middleware/middleware.go`

```go
// slog.Log(r.Context(), ...) — ctx carries the active span.
// The otelslog bridge extracts trace_id/span_id from ctx and attaches them
// to the OTLP log record automatically, so Loki can link to Tempo.
//
// For the stdout JSON handler, inject manually:
spanCtx := trace.SpanFromContext(r.Context()).SpanContext()
if spanCtx.IsValid() {
    slog.InfoContext(ctx, "event",
        "trace_id", spanCtx.TraceID().String(),
        "span_id",  spanCtx.SpanID().String(),
    )
}
```

LogQL to jump from log to trace:
```logql
{service_name="go-lgtmp"} | json | trace_id="<your-trace-id>"
```

### 8. Custom metrics

```go
meter := otel.Meter("your-service/name")

// Counter
requests, _ := meter.Int64Counter("myapp_requests_total",
    metric.WithDescription("Total requests"),
    metric.WithUnit("{request}"),
)
requests.Add(ctx, 1, metric.WithAttributes(attribute.String("route", "/foo")))

// Histogram
duration, _ := meter.Float64Histogram("myapp_request_duration_seconds",
    metric.WithUnit("s"),
    metric.WithExplicitBucketBoundaries(0.005, 0.01, 0.05, 0.1, 0.5, 1, 2.5),
)
duration.Record(ctx, elapsed.Seconds())

// Gauge (UpDownCounter)
active, _ := meter.Int64UpDownCounter("myapp_active_requests")
active.Add(ctx, 1)
defer active.Add(ctx, -1)
```

### 9. Profiling — `internal/telemetry/profiler.go`

```go
// In main():
stop, err := telemetry.InitProfiler(cfg)
if err != nil { slog.Warn("profiler unavailable", "error", err) }
defer stop()

// InitProfiler calls pyroscope.Start() with all profile types:
// ProfileCPU, ProfileInuseObjects, ProfileAllocObjects,
// ProfileInuseSpace, ProfileAllocSpace, ProfileGoroutines,
// ProfileMutexCount/Duration, ProfileBlockCount/Duration
//
// otelpyroscope (wired in InitOTel) tags each goroutine with trace_id
// so profiles are automatically linked to Tempo spans.
```

---

## HTTP Middleware Pattern

```go
// Mount order matters:
r := chi.NewRouter()
r.Use(chimiddleware.Recoverer)  // 1. panic recovery (outermost)

// Wrap router with OTel BEFORE routes so spans exist when middleware runs:
httpHandler := otelhttp.NewHandler(r, "http.server", ...)

// Inside the router, logging middleware reads the already-active span:
r.Use(middleware.Logging)  // reads span → injects trace_id in logs
```

---

## Grafana Queries

### Loki (Logs)

```logql
# All logs for this service
{service_name="go-lgtmp"}

# Error logs only
{service_name="go-lgtmp"} | json | level="ERROR"

# Logs for a specific trace (log → trace correlation)
{service_name="go-lgtmp"} | json | trace_id="abc123..."

# Request rate by status code
sum by (status) (rate({service_name="go-lgtmp"} | json | __error__="" [1m]))
```

### Tempo (Traces)

```
# TraceQL — slow fibonacci calls
{ .service.name = "go-lgtmp" && .fibonacci.n > 30 }

# Error spans
{ .service.name = "go-lgtmp" } | status = error

# DB spans slower than 50ms
{ .db.system = "postgresql" && duration > 50ms }

# Cache misses
{ .cache.hit = false }

# Redis commands
{ .db.system = "redis" }
```

### Mimir / Prometheus (Metrics)

```promql
# ── RED metrics (from otelhttp, zero boilerplate) ──────────────────────────

# Request rate by route
sum by (http_route) (
  rate(http_server_request_duration_seconds_count[1m])
)

# Error rate (5xx)
sum(rate(http_server_request_duration_seconds_count{http_response_status_code=~"5.."}[1m]))
/ sum(rate(http_server_request_duration_seconds_count[1m]))

# P99 latency by route
histogram_quantile(0.99, sum by (le, http_route) (
  rate(http_server_request_duration_seconds_bucket[5m])
))

# ── Database pool metrics (from otelsql.RegisterDBStatsMetrics) ─────────────

# Active connections
db_client_connections_usage{state="used"}

# Idle connections
db_client_connections_usage{state="idle"}

# Max pool size
db_client_connections_max

# ── Redis operation metrics (from redisotel.InstrumentMetrics) ─────────────

# Redis operation latency P99
histogram_quantile(0.99, sum by (le, db_operation) (
  rate(db_client_operation_duration_seconds_bucket{db_system="redis"}[5m])
))

# ── Domain metrics ──────────────────────────────────────────────────────────

# In-flight requests
demo_active_requests

# Fibonacci computations by n value
sum by (n) (rate(demo_fibonacci_computed_total[1m]))
```

---

## Implementation Checklist (for agents)

Copy this checklist when implementing LGTMP observability in a new Go service:

- [ ] `go get go.opentelemetry.io/otel go.opentelemetry.io/otel/sdk ...`
- [ ] Copy `internal/telemetry/otel.go` → call `InitOTel(ctx, cfg)` in main
- [ ] Copy `internal/telemetry/profiler.go` → call `InitProfiler(cfg)` in main
- [ ] Copy `internal/config/config.go` → add service-specific config fields
- [ ] Copy `internal/middleware/middleware.go` → mount after `otelhttp.NewHandler`
- [ ] Wrap router: `otelhttp.NewHandler(router, "http.server", ...)`
- [ ] Add RoutePattern middleware to stamp `http_route` label (prevents cardinality explosion from path params)
- [ ] Add `/metrics` endpoint: `r.Handle("/metrics", promhttp.Handler())`
- [ ] Add `/healthz` and `/readyz` endpoints; add them to `otelhttp.WithFilter`
- [ ] In each handler: `tracer.Start(r.Context(), "operation")` + `defer span.End()`
- [ ] Always use `slog.*Context(ctx, ...)` — never `slog.Info` (loses trace correlation)
- [ ] For PostgreSQL: `otelsql.Open("pgx", dsn, ...)` + `RegisterDBStatsMetrics`
- [ ] For Redis: `redisotel.InstrumentTracing(client)` + `InstrumentMetrics`
- [ ] Set `OTEL_SERVICE_NAME`, `DATABASE_DSN`, `REDIS_ADDR` env vars
- [ ] Configure Alloy to scrape `/metrics` and receive OTLP on :4317

---

## File Structure Reference

```
go-lgtmp/
├── cmd/server/main.go           # Entry point — wires telemetry + stores, builds router, graceful shutdown
├── internal/
│   ├── config/config.go         # Env-based config (copy this pattern)
│   ├── telemetry/
│   │   ├── otel.go              # OTel SDK init — traces + metrics + logs + otelpyroscope (copy this)
│   │   └── profiler.go          # Pyroscope init (copy this)
│   ├── store/
│   │   ├── db.go                # PostgreSQL with otelsql — auto-instrumented spans + pool metrics
│   │   └── cache.go             # Redis with redisotel — auto-instrumented spans + op metrics
│   ├── handler/
│   │   ├── handler.go           # Demo handlers: ping, rolldice, fibonacci
│   │   ├── store_handler.go     # DB + cache handlers: /db/*, /cache/*
│   │   └── health.go            # /healthz /readyz (no telemetry noise)
│   └── middleware/
│       └── middleware.go        # Logging middleware with trace_id/span_id correlation
├── docker-compose.yml           # Full local dev stack (Postgres, Redis, Alloy, Tempo, Loki, Mimir, Pyroscope, Grafana)
├── k8s/
│   ├── configmap.yaml
│   ├── deployment.yaml
│   └── service.yaml
├── .github/workflows/ci.yml
├── Dockerfile
├── Makefile
└── go.mod
```
