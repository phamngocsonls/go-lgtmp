// Package store provides instrumented database and cache clients.
//
// # PostgreSQL — otelsql
//
// otelsql wraps the standard database/sql driver and automatically creates a
// child span for every QueryContext / ExecContext / BeginTx call. Each span
// carries OTel DB semantic convention attributes:
//
//	db.system      = "postgresql"
//	db.statement   = "<SQL text>"
//	db.operation   = "SELECT" | "INSERT" | …
//	server.address = "<host>"
//	server.port    = <port>
//
// No manual span creation is needed inside queries — just pass ctx through and
// every SQL call automatically becomes a child of the active span.
//
// Pattern for other services:
//
//	db, err := store.OpenDB(cfg.DatabaseDSN)
//	defer db.Close()
//	// then pass db into handlers and always call methods with a context
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/XSAM/otelsql"
	_ "github.com/jackc/pgx/v5/stdlib" // registers "pgx" driver for database/sql
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// DB wraps a *sql.DB instrumented with OpenTelemetry via otelsql.
type DB struct {
	db       *sql.DB
	statsReg metric.Registration // unregistered on Close to avoid metric leaks
}

// OpenDB opens a PostgreSQL connection pool wrapped with OTel instrumentation.
// dsn is a standard PostgreSQL connection string, e.g.:
//
//	"postgres://user:pass@localhost:5432/dbname?sslmode=disable"
//
// The returned DB is safe for concurrent use. Call Close when done.
func OpenDB(dsn string) (*DB, error) {
	// otelsql.Open wraps the driver: every QueryContext / ExecContext / BeginTx
	// automatically creates a child span with db.statement and db.operation
	// attributes following OTel semantic conventions for databases.
	db, err := otelsql.Open("pgx", dsn,
		otelsql.WithAttributes(semconv.DBSystemPostgreSQL),
		otelsql.WithSpanOptions(otelsql.SpanOptions{
			// Record the full SQL statement on the span (disable in prod if
			// your queries contain PII / sensitive data).
			DisableQuery: false,
			// Propagate errors so error spans are visible in Tempo.
			RecordError: func(err error) bool { return err != nil },
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	// Conservative pool settings — tune for your workload.
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)
	db.SetConnMaxIdleTime(2 * time.Minute)

	// Register connection pool stats as OTel metrics. These flow to Prometheus
	// via the global MeterProvider (set by initMeter in telemetry/otel.go):
	//   db_client_connections_usage{pool.name, state}   — active vs idle
	//   db_client_connections_max                        — pool ceiling
	//   db_client_connections_wait_duration_seconds      — wait time histogram
	statsReg, err := otelsql.RegisterDBStatsMetrics(db,
		otelsql.WithAttributes(semconv.DBSystemPostgreSQL),
	)
	if err != nil {
		return nil, fmt.Errorf("register db stats metrics: %w", err)
	}

	// Verify the connection is actually reachable at startup.
	// sql.Open only validates the DSN format; the first real network call
	// happens here so bad credentials or unreachable hosts fail fast.
	if err := db.PingContext(context.Background()); err != nil {
		_ = statsReg.Unregister()
		_ = db.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}

	return &DB{db: db, statsReg: statsReg}, nil
}

// Ping verifies the connection is alive. The ping creates a child span
// (db.operation = "ping") under whatever span is active in ctx.
func (d *DB) Ping(ctx context.Context) error {
	return d.db.PingContext(ctx)
}

// Close unregisters the pool metrics and closes all connections.
func (d *DB) Close() error {
	if d.statsReg != nil {
		_ = d.statsReg.Unregister()
	}
	return d.db.Close()
}

// ServerInfo holds the result of the demo diagnostic query.
type ServerInfo struct {
	Now      time.Time `json:"now"`
	Database string    `json:"database"`
	Version  string    `json:"version"`
}

// ServerInfo runs a lightweight diagnostic SELECT that shows:
//   - current timestamp (demonstrates round-trip latency)
//   - current database name
//   - PostgreSQL server version
//
// otelsql auto-creates a child span for this query with:
//
//	db.statement = "SELECT now(), current_database(), version()"
//	db.operation = "SELECT"
func (d *DB) ServerInfo(ctx context.Context) (*ServerInfo, error) {
	row := d.db.QueryRowContext(ctx,
		"SELECT now(), current_database(), version()")

	var info ServerInfo
	if err := row.Scan(&info.Now, &info.Database, &info.Version); err != nil {
		return nil, fmt.Errorf("scan server info: %w", err)
	}
	return &info, nil
}

// CreateUsersTable creates the demo users table if it does not exist.
// Called once at startup so the /db/users endpoints work without manual setup.
func (d *DB) CreateUsersTable(ctx context.Context) error {
	_, err := d.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS users (
			id         BIGSERIAL PRIMARY KEY,
			name       TEXT      NOT NULL,
			email      TEXT      NOT NULL UNIQUE,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`)
	return err
}

// User represents a row in the users table.
type User struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"created_at"`
}

// ListUsers returns all users. Each call creates a child DB span automatically.
func (d *DB) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := d.db.QueryContext(ctx,
		"SELECT id, name, email, created_at FROM users ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Name, &u.Email, &u.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// GetUserByID fetches a single user by primary key.
// Returns sql.ErrNoRows (wrapped) when not found — check with errors.Is(err, sql.ErrNoRows).
// Creates a child DB span: db.statement = "SELECT ... WHERE id = $1"
func (d *DB) GetUserByID(ctx context.Context, id int64) (*User, error) {
	var u User
	err := d.db.QueryRowContext(ctx,
		"SELECT id, name, email, created_at FROM users WHERE id = $1", id,
	).Scan(&u.ID, &u.Name, &u.Email, &u.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("get user by id: %w", err)
	}
	return &u, nil
}

// CreateUser inserts a new user and returns the created record.
func (d *DB) CreateUser(ctx context.Context, name, email string) (*User, error) {
	var u User
	err := d.db.QueryRowContext(ctx,
		"INSERT INTO users (name, email) VALUES ($1, $2) RETURNING id, name, email, created_at",
		name, email,
	).Scan(&u.ID, &u.Name, &u.Email, &u.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}
	return &u, nil
}
