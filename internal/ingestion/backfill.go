package ingestion

import (
	"context"
	"log/slog"
)

// RunBackfill processes all schemes that need syncing (status: pending/error/stale-running).
// Uses FOR UPDATE SKIP LOCKED so it is safe to call from multiple goroutines or processes.
// Blocks until all pending work is done or the context is cancelled.
func (p *Pipeline) RunBackfill(ctx context.Context) {
	log := p.log.With("phase", "backfill")
	log.Info("starting backfill")

	processed := 0
	failed := 0

	for {
		if ctx.Err() != nil {
			log.Info("backfill cancelled", "processed", processed, "failed", failed)
			return
		}

		code, err := p.store.AcquireNextPendingJob(ctx)
		if err != nil {
			log.Error("failed to acquire next job", "error", err)
			return
		}
		if code == "" {
			// No more pending jobs
			break
		}

		log.Info("processing scheme", "scheme_code", code, "processed_so_far", processed)
		if err := p.SyncScheme(ctx, code); err != nil {
			log.Error("scheme sync failed", "scheme_code", code, "error", err)
			failed++
		} else {
			processed++
		}
	}

	if failed > 0 {
		log.Warn("backfill completed with errors",
			"processed", processed,
			"failed", failed,
		)
	} else {
		log.Info("backfill complete", "processed", processed)
	}
}

// RunBackfillSync is the synchronous version used during startup.
// The HTTP server should not start until this returns.
func (p *Pipeline) RunBackfillSync(ctx context.Context, log *slog.Logger) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.RunBackfill(ctx)
	}()
	select {
	case <-done:
		log.Info("initial backfill phase done")
	case <-ctx.Done():
		log.Warn("startup backfill interrupted by context cancellation")
	}
}
