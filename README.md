# go-lgtmp — LGTMP Observability Reference Service

A production-ready Go HTTP service demonstrating **all four observability pillars**
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
| PostgreSQL | `github.com/XSAM/otelsql` | Child span per query, `db.statement`, pool metrics |
| Redis | `github.com/redis/go-redis/extra/redisotel/v9` | Child span per command, operation duration histogram |

---

## Observability Stack & OpenTelemetry Flow

```
┌─────────────────────────────────────────────────────────────────────────┐
│  Go Service  (host :8080)                                               │
│                                                                         │
│  ┌──────────────┐  OTLP gRPC   ┌──────────────────────────────────┐    │
│  │ Traces       │ ────────────► │                                  │    │
│  │ (OTel SDK)   │              │   Grafana Alloy  (:4317)          │    │
│  ├──────────────┤  OTLP gRPC   │                                  │    │
│  │ Logs         │ ────────────► │   otelcol.receiver.otlp          │    │
│  │ (slog+OTel)  │              │         │                        │    │
│  ├──────────────┤  HTTP scrape │         ▼                        │    │
│  │ Metrics /    │ ◄─────────── │   otelcol.processor.batch        │    │
│  │ metrics      │              │         │              │          │    │
│  │ (Prometheus) │              │    traces│         logs│          │    │
│  └──────────────┘              │         ▼              ▼          │    │
│                                │  otelcol.exporter  otelcol.      │    │
│  ┌──────────────┐  HTTP push   │  .otlp (Tempo)     exporter.     │    │
│  │ Profiling    │ ────────────► │                    loki          │    │
│  │ (Pyroscope   │              │                                  │    │
│  │  Go SDK)     │              │  prometheus.scrape → remote_write │    │
│  └──────────────┘              │  (Mimir)                         │    │
│                                └──────────────────────────────────┘    │
└─────────────────────────────────────────────────────────────────────────┘
           │ traces           │ logs            │ metrics    │ profiles
           ▼                  ▼                 ▼            ▼
    ┌────────────┐    ┌─────────────┐   ┌──────────┐  ┌───────────┐
    │   Tempo    │    │    Loki     │   │  Mimir   │  │Pyroscope  │
    │  :3200     │    │   :3100     │   │  :9009   │  │  :4040    │
    │            │    │             │   │          │  │           │
    │ metrics_   │───►│             │   │          │  │           │
    │ generator  │    │             │   │          │  │           │
    │ (RED+graph)│    │             │   │          │  │           │
    └─────┬──────┘    └──────┬──────┘   └────┬─────┘  └─────┬─────┘
          │ remote_write     │               │              │
          └──────────────────┘───────────────┘              │
                             │                              │
                             ▼                              │
                    ┌─────────────────┐                     │
                    │     Grafana     │ ◄───────────────────┘
                    │    :3000        │
                    │                 │
                    │ Tempo datasource│──── TraceQL search
                    │  ↳ tracesToLogs │──── jump to Loki logs
                    │  ↳ tracesToProf │──── jump to Pyroscope flamegraph
                    │  ↳ serviceMap   │──── service graph (from Mimir)
                    │                 │
                    │ Loki datasource │──── derivedFields → Tempo trace
                    └─────────────────┘
```

### Signal details

| Signal | SDK / Library | Transport | Alloy component | Backend |
|---|---|---|---|---|
| **Traces** | `go.opentelemetry.io/otel` | OTLP gRPC → Alloy :4317 | `otelcol.exporter.otlp` | Tempo |
| **Logs** | `log/slog` + `otelslog` bridge | OTLP gRPC → Alloy :4317 | `otelcol.exporter.loki` | Loki |
| **Metrics** | OTel Prometheus exporter | HTTP pull ← Alloy scrape :8080 | `prometheus.remote_write` | Mimir |
| **Profiles** | `grafana/pyroscope-go` | HTTP push → Pyroscope :4040 | _(direct, no Alloy)_ | Pyroscope |
| **Span metrics** | Tempo `metrics_generator` | remote_write | _(Tempo internal)_ | Mimir |
| **Docker logs** | Alloy `loki.source.docker` | Docker socket | `loki.write` | Loki |

### Cross-signal correlations

| From | To | How |
|---|---|---|
| Trace → Logs | Tempo → Loki | `trace_id` in OTLP log attributes; Loki `derivedFields` links back |
| Trace → Profile | Tempo → Pyroscope | `otelpyroscope` sets `profile_id` pprof label per span |
| Log → Trace | Loki → Tempo | Click trace ID in log line |
| Metrics → Traces | Mimir → Tempo | Exemplars on `http_server_request_duration_seconds` |
| Service Map | Tempo → Mimir | `metrics_generator` `service-graphs` processor |

---

## Local Development

The Go service runs on your **host machine** (for fast iteration without rebuilding images).
The full observability stack (Postgres, Redis, Alloy, Tempo, Loki, Mimir, Pyroscope, Grafana)
runs in Docker Compose.

### 1. Start the infrastructure

```bash
make infra
# or: docker compose up -d
```

Wait ~10s for all services to be healthy.

### 2. Configure the service

```bash
cp .env.example .env
# Edit .env if needed — defaults point to the docker compose services
```

### 3. Run the service

```bash
# Option A — using .env file
source .env && make run

# Option B — inline env vars
DATABASE_DSN="postgres://demo:demo@localhost:5432/demo?sslmode=disable" \
REDIS_ADDR="localhost:6379" \
make run
```

The service starts on http://localhost:8080.

### 4. Generate traffic

Open a second terminal:

```bash
make load          # continuous traffic loop — Ctrl+C to stop

# or individual requests:
curl http://localhost:8080/ping
curl http://localhost:8080/rolldice
curl "http://localhost:8080/fibonacci?n=35"
curl http://localhost:8080/db/users
curl http://localhost:8080/cache/users/1   # cache miss → DB → write to cache
curl http://localhost:8080/cache/users/1   # cache hit (X-Cache: HIT)
```

### 5. Open Grafana

http://localhost:3000 (no login required)

| Signal | Where to look |
|---|---|
| Logs | Explore → Loki → `{service_name="go-lgtmp"}` |
| Traces | Explore → Tempo → search service `go-lgtmp` |
| Metrics | Explore → Mimir → `http_server_request_duration_seconds` |
| Profiling | Explore → Pyroscope → application `go-lgtmp` |
| Trace → Profile | Open any span in Tempo → click **Profiles** tab |
| Trace → Logs | Open any span in Tempo → click **Logs** tab |
| Service Map | Tempo datasource → Service Map |

Other UIs:
- **Alloy pipeline**: http://localhost:12345
- **Tempo API**: http://localhost:3200/ready
- **Loki API**: http://localhost:3100/ready

### Stop / clean up

```bash
make infra-down
# or: docker compose down -v   (-v removes volumes / stored data)
```

---

## Running Tests

```bash
# All tests with race detector
make test
# or: go test -race -count=1 -cover ./...

# Single package
go test -race ./internal/handler/...

# With verbose output
go test -v -race ./...
```

The tests do **not** require the Docker Compose stack — they use no external dependencies.

---

## Makefile Reference

```
make infra         Start docker compose stack (Postgres, Redis, Alloy, Grafana, …)
make run           Run the Go service locally (source .env first)
make load          Send continuous demo traffic to localhost:8080
make test          Run tests with race detector
make lint          Run golangci-lint
make build         Compile binary to ./bin/server
make infra-down    Stop and remove docker compose stack + volumes
make help          Show all targets
```

---

## API Endpoints

| Endpoint | Description |
|---|---|
| `GET /ping` | Liveness check + minimal span |
| `GET /rolldice` | Random dice roll — generates error rate + latency distribution |
| `GET /fibonacci?n=<int>` | CPU-intensive — drives Pyroscope flamegraph |
| `GET /db` | PostgreSQL diagnostic `SELECT 1` |
| `GET /db/users` | List users — `SELECT` with otelsql child span |
| `POST /db/users` | Create user `{"name":"…","email":"…"}` — `INSERT` span |
| `GET /cache/users/{id}` | Cache-aside: Redis GET → miss → Postgres SELECT → Redis SET |
| `GET /metrics` | Prometheus text (Alloy scrapes this → Mimir) |
| `GET /healthz` | Liveness probe (no telemetry) |
| `GET /readyz` | Readiness probe — pings DB + Redis |

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
    ├── GET  /cache/users/{id} ← cache-aside: Redis GET → miss → Postgres → Redis SET
    │
    ├── GET  /metrics          ← Prometheus text (Alloy scrapes → Mimir)
    ├── GET  /healthz          ← liveness (no telemetry)
    └── GET  /readyz           ← readiness (pings DB + Redis)

Telemetry pipeline (Grafana Alloy):
    ├── OTLP gRPC :4317  ← receives traces + logs from service
    ├── Prometheus scrape :8080/metrics  ← pulls RED + DB pool metrics
    ├── → Tempo :3200 (traces)
    ├── → Loki :3100 (logs with trace_id correlation)
    └── → Mimir :9009 (metrics)

Pyroscope ← service pushes profiles directly, tagged with trace_id per span
```

---

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `PORT` | `8080` | HTTP listen port |
| `OTEL_SERVICE_NAME` | `go-lgtmp` | Service name in all telemetry |
| `OTEL_SERVICE_VERSION` | `0.1.0` | Service version tag |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `http://localhost:4317` | Alloy OTLP gRPC endpoint |
| `OTEL_EXPORTER_OTLP_INSECURE` | `true` | Disable TLS for local dev |
| `PYROSCOPE_SERVER_ADDRESS` | `http://localhost:4040` | Pyroscope server URL |
| `ENVIRONMENT` | `development` | Deployment environment tag |
| `LOG_LEVEL` | `info` | Log level (debug/info/warn/error) |
| `DATABASE_DSN` | _(empty)_ | PostgreSQL DSN — enables `/db/*` endpoints |
| `REDIS_ADDR` | _(empty)_ | Redis `host:port` — enables `/cache/*` endpoints |

---

## Grafana Queries

### Loki (Logs)

```logql
# All service logs
{service_name="go-lgtmp"}

# Errors only
{service_name="go-lgtmp"} | json | level="ERROR"

# Logs for a specific trace (click trace ID in Tempo to jump here automatically)
{service_name="go-lgtmp"} | json | trace_id="<trace-id>"

# Request rate by status code
sum by (status) (rate({service_name="go-lgtmp"} | json | __error__="" [1m]))
```

### Tempo (TraceQL)

```
# All spans for this service
{ resource.service.name = "go-lgtmp" }

# Slow fibonacci calls
{ resource.service.name = "go-lgtmp" && span.fibonacci.n > 30 }

# Error spans
{ resource.service.name = "go-lgtmp" } | status = error

# DB spans slower than 50ms
{ span.db.system = "postgresql" && duration > 50ms }

# Redis commands
{ span.db.system = "redis" }
```

### Mimir / Prometheus (Metrics)

```promql
# Request rate by route
sum by (http_route) (rate(http_server_request_duration_seconds_count[1m]))

# Error rate (5xx)
sum(rate(http_server_request_duration_seconds_count{http_response_status_code=~"5.."}[1m]))
/ sum(rate(http_server_request_duration_seconds_count[1m]))

# P99 latency by route
histogram_quantile(0.99, sum by (le, http_route) (
  rate(http_server_request_duration_seconds_bucket[5m])
))

# Active DB connections
db_client_connections_usage{state="used"}

# Redis operation latency P99
histogram_quantile(0.99, sum by (le, db_operation) (
  rate(db_client_operation_duration_seconds_bucket{db_system="redis"}[5m])
))

# In-flight requests
demo_active_requests
```

---

## Stack Versions

| Component | Image | Version |
|---|---|---|
| Tempo | `grafana/tempo` | 2.10.1 |
| Loki | `grafana/loki` | 3.6.7 |
| Mimir | `grafana/mimir` | 2.17.7 |
| Pyroscope | `grafana/pyroscope` | 1.18.1 |
| Alloy | `grafana/alloy` | v1.13.2 |
| Grafana | `grafana/grafana` | 12.4.0 |
| PostgreSQL | `postgres` | 16.13-alpine |
| Redis | `redis` | 7.4.8-alpine |

---

## OTel Wiring Pattern

### 1. Init telemetry

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
httpHandler := otelhttp.NewHandler(r, "http.server",
    otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
        return r.Method + " " + r.URL.Path
    }),
    otelhttp.WithFilter(func(r *http.Request) bool {
        switch r.URL.Path {
        case "/healthz", "/readyz", "/metrics":
            return false  // exclude health probes from tracing
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

### 4. PostgreSQL tracing

```go
// otelsql.Open wraps database/sql: every query creates a child span with
// db.system, db.statement, db.operation, server.address, server.port
db, err := otelsql.Open("pgx", dsn,
    otelsql.WithAttributes(semconv.DBSystemPostgreSQL),
)
// Pool stats → Prometheus: db_client_connections_usage, _max, _wait_duration
otelsql.RegisterDBStatsMetrics(db, otelsql.WithAttributes(semconv.DBSystemPostgreSQL))
```

### 5. Redis tracing

```go
client := redis.NewClient(&redis.Options{Addr: addr})
redisotel.InstrumentTracing(client)   // child span per command
redisotel.InstrumentMetrics(client)   // db_client_operation_duration_seconds histogram
```

### 6. Trace ↔ Profile linking

```go
// Wrap TracerProvider: every span start/end sets pprof labels on the goroutine.
// Pyroscope captures CPU samples with trace_id/span_id labels.
// In Grafana: open a span in Tempo → click Profiles tab.
otel.SetTracerProvider(otelpyroscope.NewTracerProvider(tp))
```

---

## File Structure

```
go-lgtmp/
├── cmd/server/main.go                   # Entry point — wires telemetry, stores, router, graceful shutdown
├── internal/
│   ├── config/config.go                 # Env-based config
│   ├── telemetry/
│   │   ├── otel.go                      # OTel SDK init — traces + metrics + logs + otelpyroscope
│   │   └── profiler.go                  # Pyroscope init
│   ├── store/
│   │   ├── db.go                        # PostgreSQL with otelsql
│   │   └── cache.go                     # Redis with redisotel
│   ├── handler/
│   │   ├── handler.go                   # Demo handlers: ping, rolldice, fibonacci
│   │   ├── store_handler.go             # DB + cache handlers
│   │   └── health.go                    # /healthz /readyz
│   └── middleware/
│       └── middleware.go                # Logging middleware with trace_id/span_id
├── observability/
│   ├── alloy/config.alloy               # Alloy pipeline: OTLP → Tempo/Loki, scrape → Mimir, Docker logs → Loki
│   ├── grafana/provisioning/            # Auto-provisioned datasources + APM dashboard
│   ├── tempo/tempo.yaml                 # Tempo single-node config
│   ├── loki/loki.yaml                   # Loki config
│   └── mimir/mimir.yaml                 # Mimir config
├── docker-compose.yml                   # Full local dev stack
├── k8s/                                 # Kubernetes manifests
├── .github/workflows/ci.yml             # CI: lint + test + tag + publish image
├── Dockerfile
├── Makefile
└── go.mod
```
