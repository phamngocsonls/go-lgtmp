package config

import (
	"os"
	"strings"
)

// Config holds all service configuration loaded from environment variables.
type Config struct {
	Port                   string
	ServiceName            string
	ServiceVersion         string
	OTLPEndpoint           string // host:port for gRPC (e.g. localhost:4317)
	OTLPInsecure           bool
	PyroscopeServerAddress string
	Environment            string
	LogLevel               string
	DatabaseDSN            string // PostgreSQL DSN — empty disables DB demo endpoints
	RedisAddr              string // Redis host:port — empty disables cache demo endpoints
}

// Load reads configuration from environment variables with sensible defaults.
// All settings can be overridden via env vars - see README for full reference.
func Load() *Config {
	return &Config{
		Port:                   getEnv("PORT", "8080"),
		ServiceName:            getEnv("OTEL_SERVICE_NAME", "go-lgtmp"),
		ServiceVersion:         getEnv("OTEL_SERVICE_VERSION", "0.1.0"),
		OTLPEndpoint:           stripScheme(getEnv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4317")),
		OTLPInsecure:           getEnv("OTEL_EXPORTER_OTLP_INSECURE", "true") == "true",
		PyroscopeServerAddress: getEnv("PYROSCOPE_SERVER_ADDRESS", "http://localhost:4040"),
		Environment:            getEnv("ENVIRONMENT", "development"),
		LogLevel:               getEnv("LOG_LEVEL", "info"),
		DatabaseDSN:            getEnv("DATABASE_DSN", ""),
		RedisAddr:              getEnv("REDIS_ADDR", ""),
	}
}

func getEnv(key, defaultValue string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultValue
}

// stripScheme removes http:// or https:// prefix so we get host:port for gRPC.
func stripScheme(endpoint string) string {
	endpoint = strings.TrimPrefix(endpoint, "http://")
	endpoint = strings.TrimPrefix(endpoint, "https://")
	return endpoint
}
