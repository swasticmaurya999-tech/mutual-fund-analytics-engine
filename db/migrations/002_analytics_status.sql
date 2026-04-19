-- =============================================================================
-- Migration 002: Analytics lifecycle tracking
--
-- Problem: sync_state tracks whether NAV data was fetched (status = 'done')
-- but has no record of whether analytics were subsequently computed.
-- If the server crashes between SaveSyncResult and ComputeAll, the scheme
-- stays 'done' in sync_state and analytics is silently missing — with no
-- way to detect or repair it without a full table scan of analytics.
--
-- Fix: Add analytics_status / analytics_error / analytics_computed_at to
-- sync_state so the RepairAnalytics pass knows exactly which schemes need
-- analytics re-computed after a crash or transient failure.
--
-- Lifecycle:
--   pending  → initial state; scheme synced but analytics not yet run
--   done     → ComputeAll succeeded and all 4 windows were persisted
--   error    → ComputeAll failed; scheme will be retried by RepairAnalytics
--
-- Idempotent: ADD COLUMN IF NOT EXISTS is safe to run multiple times.
-- =============================================================================

ALTER TABLE sync_state
    ADD COLUMN IF NOT EXISTS analytics_status       TEXT        NOT NULL DEFAULT 'pending',
    ADD COLUMN IF NOT EXISTS analytics_error        TEXT,
    ADD COLUMN IF NOT EXISTS analytics_computed_at  TIMESTAMPTZ;

-- Add CHECK constraint only if it does not already exist.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.check_constraints
        WHERE  constraint_schema = current_schema()
          AND  constraint_name   = 'sync_state_analytics_status_check'
    ) THEN
        ALTER TABLE sync_state
            ADD CONSTRAINT sync_state_analytics_status_check
            CHECK (analytics_status IN ('pending', 'done', 'error'));
    END IF;
END $$;

-- Partial index: fast lookup of schemes that need an analytics repair pass.
-- The WHERE clause ensures Postgres only indexes the rows that matter.
CREATE INDEX IF NOT EXISTS idx_sync_state_analytics_pending
    ON sync_state (scheme_code)
    WHERE status = 'done' AND analytics_status != 'done';

-- NOTE: existing rows get analytics_status = 'pending' (the column DEFAULT).
-- On the next startup, RepairAnalytics will detect them and compute the
-- missing analytics automatically — no manual backfill step required.
