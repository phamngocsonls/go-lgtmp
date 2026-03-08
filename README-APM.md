# APM Enterprise → LGTMP Migration Roadmap

> Migration guide from **Datadog APM** or **New Relic APM** to the open-source
> **LGTMP stack** (Loki · Grafana · Tempo · Mimir · Pyroscope) on Kubernetes.
>
> The Go service in this repository (`go-lgtmp`) is the reference implementation
> for instrumented applications after migration.

---

## Why Migrate?

| | Datadog / New Relic | LGTMP |
|---|---|---|
| **Cost** | $23–$40/host/month + ingestion fees | Compute + storage only (~$2–5/host/month) |
| **Data ownership** | Vendor cloud | Your own cluster / S3 |
| **Vendor lock-in** | Proprietary agents & SDK | OpenTelemetry standard |
| **Cardinality limits** | Hard limits on metric cardinality | Self-controlled |
| **Retention** | 15 days default (expensive to extend) | Define your own per-signal |
| **Correlation** | Siloed (APM ↔ Logs ↔ Infra separate products) | Native cross-signal in Grafana |

**Typical savings: 70–80% reduction in observability costs.**

---

## Feature Mapping

| Datadog | New Relic | LGTMP Equivalent | Notes |
|---|---|---|---|
| Datadog Agent (DaemonSet) | Infrastructure Agent | **Grafana Alloy** (DaemonSet) | Collects all signals |
| APM + Distributed Tracing | APM / Distributed Tracing | **Tempo** via OpenTelemetry | Swap SDK, not just config |
| Metrics / DogStatsD | Metrics API | **Mimir** via Prometheus | |
| Log Management | Log Management | **Loki** | Alloy tails pod logs |
| Continuous Profiler | CodeStream / N/A | **Pyroscope** | |
| Dashboards | Dashboards | **Grafana** | Redesign, don't 1:1 copy |
| Monitors / Alerts | Alerts | **Grafana Alerting** + Alertmanager | |
| Service Map | Distributed Tracing Map | **Grafana Service Graph** (Tempo) | |
| Infrastructure | Infrastructure | **kube-state-metrics** + **node-exporter** | |
| Container Monitoring | Kubernetes | **cAdvisor** + kubelet `/metrics` | |
| Synthetics | Synthetic Monitoring | **Grafana Synthetic Monitoring** / Blackbox Exporter | |
| RUM | Browser Agent | **Grafana Faro** (OpenTelemetry) | |
| Error Tracking | Errors Inbox | **Loki** + Grafana Alert on `level=error` | |
| SLOs | Service Levels | **Grafana SLO plugin** | |
| Audit Logs | Audit Log | Alloy → Loki | |

---

## Architecture: Before vs After

### Before (Datadog / New Relic)

```
K8s Node
  ├── Datadog Agent (DaemonSet)
  │     ├── Pulls container logs → DD Log Management
  │     ├── Scrapes metrics → DD Metrics
  │     └── Receives traces from dd-trace SDK → DD APM
  └── App Pod
        └── dd-trace-go SDK  ──────────────────→  Datadog Cloud
              (proprietary format, vendor lock-in)
```

### After (LGTMP)

```
K8s Node
  ├── Grafana Alloy (DaemonSet)                 ← replaces DD Agent
  │     ├── Tails pod logs ──────────────────→  Loki
  │     ├── Receives OTLP traces ─────────────→  Tempo
  │     ├── Scrapes /metrics endpoints ───────→  Mimir
  │     └── Scrapes kube-state/node-exporter ─→  Mimir
  └── App Pod
        └── OpenTelemetry SDK (standard)
              ├── Traces  ──────────────────────→  Alloy → Tempo
              ├── Metrics ──────────────────────→  /metrics → Alloy → Mimir
              └── Logs    ──────────────────────→  stdout + OTLP → Alloy → Loki

Pyroscope ←── Pyroscope SDK (push from app)

Grafana  ←── All signals correlated via trace_id / exemplars
```

---

## Roadmap

### Phase 0 — Assessment (2–4 weeks)

**Goal**: Know exactly what you have before you remove anything.

#### Tasks
- [ ] **Inventory current APM usage**
  - Export all active Datadog/New Relic dashboards (screenshot + JSON export)
  - List all alert rules with thresholds and notification channels
  - Document current data retention settings
  - Identify business-critical SLOs and SLAs

- [ ] **Audit instrumentation**
  - List every service using `dd-trace-*` / `newrelic-*` SDK
  - Identify auto-instrumented vs manually instrumented services
  - Flag services with custom metrics (DogStatsD / NR Custom Events)
  - Identify any Datadog Log Parsing Rules → must be converted to Loki pipeline stages

- [ ] **Define success criteria**
  - All P0 alerts recreated and firing correctly
  - Zero observability gap window during cutover
  - All team members trained on Grafana

- [ ] **Capacity planning**
  - Estimate trace/log/metric volume (check DD/NR ingestion stats)
  - Size storage backends (Loki: ~0.5x DD compressed; Tempo: ~0.3x DD)
  - Plan object storage (S3/GCS) buckets for each signal

---

### Phase 1 — Deploy LGTMP Stack on K8s (4–6 weeks)

**Goal**: Running LGTMP stack, data flowing, basic dashboards working.

#### 1.1 — Install Helm charts

```bash
helm repo add grafana https://grafana.github.io/helm-charts
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm repo update
```

| Component | Helm Chart | Notes |
|---|---|---|
| Grafana | `grafana/grafana` | Or use `kube-prometheus-stack` which bundles it |
| Loki | `grafana/loki` | Use `loki-distributed` for production scale |
| Tempo | `grafana/tempo` | Use `tempo-distributed` for > 1k traces/sec |
| Mimir | `grafana/mimir-distributed` | Use `mimir-distributed` for HA |
| Pyroscope | `grafana/pyroscope` | |
| Alloy | `grafana/alloy` | Deploy as DaemonSet |
| kube-prometheus-stack | `prometheus-community/kube-prometheus-stack` | Bundles: Prometheus, kube-state-metrics, node-exporter, Alertmanager |

```bash
# Example: production namespace
kubectl create namespace monitoring

helm install lgtmp-loki grafana/loki \
  --namespace monitoring \
  --set loki.storage.type=s3 \
  --set loki.storage.s3.bucketnames=loki-chunks

helm install lgtmp-tempo grafana/tempo \
  --namespace monitoring \
  --set storage.trace.backend=s3 \
  --set storage.trace.s3.bucket=tempo-traces

helm install lgtmp-mimir grafana/mimir-distributed \
  --namespace monitoring

helm install lgtmp-pyroscope grafana/pyroscope \
  --namespace monitoring

helm install lgtmp-alloy grafana/alloy \
  --namespace monitoring \
  --set controller.type=daemonset
```

#### 1.2 — Configure Alloy (replaces DD Agent)

Alloy config (`observability/alloy/config.alloy` in this repo) handles:

```hcl
// Collect all pod logs from the node
loki.source.kubernetes "pods" {
  targets    = discovery.kubernetes.pods.targets
  forward_to = [loki.write.default.receiver]
}

// Receive OTLP traces and logs from apps
otelcol.receiver.otlp "default" {
  grpc { endpoint = "0.0.0.0:4317" }
  http { endpoint = "0.0.0.0:4318" }
  output {
    traces  = [otelcol.exporter.otlphttp.tempo.input]
    logs    = [otelcol.exporter.loki.default.input]
    metrics = [otelcol.exporter.prometheus.default.input]
  }
}

// Scrape Prometheus /metrics endpoints
prometheus.scrape "pods" {
  targets    = discovery.kubernetes.pods.targets
  forward_to = [prometheus.remote_write.mimir.receiver]
}
```

#### 1.3 — Configure Grafana datasources

```yaml
# observability/grafana/provisioning/datasources/datasources.yaml
datasources:
  - name: Loki
    type: loki
    url: http://loki-gateway.monitoring.svc.cluster.local
    jsonData:
      derivedFields:
        - name: TraceID
          matcherRegex: '"trace_id":"(\w+)"'
          url: '$${__value.raw}'
          datasourceUid: tempo   # Loki → Tempo link

  - name: Tempo
    type: tempo
    url: http://tempo-query-frontend.monitoring.svc.cluster.local:3100
    jsonData:
      lokiSearch: { datasourceUid: loki }     # Tempo → Loki link
      serviceMap:  { datasourceUid: prometheus }

  - name: Mimir
    type: prometheus
    url: http://mimir-nginx.monitoring.svc.cluster.local/prometheus

  - name: Pyroscope
    type: grafana-pyroscope-datasource
    url: http://pyroscope.monitoring.svc.cluster.local:4040
```

#### 1.4 — Tasks checklist
- [ ] All 5 backends running and healthy
- [ ] Grafana accessible, all datasources green
- [ ] Test log ingestion: `kubectl logs -n monitoring` flowing to Loki
- [ ] Test trace ingestion: send a test OTLP trace via `grpcurl`
- [ ] Prometheus scrape targets showing in Grafana Explore
- [ ] S3 buckets configured for long-term retention

---

### Phase 2 — K8s Infrastructure Observability (3–4 weeks)

**Goal**: Replace Datadog Infrastructure / New Relic Infrastructure with full K8s visibility.

#### 2.1 — K8s Infrastructure coverage map

| Datadog Check | New Relic Integration | LGTMP Source | Metrics |
|---|---|---|---|
| Kubernetes | Kubernetes | kube-state-metrics | pod/node/deployment/pvc state |
| System | Infrastructure | node-exporter | CPU, memory, disk, network |
| Container | Containers | cAdvisor (kubelet) | container resource usage |
| Kubernetes Events | K8s Events | k8s-events-exporter → Loki | pod OOM, crashloop, etc. |
| Kubernetes Audit | Audit Log | Alloy → Loki | API server audit events |
| Network Performance | Network | Hubble / Cilium metrics | L3/L4 flow metrics |

#### 2.2 — Essential Grafana dashboards to import

```
Grafana Dashboard IDs (grafana.com/grafana/dashboards/):
  - 15759  Kubernetes / Views / Global
  - 15760  Kubernetes / Views / Namespaces
  - 15761  Kubernetes / Views / Nodes
  - 15762  Kubernetes / Views / Pods
  - 3119   Kubernetes cluster monitoring
  - 1860   Node Exporter Full
  - 13332  kube-state-metrics
```

#### 2.3 — Migrate K8s alerts

```yaml
# Example: CrashLoopBackOff alert (Grafana Alerting)
apiVersion: 1
groups:
  - name: kubernetes
    rules:
      - title: Pod CrashLoopBackOff
        condition: C
        data:
          - refId: A
            expr: |
              kube_pod_container_status_waiting_reason{reason="CrashLoopBackOff"} == 1
        for: 5m
        labels:
          severity: critical
        annotations:
          summary: "Pod {{ $labels.pod }} is in CrashLoopBackOff"
```

#### 2.4 — Tasks checklist
- [ ] kube-state-metrics scraping all namespaces
- [ ] node-exporter running on every node
- [ ] cAdvisor metrics flowing to Mimir
- [ ] Kubernetes Events → Loki pipeline active
- [ ] Top-5 critical K8s alerts recreated in Grafana Alerting
- [ ] Alertmanager connected to PagerDuty / Slack
- [ ] Grafana K8s dashboards imported and verified

---

### Phase 3 — Application Instrumentation Migration (6–12 weeks)

**Goal**: Remove all Datadog/New Relic SDKs from apps. Replace with OpenTelemetry.

#### 3.1 — SDK migration per language

| Language | Remove | Add |
|---|---|---|
| **Go** | `gopkg.in/DataDog/dd-trace-go.v1` | `go.opentelemetry.io/otel` |
| **Java** | `dd-java-agent.jar` | `opentelemetry-javaagent.jar` |
| **Python** | `ddtrace` / `newrelic` | `opentelemetry-sdk` |
| **Node.js** | `dd-trace` / `newrelic` | `@opentelemetry/sdk-node` |
| **.NET** | `Datadog.Trace` | `OpenTelemetry.Sdk` |

#### 3.2 — Go migration pattern (use this repo as template)

```diff
- import "gopkg.in/DataDog/dd-trace-go.v1/ddtrace/tracer"
- import "gopkg.in/DataDog/dd-trace-go.v1/contrib/net/http/httptrace"
+ import "go.opentelemetry.io/otel"
+ import "go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

  func main() {
-     tracer.Start(tracer.WithService("my-service"))
-     defer tracer.Stop()
+     shutdown, _ := telemetry.InitOTel(ctx, cfg)  // copy from go-lgtmp
+     defer shutdown(ctx)
  }

  // HTTP handler
- mux.Handle("/api", httptrace.WrapHandler(handler, "api"))
+ mux.Handle("/api", otelhttp.NewHandler(handler, "api"))
```

Copy these files from `go-lgtmp` to any new Go service:
```
internal/telemetry/otel.go       # OTel SDK init (traces + metrics + logs)
internal/telemetry/profiler.go   # Pyroscope profiler
internal/middleware/middleware.go # HTTP logging with trace correlation
internal/config/config.go        # Env-based config pattern
```

Required env vars (add to K8s ConfigMap):
```yaml
OTEL_SERVICE_NAME: "your-service"
OTEL_EXPORTER_OTLP_ENDPOINT: "http://alloy.monitoring.svc.cluster.local:4317"
OTEL_EXPORTER_OTLP_INSECURE: "true"
PYROSCOPE_SERVER_ADDRESS: "http://pyroscope.monitoring.svc.cluster.local:4040"
```

#### 3.3 — Migrate custom metrics (DogStatsD → OTel)

```diff
- import "github.com/DataDog/datadog-go/v5/statsd"
- client.Count("orders.created", 1, []string{"region:us"}, 1)

+ meter := otel.Meter("your-service")
+ counter, _ := meter.Int64Counter("orders_created",
+     metric.WithDescription("Orders placed"))
+ counter.Add(ctx, 1, metric.WithAttributes(
+     attribute.String("region", "us")))
```

Metric naming: Datadog uses `.` separators (`orders.created`). Prometheus uses `_` (`orders_created`). **Update dashboards and alerts accordingly.**

#### 3.4 — Migrate log parsing rules

Datadog Log Parsing rules (Grok) → Alloy Loki pipeline stages:

```hcl
# Alloy: parse structured JSON logs from apps
stage.json {
  expressions = {
    level     = "level",
    trace_id  = "trace_id",
    span_id   = "span_id",
    message   = "msg",
  }
}
stage.labels {
  values = {
    level    = null,
    trace_id = null,
  }
}
```

#### 3.5 — Service migration priority order

Migrate in this order to minimise risk:

1. **Non-critical / internal tools** — lowest blast radius, team learns OTel
2. **Backend services with high observability value** — most gain
3. **Critical path services** — run parallel (both DD/NR and OTel) for 2+ weeks
4. **Gateway / ingress** — last, after all downstream services are migrated

#### 3.6 — Tasks checklist (per service)
- [ ] Remove Datadog/New Relic SDK dependency
- [ ] Add OpenTelemetry SDK (follow go-lgtmp pattern)
- [ ] Add `OTEL_SERVICE_NAME` env var to K8s ConfigMap
- [ ] Verify traces appear in Tempo
- [ ] Verify metrics appear in Mimir `/metrics`
- [ ] Verify structured logs appear in Loki with `trace_id` field
- [ ] Add Pyroscope SDK for profiling
- [ ] Update service's K8s Deployment annotations for Prometheus scrape

---

### Phase 4 — Dashboards & Alerting Migration (4–6 weeks)

**Goal**: All operational dashboards and alerts running in Grafana.

#### 4.1 — Dashboard migration strategy

> **Do not 1:1 copy dashboards.** Datadog/New Relic dashboards are optimised
> for their own query language. Redesign them using Grafana best practices.

| Dashboard Type | Tool | Notes |
|---|---|---|
| Service RED (Rate, Errors, Duration) | Grafana + Mimir | Use `http_server_request_duration_seconds` from otelhttp |
| Service Map | Grafana + Tempo | Enable Service Graph in Tempo config |
| Logs Explorer | Grafana + Loki | LogQL is more powerful than DD Log search |
| Traces Explorer | Grafana + Tempo | TraceQL for trace search |
| K8s Infrastructure | Grafana + Mimir | Import community dashboards (IDs above) |
| Profiling Flamegraph | Grafana + Pyroscope | Native Pyroscope panel |
| SLO Dashboard | Grafana SLO plugin | Define error budget burn rate alerts |

#### 4.2 — Alert migration

Datadog Monitor → Grafana Alert Rule translation:

```
# Datadog Monitor (JSON export)
{
  "query": "avg(last_5m):avg:trace.http.request.errors{service:payments} > 0.05",
  "message": "Error rate above 5%"
}

# Grafana Alert Rule (PromQL)
expr: |
  sum(rate(http_server_request_duration_seconds_count{
    service_name="payments",
    http_response_status_code=~"5.."
  }[5m]))
  /
  sum(rate(http_server_request_duration_seconds_count{
    service_name="payments"
  }[5m]))
  > 0.05
for: 5m
labels:
  severity: critical
annotations:
  summary: "Error rate {{ $value | humanizePercentage }} on payments service"
```

#### 4.3 — Tasks checklist
- [ ] Top 10 most-viewed dashboards recreated in Grafana
- [ ] All P0/P1 alert rules migrated and tested
- [ ] Alertmanager routing configured (PagerDuty, Slack, email)
- [ ] On-call runbooks updated with Grafana links
- [ ] SLO dashboards created for all critical services

---

### Phase 5 — Advanced Features (4–8 weeks)

**Goal**: Full feature parity + features Datadog/New Relic don't offer.

#### 5.1 — Trace-to-Profile correlation (unique to LGTMP)

`otel-profiling-go` (already in this repo's `otel.go`) links every OTel span to
Pyroscope profile data. In Grafana Tempo, clicking a span shows the exact CPU
flamegraph for that span's duration — **no equivalent exists in Datadog/New Relic**.

```go
// Already wired in go-lgtmp otel.go:
otel.SetTracerProvider(otelpyroscope.NewTracerProvider(tp))
```

#### 5.2 — Exemplars: Metrics → Traces

Link histogram metrics to traces for drill-down without leaving Grafana:

```go
// In Prometheus histogram — attach trace_id as exemplar
h.metrics.requestDuration.Record(ctx, duration.Seconds(),
    metric.WithAttributes(attribute.String("route", route)))
// OTel SDK automatically attaches exemplars when a span is active
```

In Grafana: enable exemplars on histogram panels → click a data point → jumps to trace in Tempo.

#### 5.3 — Tail sampling in Alloy

Keep 100% of error traces, sample success traces at 10%:

```hcl
otelcol.processor.tail_sampling "default" {
  decision_wait = "10s"
  policy {
    name = "errors-always"
    type = "status_code"
    status_code { status_codes = ["ERROR"] }
  }
  policy {
    name = "success-sample-10pct"
    type = "probabilistic"
    probabilistic { sampling_percentage = 10 }
  }
}
```

#### 5.4 — Recording rules for cost optimisation

Pre-aggregate high-cardinality metrics in Mimir to reduce storage and query cost:

```yaml
# Mimir recording rules
groups:
  - name: service_slo
    interval: 1m
    rules:
      - record: job:http_server_requests:rate5m
        expr: sum by (job, http_route) (rate(http_server_request_duration_seconds_count[5m]))
      - record: job:http_server_errors:rate5m
        expr: sum by (job, http_route) (rate(http_server_request_duration_seconds_count{http_response_status_code=~"5.."}[5m]))
```

#### 5.5 — SLO definition

```yaml
# Grafana SLO plugin
apiVersion: slo.grafana.io/v1alpha1
kind: SLO
metadata:
  name: payments-availability
spec:
  service: payments
  slo: 0.999  # 99.9% availability
  window: 30d
  indicator:
    ratio:
      errors:
        metric: job:http_server_errors:rate5m{job="payments"}
      total:
        metric: job:http_server_requests:rate5m{job="payments"}
```

#### 5.6 — Tasks checklist
- [ ] Trace-to-profile links verified in Grafana Tempo
- [ ] Exemplars enabled and linking metrics → traces
- [ ] Tail sampling configured (100% errors, sampled success)
- [ ] Recording rules deployed in Mimir
- [ ] SLO definitions created for P0 services
- [ ] Error budget burn rate alerts configured
- [ ] Synthetic checks running via Blackbox Exporter

---

### Phase 6 — Cutover & Decommission (2–4 weeks)

**Goal**: Remove Datadog/New Relic agents. Cancel subscriptions.

#### Cutover checklist

- [ ] **Run parallel for minimum 2 weeks** — both old APM and LGTMP active
- [ ] Verify metric parity (same alert counts, similar values)
- [ ] All team members have completed Grafana training
- [ ] On-call runbooks updated
- [ ] Confirm all PagerDuty/Slack/email integrations work via Alertmanager
- [ ] Remove `dd-trace` / `newrelic` SDK from all services (confirmed in PR)
- [ ] Remove Datadog Agent DaemonSet: `helm uninstall datadog`
- [ ] Remove New Relic Infrastructure Agent DaemonSet
- [ ] Delete Datadog API keys from Kubernetes secrets
- [ ] Cancel Datadog/New Relic subscription

#### Rollback plan

Keep LGTMP and old APM running in parallel. If issues arise before full cutover,
the old APM can be re-enabled within minutes. Do **not** cancel the subscription
until you have 2+ weeks of stable LGTMP operation.

---

## K8s Deployment Checklist (LGTMP Stack)

```
Namespace: monitoring
├── Grafana                        # Dashboards, alerting, SLOs
├── Loki (distributed)             # Logs  — S3 backend
├── Tempo (distributed)            # Traces — S3 backend
├── Mimir (distributed)            # Metrics — S3 backend
├── Pyroscope                      # Profiles
├── Alloy (DaemonSet)              # Collector (1 pod per node)
├── kube-state-metrics             # K8s object states
├── node-exporter (DaemonSet)      # Node hardware metrics
└── Alertmanager                   # Alert routing

Namespace: your-app-namespace
├── App pods
│     ├── annotations:
│     │     prometheus.io/scrape: "true"
│     │     prometheus.io/port: "8080"
│     │     prometheus.io/path: "/metrics"
│     └── env:
│           OTEL_SERVICE_NAME: "my-service"
│           OTEL_EXPORTER_OTLP_ENDPOINT: "http://alloy.monitoring:4317"
│           PYROSCOPE_SERVER_ADDRESS: "http://pyroscope.monitoring:4040"
└── ConfigMap: observability config
```

---

## Common Pitfalls

| Pitfall | Solution |
|---|---|
| Metric names don't match (`orders.created` vs `orders_created`) | Update all alert rules and dashboards when renaming metrics |
| DD APM trace IDs are 64-bit; OTel uses 128-bit | TraceQL and Loki correlation will work — no data loss, just different format |
| Datadog log parsing rules (Grok) lost | Convert to Alloy Loki pipeline stages (regex/JSON extraction) |
| Alert thresholds wrong after metric rename | Re-baseline thresholds using 2 weeks of LGTMP data before cutover |
| High cardinality explodes Mimir | Use recording rules; avoid high-cardinality labels (user_id, request_id) |
| `dd-trace` and OTel produce duplicate traces | Remove dd-trace completely before cutting over; don't run both |

---

## Reference

### This Repository

`go-lgtmp` is the canonical reference for instrumenting a Go service post-migration:

```
internal/telemetry/otel.go       # Copy this — OTel traces + metrics + logs init
internal/telemetry/profiler.go   # Copy this — Pyroscope profiler init
internal/middleware/middleware.go # Copy this — HTTP logging with trace_id
internal/config/config.go        # Copy this — env-based config pattern
```

### Official Helm Chart Values

| Chart | Key values file |
|---|---|
| Alloy | [grafana.com/docs/alloy](https://grafana.com/docs/alloy/latest/reference/config-blocks/) |
| Loki | [grafana.com/docs/loki/latest/setup/install/helm](https://grafana.com/docs/loki/latest/setup/install/helm/) |
| Tempo | [grafana.com/docs/tempo/latest/setup/helm-chart](https://grafana.com/docs/tempo/latest/setup/helm-chart/) |
| Mimir | [grafana.com/docs/mimir/latest/operators-guide/deploy-grafana-mimir-with-helm](https://grafana.com/docs/mimir/latest/operators-guide/deploy-grafana-mimir-with-helm/) |
| Pyroscope | [grafana.com/docs/pyroscope/latest/deploy-kubernetes](https://grafana.com/docs/pyroscope/latest/deploy-kubernetes/) |

### Useful PromQL after migration

```promql
# Request rate per service (equivalent to DD APM traffic)
sum by (service_name) (rate(http_server_request_duration_seconds_count[1m]))

# Error rate per service
sum by (service_name) (
  rate(http_server_request_duration_seconds_count{http_response_status_code=~"5.."}[5m])
) / sum by (service_name) (
  rate(http_server_request_duration_seconds_count[5m])
)

# P99 latency per service and route
histogram_quantile(0.99, sum by (le, service_name, http_route) (
  rate(http_server_request_duration_seconds_bucket[5m])
))

# Active pods per namespace
count by (namespace) (kube_pod_status_phase{phase="Running"} == 1)

# Container OOMKills
increase(kube_pod_container_status_last_terminated_reason{reason="OOMKilled"}[1h])
```

### Useful LogQL after migration

```logql
# All error logs for a service
{service_name="payments"} | json | level="error"

# Find logs for a specific trace (log → trace drill-down)
{service_name="payments"} | json | trace_id="abc123def456"

# Error rate from logs (alternative to metrics)
sum(rate({namespace="production"} | json | level="error" [5m]))
  by (service_name)

# Slow requests (duration_ms > 500)
{service_name="payments"} | json | duration_ms > 500
```
