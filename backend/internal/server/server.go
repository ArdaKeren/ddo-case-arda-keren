// Package server assembles the HTTP application: routes, middleware and the
// background workers (realtime hub, janitor). It is shared by cmd/api and the
// end-to-end integration tests so both exercise exactly the same wiring.
package server

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/qa/backend/internal/admin"
	"github.com/qa/backend/internal/audit"
	"github.com/qa/backend/internal/auth"
	"github.com/qa/backend/internal/config"
	"github.com/qa/backend/internal/health"
	"github.com/qa/backend/internal/httpx"
	"github.com/qa/backend/internal/idempotency"
	"github.com/qa/backend/internal/queue"
	"github.com/qa/backend/internal/realtime"
	"github.com/qa/backend/migrations"
)

type App struct {
	Handler http.Handler

	db        *sql.DB
	repo      *queue.Repository
	hub       *realtime.Hub
	retention time.Duration
}

// New builds the application on an already-migrated database and bootstraps
// the first admin if configured. Call Run to start the background workers.
func New(ctx context.Context, cfg config.Config, db *sql.DB) (*App, error) {
	store := auth.NewStore(db)
	if err := auth.BootstrapAdmin(ctx, store, cfg.AdminEmail, cfg.AdminPassword); err != nil {
		return nil, err
	}

	tokens := auth.NewTokenIssuer(cfg.JWTSecret, cfg.JWTIssuer, cfg.AccessTTL)
	auditLog := audit.New(db)
	authHandler, err := auth.NewHandler(store, tokens, auditLog, cfg.RefreshTTL)
	if err != nil {
		return nil, err
	}
	healthHandler := health.NewHandler(db, migrations.VersionTable)
	queueRepo := queue.NewRepository(db, cfg.QueueTimezone)
	queueHandler := queue.NewHandler(queueRepo, tokens, auditLog)
	hub := realtime.NewHub(queueRepo, cfg.DatabaseURL)
	wsServer := realtime.NewServer(hub, queueRepo, tokens, cfg.AllowedOrigins)

	r := chi.NewRouter()
	r.Use(httpx.DDOMark) // first, so every response carries the mark
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)

	r.NotFound(func(w http.ResponseWriter, r *http.Request) { httpx.Error(w, http.StatusNotFound, "not found") })
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		httpx.Error(w, http.StatusMethodNotAllowed, "method not allowed")
	})

	// WebSockets are long-lived, so they must not get the request timeout below.
	r.With(middleware.Logger).Get("/ws", wsServer.ServeHTTP)

	r.Group(func(r chi.Router) {
		r.Use(middleware.Logger)
		r.Use(middleware.Timeout(30 * time.Second))

		r.Get("/health", healthHandler.Public)
		r.Get("/_hz/bfe1", healthHandler.Hidden)

		r.Route("/api", func(r chi.Router) {
			r.Mount("/auth", authHandler.Routes())
			r.Mount("/admin", admin.NewHandler(store, tokens).Routes())
			r.Mount("/tickets", queueHandler.TicketRoutes())
			r.Mount("/appointments", queueHandler.AppointmentRoutes())
			r.Mount("/queue", queueHandler.QueueRoutes())
		})
	})

	return &App{Handler: r, db: db, repo: queueRepo, hub: hub, retention: cfg.EventRetention}, nil
}

// Run blocks until ctx is cancelled, running the realtime hub and the janitor.
func (a *App) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); a.hub.Run(ctx) }()
	go func() { defer wg.Done(); a.janitor(ctx) }()
	wg.Wait()
}

// janitor enforces event retention and drops expired idempotency keys hourly.
func (a *App) janitor(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if n, err := a.repo.DeleteEventsBefore(ctx, time.Now().Add(-a.retention)); err != nil && ctx.Err() == nil {
			log.Printf("janitor: events: %v", err)
		} else if n > 0 {
			log.Printf("janitor: deleted %d queue events older than %s", n, a.retention)
		}
		if _, err := idempotency.DeleteExpired(ctx, a.db); err != nil && ctx.Err() == nil {
			log.Printf("janitor: idempotency keys: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
