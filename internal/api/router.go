package api

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"mutualfundanalysis/internal/analytics"
)

// Handler holds shared dependencies for all HTTP handlers.
type Handler struct {
	store    DataStore
	pipeline PipelineRunner
	analytics *analytics.Engine
	log      *slog.Logger
}

// NewRouter wires up all routes and returns a ready-to-serve http.Handler.
// Both store and pipeline are accepted as interfaces so the router can be
// constructed with either the real implementations or test doubles.
func NewRouter(
	st DataStore,
	pipeline PipelineRunner,
	ae *analytics.Engine,
	log *slog.Logger,
) http.Handler {
	h := &Handler{store: st, pipeline: pipeline, analytics: ae, log: log}

	r := chi.NewRouter()

	// Global middleware
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(structuredLogger(log))
	r.Use(recoverer(log))
	r.Use(middleware.Compress(5))

	// Health check
	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// Fund endpoints
	// NOTE: /funds/rank must be registered BEFORE /funds/{code} so chi
	// matches the static path first (chi gives priority to static segments).
	r.Get("/funds/rank", h.RankFunds)
	r.Get("/funds", h.ListFunds)
	r.Get("/funds/{code}", h.GetFund)
	r.Get("/funds/{code}/analytics", h.GetAnalytics)

	// Sync / pipeline endpoints
	r.Post("/sync/trigger", h.TriggerSync)
	r.Get("/sync/status", h.GetSyncStatus)

	// API documentation
	r.Get("/docs", swaggerUIHandler)
	r.Get("/docs/openapi.yaml", openAPISpecHandler)

	return r
}
