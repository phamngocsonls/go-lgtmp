package telemetry

import (
	"log/slog"

	"github.com/grafana/pyroscope-go"

	"github.com/go-lgtmp/go-lgtmp/internal/config"
)

// InitProfiler starts the Pyroscope continuous profiler.
// Returns a stop func that flushes and disconnects the profiler.
//
// Profiles enabled:
//   - CPU (goroutine scheduling, hot paths)
//   - Heap inuse/alloc objects (memory leaks)
//   - Heap inuse/alloc space (memory pressure)
//   - Goroutine (blocking, concurrency issues)
//   - Mutex (lock contention)
//
// Pattern for other services:
//
//	stop, err := telemetry.InitProfiler(cfg)
//	defer stop()
func InitProfiler(cfg *config.Config) (func(), error) {
	profiler, err := pyroscope.Start(pyroscope.Config{
		ApplicationName: cfg.ServiceName,
		ServerAddress:   cfg.PyroscopeServerAddress,
		Logger:          pyroscope.StandardLogger,
		Tags: map[string]string{
			"version":     cfg.ServiceVersion,
			"environment": cfg.Environment,
		},
		ProfileTypes: []pyroscope.ProfileType{
			pyroscope.ProfileCPU,
			pyroscope.ProfileInuseObjects,
			pyroscope.ProfileAllocObjects,
			pyroscope.ProfileInuseSpace,
			pyroscope.ProfileAllocSpace,
			pyroscope.ProfileGoroutines,
			pyroscope.ProfileMutexCount,
			pyroscope.ProfileMutexDuration,
			pyroscope.ProfileBlockCount,
			pyroscope.ProfileBlockDuration,
		},
	})
	if err != nil {
		return func() {}, err
	}

	slog.Info("pyroscope profiler started",
		"server", cfg.PyroscopeServerAddress,
		"app", cfg.ServiceName,
	)

	return func() {
		if err := profiler.Stop(); err != nil {
			slog.Error("pyroscope stop error", "error", err)
		}
	}, nil
}
