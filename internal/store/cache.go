package store

// Redis — redisotel
//
// redisotel.InstrumentTracing hooks into go-redis v9's command pipeline and
// automatically creates a child span for every command (GET, SET, DEL, …).
// Each span carries OTel DB semantic convention attributes:
//
//	db.system    = "redis"
//	db.statement = "SET go-lgtmp:users EX 60"
//	server.address = "<host>"
//	server.port    = <port>
//
// redisotel.InstrumentMetrics additionally exposes:
//
//	db_client_operation_duration_seconds{db_system="redis", db_operation="SET"}
//
// No manual span creation is needed — pass ctx to every client call and it
// automatically becomes a child of the active span.
//
// Pattern for other services:
//
//	cache, err := store.OpenCache(cfg.RedisAddr)
//	defer cache.Close()

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/extra/redisotel/v9"
	"github.com/redis/go-redis/v9"
)

// ErrCacheMiss is returned by Get when the key does not exist in Redis.
var ErrCacheMiss = redis.Nil

// Cache wraps a go-redis client instrumented with OpenTelemetry.
type Cache struct {
	client *redis.Client
}

// OpenCache connects to Redis at addr (host:port) and instruments the client
// with OTel tracing and metrics.
//
// Every command (GET, SET, DEL, …) automatically creates a child span under
// whatever span is active in ctx, following OTel DB semantic conventions.
func OpenCache(addr string) (*Cache, error) {
	client := redis.NewClient(&redis.Options{
		Addr:         addr,
		DialTimeout:  3 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
		PoolSize:     10,
		MinIdleConns: 2,
	})

	// InstrumentTracing adds a hook that creates a child span per command.
	// db.system="redis", db.statement="SET key value EX 60"
	if err := redisotel.InstrumentTracing(client); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("instrument redis tracing: %w", err)
	}

	// InstrumentMetrics exposes db_client_operation_duration_seconds histogram
	// via the global MeterProvider (which feeds Prometheus /metrics).
	if err := redisotel.InstrumentMetrics(client); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("instrument redis metrics: %w", err)
	}

	return &Cache{client: client}, nil
}

// Ping checks connectivity. Creates a child span (db.statement="ping").
func (c *Cache) Ping(ctx context.Context) error {
	return c.client.Ping(ctx).Err()
}

// Close closes the Redis connection pool.
func (c *Cache) Close() error {
	return c.client.Close()
}

// Set stores value at key with the given TTL. Pass 0 for no expiry.
// Creates a child span: db.statement = "set <key> <value> ex <ttl>"
func (c *Cache) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	return c.client.Set(ctx, key, value, ttl).Err()
}

// Get retrieves the value stored at key.
// Returns ErrCacheMiss (redis.Nil) if the key does not exist.
// Creates a child span: db.statement = "get <key>"
func (c *Cache) Get(ctx context.Context, key string) (string, error) {
	return c.client.Get(ctx, key).Result()
}

// Del deletes one or more keys. Creates a child span: db.statement = "del <key>…"
func (c *Cache) Del(ctx context.Context, keys ...string) error {
	return c.client.Del(ctx, keys...).Err()
}
