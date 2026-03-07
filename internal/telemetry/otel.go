// Package telemetry wires up the full OpenTelemetry SDK:
//   - Traces  → OTLP gRPC → Alloy → Tempo
//   - Metrics → Prometheus /metrics endpoint (scraped by Alloy → Mimir)
//   - Logs    → OTLP gRPC → Alloy → Loki, bridged from slog
package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	otelpyroscope "github.com/grafana/otel-profiling-go"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/prometheus"
	otellog "go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/go-lgtmp/go-lgtmp/internal/config"
)

// ShutdownFunc flushes and shuts down all OTel providers. Call on service exit.
type ShutdownFunc func(ctx context.Context) error

// InitOTel initialises the OpenTelemetry SDK and sets global providers.
// Returns a ShutdownFunc that must be called before process exit.
//
// Pattern for other services:
//
//	shutdown, err := telemetry.InitOTel(ctx, cfg)
//	defer shutdown(ctx)
func InitOTel(ctx context.Context, cfg *config.Config) (ShutdownFunc, error) {
	res, err := buildResource(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("build resource: %w", err)
	}

	// Single shared gRPC connection for all OTLP signals (traces + logs).
	// Reusing one connection avoids redundant TCP handshakes and halves the
	// number of open file descriptors.
	dialOpts := []grpc.DialOption{}
	if cfg.OTLPInsecure {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	conn, err := grpc.NewClient(cfg.OTLPEndpoint, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("grpc dial: %w", err)
	}

	var shutdowns []func(context.Context) error

	// ── Traces ────────────────────────────────────────────────────────────────
	traceShutdown, err := initTracer(ctx, conn, res)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("init tracer: %w", err)
	}
	shutdowns = append(shutdowns, traceShutdown)

	// ── Metrics ───────────────────────────────────────────────────────────────
	metricShutdown, err := initMeter(res)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("init meter: %w", err)
	}
	shutdowns = append(shutdowns, metricShutdown)

	// ── Logs ──────────────────────────────────────────────────────────────────
	logShutdown, err := initLogger(ctx, conn, res, cfg)
	if err != nil {
		// Non-fatal: fall back to stdout-only logging.
		slog.Warn("OTLP log exporter unavailable, using stdout only", "error", err)
		installStdoutLogger(cfg)
	} else {
		shutdowns = append(shutdowns, logShutdown)
	}

	// Set global propagator: W3C TraceContext headers + Baggage.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	// The gRPC connection is closed last, after all exporters have flushed.
	shutdowns = append(shutdowns, func(_ context.Context) error {
		return conn.Close()
	})

	return func(ctx context.Context) error {
		var lastErr error
		for _, fn := range shutdowns {
			if err := fn(ctx); err != nil {
				lastErr = err
			}
		}
		return lastErr
	}, nil
}

// buildResource creates an OTel Resource with service identity attributes.
func buildResource(ctx context.Context, cfg *config.Config) (*resource.Resource, error) {
	return resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithProcess(),
		resource.WithOS(),
		resource.WithHost(),
		resource.WithAttributes(
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(cfg.ServiceVersion),
			semconv.DeploymentEnvironment(cfg.Environment),
		),
	)
}

// initTracer sets up the OTLP gRPC trace exporter and registers the global TracerProvider.
//
// otelpyroscope.NewTracerProvider wraps the SDK provider and tags every goroutine
// with pprof labels (profile_id, span_id, trace_id, span_name) for the duration
// of each span. This is what enables the "Profiles" button in Grafana Tempo —
// clicking it opens the Pyroscope flamegraph filtered to the exact CPU work that
// happened during that span's lifetime.
func initTracer(ctx context.Context, conn *grpc.ClientConn, res *resource.Resource) (func(context.Context) error, error) {
	exporter, err := otlptracegrpc.New(ctx, otlptracegrpc.WithGRPCConn(conn))
	if err != nil {
		return nil, fmt.Errorf("otlp trace exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter,
			sdktrace.WithBatchTimeout(5*time.Second),
		),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)

	// Wrap with Pyroscope ↔ Tempo profiling link: every span start/end sets
	// pprof labels on the goroutine so Pyroscope captures CPU samples tagged
	// with the active trace_id, enabling cross-pillar drill-down in Grafana.
	otel.SetTracerProvider(otelpyroscope.NewTracerProvider(tp))

	return tp.Shutdown, nil
}

// initMeter sets up the Prometheus metric exporter and registers the global MeterProvider.
// The Prometheus registry is served on GET /metrics by promhttp.Handler().
func initMeter(res *resource.Resource) (func(context.Context) error, error) {
	promExporter, err := prometheus.New()
	if err != nil {
		return nil, fmt.Errorf("prometheus exporter: %w", err)
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(promExporter),
		sdkmetric.WithResource(res),
	)

	otel.SetMeterProvider(mp)

	return mp.Shutdown, nil
}

// initLogger sets up the OTLP gRPC log exporter, registers the global LoggerProvider,
// and installs an slog handler that bridges stdlib log/slog → OTel logs.
//
// Logs are written to two destinations:
//   - OTLP gRPC → Alloy → Loki (structured, with trace correlation)
//   - JSON stdout → Alloy log scrape / docker logs (for local dev)
func initLogger(ctx context.Context, conn *grpc.ClientConn, res *resource.Resource, cfg *config.Config) (func(context.Context) error, error) {
	logExporter, err := otlploggrpc.New(ctx, otlploggrpc.WithGRPCConn(conn))
	if err != nil {
		return nil, fmt.Errorf("otlp log exporter: %w", err)
	}

	lp := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExporter)),
		sdklog.WithResource(res),
	)

	otellog.SetLoggerProvider(lp)

	// Bridge: slog → OTel LoggerProvider + JSON stdout fan-out.
	otelHandler := otelslog.NewHandler("github.com/go-lgtmp/go-lgtmp")
	stdoutHandler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: parseLogLevel(cfg.LogLevel),
	})
	slog.SetDefault(slog.New(&multiHandler{otelHandler, stdoutHandler}))

	return lp.Shutdown, nil
}

// installStdoutLogger sets up JSON-only stdout logging when OTLP is unavailable.
func installStdoutLogger(cfg *config.Config) {
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: parseLogLevel(cfg.LogLevel),
	})
	slog.SetDefault(slog.New(h))
}

// parseLogLevel converts a LOG_LEVEL string to a slog.Level.
// Defaults to Info for unknown/empty values.
func parseLogLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// multiHandler fans out slog records to multiple handlers.
type multiHandler struct {
	a, b slog.Handler
}

func (m *multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return m.a.Enabled(ctx, level) || m.b.Enabled(ctx, level)
}

func (m *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	// Best-effort: log errors from the OTel handler but don't fail the call.
	if err := m.a.Handle(ctx, r); err != nil {
		_ = m.b.Handle(ctx, slog.NewRecord(r.Time, slog.LevelWarn,
			fmt.Sprintf("otel log handler error: %v", err), r.PC))
	}
	return m.b.Handle(ctx, r)
}

func (m *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &multiHandler{m.a.WithAttrs(attrs), m.b.WithAttrs(attrs)}
}

func (m *multiHandler) WithGroup(name string) slog.Handler {
	return &multiHandler{m.a.WithGroup(name), m.b.WithGroup(name)}
}
