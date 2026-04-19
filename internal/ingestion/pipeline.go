// Package ingestion manages the data pipeline:
//   - SeedSchemes: registers tracked scheme codes in the DB at startup
//   - Pipeline.SyncScheme: core function used by both backfill and daily sync
//   - Backfill + Scheduler build on top of Pipeline
package ingestion

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"mutualfundanalysis/internal/analytics"
	"mutualfundanalysis/internal/client"
	"mutualfundanalysis/internal/models"
	"mutualfundanalysis/internal/store"
)

// Pipeline orchestrates scheme syncing: fetch → parse → persist → analytics.
type Pipeline struct {
	client    *client.Client
	store     *store.Store
	analytics *analytics.Engine
	log       *slog.Logger
}

// NewPipeline creates a Pipeline.
func NewPipeline(
	c *client.Client,
	st *store.Store,
	ae *analytics.Engine,
	log *slog.Logger,
) *Pipeline {
	return &Pipeline{client: c, store: st, analytics: ae, log: log}
}

// SeedSchemes inserts all tracked scheme codes into schemes + sync_state tables.
// Called once at startup before any sync runs.
func SeedSchemes(ctx context.Context, st *store.Store, log *slog.Logger) {
	for _, code := range TrackedSchemes {
		if err := st.SeedScheme(ctx, code); err != nil {
			log.Error("failed to seed scheme", "code", code, "error", err)
		}
		if err := st.SeedSyncState(ctx, code); err != nil {
			log.Error("failed to seed sync_state", "code", code, "error", err)
		}
	}
	log.Info("scheme seeds applied", "count", len(TrackedSchemes))
}

// SyncScheme is the single shared function for both backfill and daily sync.
// It automatically determines the date range using the stored checkpoint:
//   - No data yet        → fetches the last 10 years
//   - Partial/crashed    → fetches only from last committed date + 1 day
//   - Already up to date → returns immediately (no API call)
//
// On success: scheme metadata, nav_data, and sync_state are updated atomically.
// On failure: sync_state is marked 'error'; the scheme will be retried.
func (p *Pipeline) SyncScheme(ctx context.Context, schemeCode string) error {
	log := p.log.With("scheme_code", schemeCode)

	// Mark as running before any network I/O so crash recovery works
	if err := p.store.MarkRunning(ctx, schemeCode); err != nil {
		return fmt.Errorf("mark running: %w", err)
	}

	// Determine start date from checkpoint
	lastDate, err := p.store.GetMaxNavDate(ctx, schemeCode)
	if err != nil {
		p.markError(ctx, schemeCode, err)
		return fmt.Errorf("get checkpoint: %w", err)
	}

	endDate := time.Now()
	var startDate time.Time
	if lastDate == nil {
		startDate = endDate.AddDate(-10, 0, 0)
		log.Info("starting 10-year backfill",
			"start_date", startDate.Format("2006-01-02"),
			"end_date", endDate.Format("2006-01-02"),
		)
	} else {
		startDate = lastDate.AddDate(0, 0, 1)
		log.Info("incremental sync",
			"from", startDate.Format("2006-01-02"),
			"to", endDate.Format("2006-01-02"),
		)
	}

	// Nothing to fetch — already up to date
	if !startDate.Before(endDate) {
		log.Info("already up to date, no API call needed")
		// Ensure status is 'done' (could be 'running' if manually triggered)
		_ = p.store.SaveSyncResult(ctx, &models.Scheme{Code: schemeCode}, nil)
		return nil
	}

	// Fetch from API (with retry + rate limiting)
	resp, err := p.client.FetchSchemeData(ctx, schemeCode, startDate, endDate)
	if err != nil {
		p.markError(ctx, schemeCode, err)
		return fmt.Errorf("fetch scheme data: %w", err)
	}

	// Parse API response into typed models
	scheme, navRows, err := client.ParseResponse(schemeCode, resp, p.log)
	if err != nil {
		p.markError(ctx, schemeCode, err)
		return fmt.Errorf("parse response: %w", err)
	}

	log.Info("fetched nav data", "rows", len(navRows))

	// Ensure navRows are sorted ascending by date before persisting
	sort.Slice(navRows, func(i, j int) bool {
		return navRows[i].NavDate.Before(navRows[j].NavDate)
	})

	// Atomically: upsert scheme + bulk insert nav_data + update sync_state
	if err := p.store.SaveSyncResult(ctx, scheme, navRows); err != nil {
		p.markError(ctx, schemeCode, err)
		return fmt.Errorf("save sync result: %w", err)
	}

	log.Info("sync complete", "inserted_rows", len(navRows))

	// Compute analytics synchronously. ComputeAll retries up to 3× internally.
	// On failure: non-fatal for sync, but analytics_status is set to 'error'
	// so RepairAnalytics will retry on the next startup or daily sync cycle.
	if err := p.analytics.ComputeAll(ctx, schemeCode); err != nil {
		log.Error("analytics computation failed — will be repaired on next run", "error", err)
		_ = p.store.MarkAnalyticsError(ctx, schemeCode, err.Error())
	} else {
		_ = p.store.MarkAnalyticsDone(ctx, schemeCode)
	}

	return nil
}

// RepairAnalytics computes and persists analytics for every scheme that has
// fully synced NAV data but is missing computed analytics (analytics_status
// != 'done'). This covers two failure modes:
//
//  1. Server crash between SaveSyncResult and ComputeAll — data is safe but
//     analytics was never written. sync_state.status='done' but
//     analytics_status='pending', so it is invisible to the backfill queue.
//
//  2. ComputeAll returned an error after a previous run — analytics_status
//     is 'error'; the scheme is not in the sync queue but needs a retry.
//
// RepairAnalytics is called on startup (after backfill) and at the start of
// every daily sync so no scheme is permanently left without analytics.
func (p *Pipeline) RepairAnalytics(ctx context.Context) {
	log := p.log.With("phase", "analytics_repair")

	codes, err := p.store.GetSchemesNeedingAnalytics(ctx)
	if err != nil {
		log.Error("failed to query schemes needing analytics — repair skipped", "error", err)
		return
	}
	if len(codes) == 0 {
		log.Info("all schemes have up-to-date analytics")
		return
	}

	log.Info("starting analytics repair", "count", len(codes))
	repaired, failed := 0, 0

	for _, code := range codes {
		if ctx.Err() != nil {
			log.Warn("analytics repair interrupted by context cancellation",
				"repaired", repaired, "failed", failed)
			return
		}
		if err := p.analytics.ComputeAll(ctx, code); err != nil {
			log.Error("analytics repair failed", "scheme_code", code, "error", err)
			_ = p.store.MarkAnalyticsError(ctx, code, err.Error())
			failed++
		} else {
			_ = p.store.MarkAnalyticsDone(ctx, code)
			repaired++
		}
	}

	log.Info("analytics repair complete", "repaired", repaired, "failed", failed)
}

// markError is a helper that logs and persists the error state.
func (p *Pipeline) markError(ctx context.Context, schemeCode string, err error) {
	msg := err.Error()
	p.log.Error("sync failed", "scheme_code", schemeCode, "error", msg)
	if dbErr := p.store.MarkError(ctx, schemeCode, msg); dbErr != nil {
		p.log.Error("failed to persist error state", "scheme_code", schemeCode, "error", dbErr)
	}
}
