package handler

// Store handlers demonstrate database and cache tracing patterns.
//
// # How child spans are created automatically
//
// Both otelsql (Postgres) and redisotel (Redis) hook into their respective
// client libraries and create child spans for every operation — no manual
// tracer.Start() calls needed inside the store layer.
//
// The only requirement: always pass ctx (which carries the active span) through
// to every DB/Redis call. The parent span (created by otelhttp at the HTTP
// boundary) is automatically linked.
//
// Resulting trace tree for GET /db/users:
//
//	[HTTP GET /db/users]          ← otelhttp
//	  └─ [list_users]             ← tracer.Start() in handler
//	       └─ [db.query]          ← otelsql auto-instrumented
//	            db.system    = postgresql
//	            db.statement = SELECT id, name, email, ...
//	            db.operation = SELECT
//
// Resulting trace tree for GET /cache/users/{id}:
//
//	[HTTP GET /cache/users/{id}]  ← otelhttp
//	  └─ [cache_get_user]         ← tracer.Start() in handler
//	       ├─ [redis GET]         ← redisotel auto-instrumented (cache hit path)
//	       │    db.system    = redis
//	       │    db.statement = get go-lgtmp:user:1
//	       │    — OR on cache miss —
//	       ├─ [redis GET]         ← miss
//	       ├─ [db.query]          ← otelsql fallback SELECT
//	       └─ [redis SET]         ← redisotel writes through to cache

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	chi "github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/go-lgtmp/go-lgtmp/internal/store"
)

// DBInfo handles GET /db
// Runs a lightweight diagnostic query and returns server info.
// Demonstrates: otelsql auto-instrumented child span for the SELECT.
func (h *Handler) DBInfo(w http.ResponseWriter, r *http.Request) {
	if h.db == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "database not configured — set DATABASE_DSN env var",
		})
		return
	}

	ctx, span := tracer.Start(r.Context(), "db_info")
	defer span.End()

	// ServerInfo calls db.QueryRowContext(ctx, "SELECT now(), ...").
	// otelsql intercepts this call and creates a child span automatically.
	info, err := h.db.ServerInfo(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.ErrorContext(ctx, "db info query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	slog.InfoContext(ctx, "db info ok", "database", info.Database)
	writeJSON(w, http.StatusOK, info)
}

// DBListUsers handles GET /db/users
// Returns all users from the database.
// Demonstrates: otelsql auto-instrumented child span for SELECT users.
func (h *Handler) DBListUsers(w http.ResponseWriter, r *http.Request) {
	if h.db == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "database not configured — set DATABASE_DSN env var",
		})
		return
	}

	ctx, span := tracer.Start(r.Context(), "list_users")
	defer span.End()

	users, err := h.db.ListUsers(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.ErrorContext(ctx, "list users failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	span.SetAttributes(attribute.Int("users.count", len(users)))
	slog.InfoContext(ctx, "list users ok", "count", len(users))
	writeJSON(w, http.StatusOK, map[string]any{"users": users, "count": len(users)})
}

// DBCreateUser handles POST /db/users
// Body: {"name": "Alice", "email": "alice@example.com"}
// Demonstrates: otelsql auto-instrumented INSERT span.
func (h *Handler) DBCreateUser(w http.ResponseWriter, r *http.Request) {
	if h.db == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "database not configured — set DATABASE_DSN env var",
		})
		return
	}

	ctx, span := tracer.Start(r.Context(), "create_user")
	defer span.End()

	var body struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if body.Name == "" || body.Email == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name and email are required"})
		return
	}

	span.SetAttributes(attribute.String("user.email", body.Email))

	user, err := h.db.CreateUser(ctx, body.Name, body.Email)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.ErrorContext(ctx, "create user failed", "email", body.Email, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	slog.InfoContext(ctx, "user created", "user_id", user.ID, "email", user.Email)
	writeJSON(w, http.StatusCreated, user)
}

// CacheGetUser handles GET /cache/users/{id}
//
// Demonstrates the cache-aside pattern with full trace visibility:
//
//  1. Try Redis GET — redisotel creates a child span automatically
//  2. On cache miss (redis.Nil): query Postgres — otelsql creates a child span
//  3. Write result back to Redis SET — redisotel creates another child span
//
// In Tempo you see all three child spans under the HTTP root span, making
// cache hit/miss immediately visible without any log digging.
func (h *Handler) CacheGetUser(w http.ResponseWriter, r *http.Request) {
	if h.db == nil || h.cache == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "database and cache required — set DATABASE_DSN and REDIS_ADDR env vars",
		})
		return
	}

	ctx, span := tracer.Start(r.Context(), "cache_get_user")
	defer span.End()

	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id must be an integer"})
		return
	}

	cacheKey := fmt.Sprintf("go-lgtmp:user:%d", id)
	span.SetAttributes(
		attribute.Int64("user.id", id),
		attribute.String("cache.key", cacheKey),
	)

	// ── Step 1: try cache ─────────────────────────────────────────────────────
	// redisotel auto-creates a child span: db.system=redis, db.statement="get go-lgtmp:user:1"
	cached, err := h.cache.Get(ctx, cacheKey)
	if err == nil {
		// Cache hit — no DB needed
		span.SetAttributes(attribute.Bool("cache.hit", true))
		slog.InfoContext(ctx, "cache hit", "key", cacheKey)
		w.Header().Set("X-Cache", "HIT")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(cached))
		return
	}

	if !errors.Is(err, store.ErrCacheMiss) {
		// Unexpected Redis error — log but fall through to DB (resilient pattern)
		slog.WarnContext(ctx, "cache get error, falling back to db", "key", cacheKey, "error", err)
	}

	span.SetAttributes(attribute.Bool("cache.hit", false))
	slog.InfoContext(ctx, "cache miss", "key", cacheKey)

	// ── Step 2: cache miss → query database by ID ────────────────────────────
	// GetUserByID issues "SELECT ... WHERE id = $1" — otelsql auto-creates a
	// child span with db.system=postgresql and the exact db.statement.
	// Using a targeted query (not ListUsers) avoids a full-table scan.
	found, err := h.db.GetUserByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "user not found"})
			return
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.ErrorContext(ctx, "db fallback failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// ── Step 3: write through to cache ────────────────────────────────────────
	// redisotel auto-creates a child span: db.statement="set go-lgtmp:user:1 ... ex 60"
	payload, _ := json.Marshal(found)
	if setErr := h.cache.Set(ctx, cacheKey, string(payload), 60*time.Second); setErr != nil {
		slog.WarnContext(ctx, "cache write-through failed", "key", cacheKey, "error", setErr)
	}

	slog.InfoContext(ctx, "cache miss served from db", "user_id", found.ID)
	w.Header().Set("X-Cache", "MISS")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(payload)
}

// CacheSet handles POST /cache
// Body: {"key": "foo", "value": "bar", "ttl_seconds": 60}
// Demonstrates: redisotel auto-instrumented SET span.
func (h *Handler) CacheSet(w http.ResponseWriter, r *http.Request) {
	if h.cache == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "cache not configured — set REDIS_ADDR env var",
		})
		return
	}

	ctx, span := tracer.Start(r.Context(), "cache_set")
	defer span.End()

	var body struct {
		Key        string `json:"key"`
		Value      string `json:"value"`
		TTLSeconds int    `json:"ttl_seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if body.Key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "key is required"})
		return
	}

	ttl := time.Duration(body.TTLSeconds) * time.Second
	span.SetAttributes(
		attribute.String("cache.key", body.Key),
		attribute.Int("cache.ttl_seconds", body.TTLSeconds),
	)

	// redisotel auto-creates: db.statement = "set <key> <value> ex <ttl>"
	if err := h.cache.Set(ctx, body.Key, body.Value, ttl); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.ErrorContext(ctx, "cache set failed", "key", body.Key, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	slog.InfoContext(ctx, "cache set ok", "key", body.Key, "ttl_seconds", body.TTLSeconds)
	writeJSON(w, http.StatusOK, map[string]any{"key": body.Key, "ttl_seconds": body.TTLSeconds})
}
