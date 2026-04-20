package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"mutualfundanalysis/internal/analytics"
	"mutualfundanalysis/internal/api"
	"mutualfundanalysis/internal/client"
	"mutualfundanalysis/internal/config"
	"mutualfundanalysis/internal/ingestion"
	"mutualfundanalysis/internal/ratelimit"
	"mutualfundanalysis/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// ── 1. Config ─────────────────────────────────────────────────────────────
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// ── 2. Structured logger ──────────────────────────────────────────────────
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: cfg.LogLevel,
	}))
	slog.SetDefault(log)
	log.Info("mutual fund analytics service starting", "port", cfg.Port)

	// ── 3. Root context with graceful shutdown ────────────────────────────────
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ── 4. Database ───────────────────────────────────────────────────────────
	pool, err := store.NewPool(ctx, cfg.DatabaseURL, log)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	st := store.New(pool, log)
	defer st.Close()
	log.Info("database connection pool ready")

	// ── 5. Rate limiter (restored from DB) ───────────────────────────────────
	limiter, err := ratelimit.New(ctx, st, log)
	if err != nil {
		return fmt.Errorf("initialize rate limiter: %w", err)
	}

	log.Info("rate limiter initialized")

	// ── 6. mfapi HTTP client ──────────────────────────────────────────────────
	mfClient := client.New(limiter, st, log)

	// ── 7. Analytics engine ───────────────────────────────────────────────────
	analyticsEngine := analytics.New(st, log)

	// ── 8. Ingestion pipeline ─────────────────────────────────────────────────
	pipeline := ingestion.NewPipeline(mfClient, st, analyticsEngine, log)

	// ── 9. Seed scheme codes + sync_state (idempotent) ────────────────────────
	ingestion.SeedSchemes(ctx, st, log)

	// ── 10. Background Sync & Repair ──────────────────────────────────────────
	// Run the initial backfill and analytics repair pass in a background goroutine.
	// This ensures the HTTP server boots instantly and passes health checks
	// even if the external API is slow or rate-limited.
	// Clients can use GET /sync/status to monitor progress.
	go func() {
		log.Info("starting background backfill")
		pipeline.RunBackfill(ctx)
		
		if ctx.Err() == nil {
			log.Info("running analytics repair pass")
			pipeline.RepairAnalytics(ctx)
		}
	}()

	// ── 11. Daily sync scheduler (background) ────────────────────────────────
	scheduler := ingestion.NewScheduler(pipeline)
	go scheduler.Start(ctx)
	log.Info("daily sync scheduler started")

	// ── 12. HTTP server ───────────────────────────────────────────────────────
	router := api.NewRouter(st, pipeline, analyticsEngine, log)
	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      router,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Start server in background
	serverErr := make(chan error, 1)
	go func() {
		log.Info("HTTP server listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	// ── 13. Block until shutdown signal or server error ───────────────────────
	select {
	case err := <-serverErr:
		return fmt.Errorf("http server error: %w", err)
	case <-ctx.Done():
		log.Info("shutdown signal received")
	}

	// ── 14. Graceful shutdown ─────────────────────────────────────────────────
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	log.Info("shutting down HTTP server gracefully")
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("HTTP server forced shutdown", "error", err)
	}

	log.Info("service stopped cleanly")
	return nil
}
