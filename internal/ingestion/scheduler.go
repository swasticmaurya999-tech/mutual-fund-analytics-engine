package ingestion

import (
	"context"
	"time"
)

// Scheduler runs the daily incremental sync at a fixed time each day.
type Scheduler struct {
	pipeline *Pipeline
}

// NewScheduler creates a Scheduler.
func NewScheduler(p *Pipeline) *Scheduler {
	return &Scheduler{pipeline: p}
}

// Start launches the daily sync loop. Blocks until ctx is cancelled.
// Syncs all 10 schemes every day after market close (20:00 IST = 14:30 UTC).
//
// On startup, it checks the DB to detect whether today's scheduled sync was
// missed (e.g. server was down during 8pm IST) and runs a catch-up immediately
// if so. This relies entirely on sync_state as the source of truth — no
// arbitrary thresholds needed.
func (s *Scheduler) Start(ctx context.Context) {
	log := s.pipeline.log.With("component", "scheduler")

	if s.wasTodaySyncMissed(ctx) {
		log.Info("today's scheduled sync was missed — running catch-up immediately")
		s.runDailySync(ctx)
	}

	for {
		next := nextRunTime()
		log.Info("next daily sync scheduled", "at", next.Format(time.RFC3339))

		select {
		case <-ctx.Done():
			log.Info("scheduler stopped")
			return
		case <-time.After(time.Until(next)):
			s.runDailySync(ctx)
		}
	}
}

// wasTodaySyncMissed reports whether the 14:30 UTC (20:00 IST) daily sync
// window has already passed today but at least one scheme was not successfully
// synced after that time. It uses sync_state as the sole source of truth so
// no threshold guessing is required.
//
// Returns false (safe default) if the scheduled time hasn't passed yet, or if
// the DB cannot be reached.
func (s *Scheduler) wasTodaySyncMissed(ctx context.Context) bool {
	now := time.Now().UTC()
	todayScheduled := time.Date(now.Year(), now.Month(), now.Day(), 14, 30, 0, 0, time.UTC)

	if now.Before(todayScheduled) {
		return false // scheduled window hasn't arrived yet — nothing missed
	}

	states, err := s.pipeline.store.GetAllSyncStates(ctx)
	if err != nil {
		s.pipeline.log.Warn("could not read sync states for catch-up check — skipping", "error", err)
		return false // fail safe: don't fire an unexpected sync on DB errors
	}

	for _, st := range states {
		// Missed if: not successfully done, OR done before today's scheduled time
		if st.Status != "done" || st.UpdatedAt.Before(todayScheduled) {
			return true
		}
	}
	return false
}

// nextRunTime returns the next 14:30 UTC (20:00 IST) from now.
func nextRunTime() time.Time {
	now := time.Now().UTC()
	next := time.Date(now.Year(), now.Month(), now.Day(), 14, 30, 0, 0, time.UTC)
	if now.After(next) {
		next = next.Add(24 * time.Hour)
	}
	return next
}

// runDailySync iterates over all tracked schemes and syncs each one.
// SyncScheme internally uses the MAX(nav_date) checkpoint, so only new
// NAV data (since the last sync) is fetched — not the full 10-year history.
func (s *Scheduler) runDailySync(ctx context.Context) {
	log := s.pipeline.log.With("component", "daily_sync")
	log.Info("running daily sync", "schemes", len(TrackedSchemes))

	// Repair any analytics that were missed from previous cycles (crash recovery
	// or transient failures). Runs before new syncs so the repair pass does not
	// compete with fresh data being written.
	s.pipeline.RepairAnalytics(ctx)
	if ctx.Err() != nil {
		return
	}

	// Reset any schemes stuck in 'error' so they get a fresh attempt
	if err := s.pipeline.store.ResetErrorsToPending(ctx); err != nil {
		log.Warn("failed to reset error schemes", "error", err)
	}

	success := 0
	failed := 0
	for _, code := range TrackedSchemes {
		if ctx.Err() != nil {
			log.Warn("daily sync interrupted", "remaining", len(TrackedSchemes)-success-failed)
			return
		}

		if err := s.pipeline.SyncScheme(ctx, code); err != nil {
			log.Error("daily sync failed for scheme", "scheme_code", code, "error", err)
			failed++
		} else {
			success++
		}
	}

	log.Info("daily sync complete", "success", success, "failed", failed)
}
