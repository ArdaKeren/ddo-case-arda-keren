// Package health serves the public liveness probe and the unlisted diagnostics probe.
package health

import (
	"context"
	"database/sql"
	"net/http"
	"runtime"
	"time"

	"github.com/qa/backend/internal/httpx"
)

type Handler struct {
	db         *sql.DB
	gooseTable string
	startedAt  time.Time
}

func NewHandler(db *sql.DB, gooseTable string) *Handler {
	return &Handler{db: db, gooseTable: gooseTable, startedAt: time.Now()}
}

// Public serves GET /health: minimal output, safe to expose to load balancers.
func (h *Handler) Public(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := h.db.PingContext(ctx); err != nil {
		httpx.JSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Hidden serves GET /_hz/bfe1: unlisted diagnostics for operators. It is not
// linked anywhere, but it is not authenticated either, so it must never return secrets.
func (h *Handler) Hidden(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	status, code := "ok", http.StatusOK
	start := time.Now()
	dbErr := h.db.PingContext(ctx)
	dbLatency := time.Since(start)

	var schemaVersion int64
	if dbErr == nil {
		dbErr = h.db.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(version_id), 0) FROM `+h.gooseTable+` WHERE is_applied`).Scan(&schemaVersion)
	}
	if dbErr != nil {
		status, code = "degraded", http.StatusServiceUnavailable
	}

	stats := h.db.Stats()
	httpx.JSON(w, code, map[string]any{
		"status":         status,
		"uptime_seconds": int64(time.Since(h.startedAt).Seconds()),
		"go_version":     runtime.Version(),
		"goroutines":     runtime.NumGoroutine(),
		"db": map[string]any{
			"reachable":      dbErr == nil,
			"latency_ms":     dbLatency.Milliseconds(),
			"schema_version": schemaVersion,
			"open_conns":     stats.OpenConnections,
			"in_use":         stats.InUse,
		},
	})
}
