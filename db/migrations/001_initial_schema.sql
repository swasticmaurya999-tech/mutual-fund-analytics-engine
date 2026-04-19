-- =============================================================================
-- Mutual Fund Analytics Service — Full Database Schema
-- Run this entire file once in Supabase SQL Editor to set up the database.
-- =============================================================================


-- =============================================================================
-- 1. SCHEMES
--    Stores metadata for the 10 tracked mutual fund schemes.
--    Populated from the `meta` block of GET /mf/{code} response.
--
--    name, amc, category, scheme_type are nullable on purpose:
--    On startup, the service seeds scheme codes first (from seeds.go),
--    then populates metadata after the first API call. This avoids a
--    chicken-and-egg problem where sync_state needs schemes to exist
--    before we've fetched their metadata.
-- =============================================================================
CREATE TABLE IF NOT EXISTS schemes (
    code                    TEXT        PRIMARY KEY,        -- scheme_code from API (e.g. "120505")
    name                    TEXT,                           -- scheme_name from API meta (populated on first sync)
    amc                     TEXT,                           -- fund_house from API meta
    category                TEXT,                           -- scheme_category (e.g. "Equity Scheme - Mid Cap Fund")
    scheme_type             TEXT,                           -- e.g. "Open Ended Schemes"
    isin_growth             TEXT,                           -- isin_growth from API meta (nullable)
    isin_div_reinvestment   TEXT,                           -- isin_div_reinvestment from API meta (nullable)
    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Indexes for GET /funds filtering by category and amc
CREATE INDEX IF NOT EXISTS idx_schemes_category ON schemes (category);
CREATE INDEX IF NOT EXISTS idx_schemes_amc      ON schemes (amc);


-- =============================================================================
-- 2. NAV_DATA
--    Daily NAV time-series. Core data store. Source of truth for all analytics.
--    One row per (scheme, trading day). ~2500 rows per scheme = ~25,000 total.
--
--    NUMERIC(20,5) used instead of FLOAT — financial data must never lose
--    precision to floating point rounding errors. API returns NAVs with up
--    to 5 decimal places (e.g. "190.52420").
--
--    Dates come from the API as "DD-MM-YYYY" — must be converted to DATE
--    before inserting.
-- =============================================================================
CREATE TABLE IF NOT EXISTS nav_data (
    scheme_code     TEXT            NOT NULL REFERENCES schemes(code) ON DELETE CASCADE,
    nav_date        DATE            NOT NULL,
    nav             NUMERIC(20, 5)  NOT NULL,

    PRIMARY KEY (scheme_code, nav_date)
);

-- Primary access pattern: fetch all NAVs for a scheme sorted ascending (analytics engine)
-- Also used for MAX(nav_date) checkpoint lookups (pipeline resumability)
CREATE INDEX IF NOT EXISTS idx_nav_data_scheme_date_asc
    ON nav_data (scheme_code, nav_date ASC);

CREATE INDEX IF NOT EXISTS idx_nav_data_scheme_date_desc
    ON nav_data (scheme_code, nav_date DESC);


-- =============================================================================
-- 3. ANALYTICS
--    Pre-computed metrics per scheme per window (1Y/3Y/5Y/10Y).
--    Written after every sync, read at query time — zero computation in API.
--    Schema directly mirrors the example analytics response in the assignment.
--
--    All return/drawdown/CAGR values stored as percentages (22.3 = 22.3%).
--    NULL on any metric = insufficient NAV history for that window.
--
--    Rolling returns: slide a window of N trading days across full NAV history,
--    compute annualized return for each position → collect stats across positions.
--
--    Volatility: standard deviation of rolling returns (mentioned in assignment
--    background). Not in example response but included for API extensibility.
-- =============================================================================
CREATE TABLE IF NOT EXISTS analytics (
    scheme_code             TEXT            NOT NULL REFERENCES schemes(code) ON DELETE CASCADE,
    "window"                TEXT            NOT NULL    CHECK ("window" IN ('1Y', '3Y', '5Y', '10Y')),

    -- Rolling returns distribution (across all rolling periods in the window)
    -- Maps to: response.rolling_returns.{min, max, median, p25, p75}
    rolling_min             NUMERIC(10, 4),
    rolling_max             NUMERIC(10, 4),
    rolling_median          NUMERIC(10, 4),
    rolling_p25             NUMERIC(10, 4),
    rolling_p75             NUMERIC(10, 4),

    -- Volatility: std deviation of rolling returns — not in assignment example
    -- response but mentioned in background; stored for extensibility
    rolling_volatility      NUMERIC(10, 4),

    -- Max drawdown: worst peak-to-trough % decline over the full window
    -- Maps to: response.max_drawdown (always a negative number, e.g. -32.1)
    max_drawdown            NUMERIC(10, 4),

    -- CAGR distribution (min/max/median across all rolling periods)
    -- Maps to: response.cagr.{min, max, median}
    cagr_min                NUMERIC(10, 4),
    cagr_max                NUMERIC(10, 4),
    cagr_median             NUMERIC(10, 4),

    -- Data availability block — maps to: response.data_availability.*
    data_start              DATE,                       -- earliest NAV date used in computation
    data_end                DATE,                       -- latest NAV date used in computation
    total_days              INTEGER,                    -- calendar days between data_start and data_end
    nav_data_points         INTEGER,                    -- actual trading day rows available
    rolling_periods_analyzed INTEGER,                  -- how many rolling windows were computed

    -- Set to TRUE when available history is shorter than the requested window.
    -- Analytics are still computed over available data; API surfaces this flag.
    insufficient_data       BOOLEAN     NOT NULL DEFAULT FALSE,

    computed_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    PRIMARY KEY (scheme_code, "window")
);

-- Index for GET /funds/rank — filters by category (via JOIN) and sorts by rolling_median or max_drawdown
CREATE INDEX IF NOT EXISTS idx_analytics_window ON analytics ("window");


-- =============================================================================
-- 4. SYNC_STATE
--    Pipeline job queue + crash recovery checkpoint. One row per scheme.
--    Drives both backfill (initial) and incremental daily sync.
--
--    Status lifecycle:
--      pending → running → done
--                       ↘ error  (retried: error → pending on next run)
--
--    Stale running detection: if status='running' AND updated_at < NOW()-10min,
--    the process crashed — treat as pending and retry.
--
--    last_nav_date is the crash-recovery cursor:
--      NULL          → never synced, fetch from today-10years
--      some date D   → fetch from D+1 to today (only the missing tail)
--
--    latest_nav is denormalized here so GET /funds/rank can get current_nav
--    without an extra JOIN to nav_data (which would need a MAX subquery).
-- =============================================================================
CREATE TABLE IF NOT EXISTS sync_state (
    scheme_code     TEXT        PRIMARY KEY REFERENCES schemes(code) ON DELETE CASCADE,
    status          TEXT        NOT NULL DEFAULT 'pending'
                                CHECK (status IN ('pending', 'running', 'done', 'error')),
    last_nav_date   DATE,                               -- checkpoint: max nav_date successfully committed
    latest_nav      NUMERIC(20, 5),                     -- NAV value on last_nav_date (for ranking response)
    nav_count       INTEGER     NOT NULL DEFAULT 0,     -- total rows stored for this scheme
    error_msg       TEXT,                               -- populated when status = 'error'
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Used by FOR UPDATE SKIP LOCKED to pick next pending/stale job
CREATE INDEX IF NOT EXISTS idx_sync_state_status_updated
    ON sync_state (status, updated_at ASC);


-- =============================================================================
-- 5. RATE_LIMITER_STATE
--    Persists token bucket levels for all 3 rate limiters across process restarts.
--    Exactly 3 rows, never more.
--
--    On restart:
--      elapsed  = NOW() - last_updated
--      refilled = elapsed_seconds * (capacity / window_seconds)
--      restored = MIN(tokens + refilled, capacity)
--
--    This guarantees rate limits are never violated across restarts because
--    the restored token count reflects what was actually available at restart time.
--
--    DOUBLE PRECISION (not REAL) — REAL has only 6 significant digits which
--    is fine for token counts but DOUBLE PRECISION is more correct.
-- =============================================================================
CREATE TABLE IF NOT EXISTS rate_limiter_state (
    limiter_id      TEXT                PRIMARY KEY,    -- 'per_sec' | 'per_min' | 'per_hr'
    tokens          DOUBLE PRECISION    NOT NULL,       -- token count at last_updated
    capacity        DOUBLE PRECISION    NOT NULL,       -- max tokens (2, 50, 300)
    refill_rate     DOUBLE PRECISION    NOT NULL,       -- tokens per second (2/1, 50/60, 300/3600)
    last_updated    TIMESTAMPTZ         NOT NULL
);

-- Seed all 3 limiters at full capacity
INSERT INTO rate_limiter_state (limiter_id, tokens, capacity, refill_rate, last_updated) VALUES
    ('per_sec', 2.0,   2.0,   2.0,               NOW()),   -- 2 tokens/sec
    ('per_min', 50.0,  50.0,  0.8333333333333334, NOW()),  -- 50/60 tokens/sec
    ('per_hr',  300.0, 300.0, 0.0833333333333333, NOW())   -- 300/3600 tokens/sec
ON CONFLICT (limiter_id) DO NOTHING;


-- =============================================================================
-- 6. REQUEST_LOG
--    Immutable append-only audit log of every outbound API call to mfapi.in.
--    Provides provable evidence of rate limit compliance.
--
--    Compliance verification queries:
--
--    -- Prove per-second limit (max 2/sec):
--    SELECT DATE_TRUNC('second', requested_at) AS sec, COUNT(*) AS hits
--    FROM request_log GROUP BY 1 HAVING COUNT(*) > 1 ORDER BY 2 DESC LIMIT 20;
--
--    -- Prove per-minute limit (max 50/min):
--    SELECT DATE_TRUNC('minute', requested_at) AS min, COUNT(*) AS hits
--    FROM request_log GROUP BY 1 ORDER BY 2 DESC LIMIT 20;
--
--    -- Prove per-hour limit (max 300/hr):
--    SELECT DATE_TRUNC('hour', requested_at) AS hr, COUNT(*) AS hits
--    FROM request_log GROUP BY 1 ORDER BY 2 DESC LIMIT 20;
-- =============================================================================
CREATE TABLE IF NOT EXISTS request_log (
    id              BIGSERIAL       PRIMARY KEY,
    scheme_code     TEXT,                           -- which scheme this call targeted (NULL for non-scheme calls)
    endpoint        TEXT            NOT NULL,       -- path called, e.g. '/mf/120505'
    full_url        TEXT            NOT NULL,       -- full URL including query params
    requested_at    TIMESTAMPTZ     NOT NULL DEFAULT NOW(),
    http_status     INTEGER,                        -- 200, 429, 500 etc.
    duration_ms     INTEGER,                        -- round-trip latency in ms
    error_msg       TEXT,                           -- populated on error or non-200 responses
    retry_attempt   INTEGER         NOT NULL DEFAULT 0  -- 0 = first attempt, 1+ = retries
);

CREATE INDEX IF NOT EXISTS idx_request_log_requested_at
    ON request_log (requested_at DESC);

CREATE INDEX IF NOT EXISTS idx_request_log_scheme_time
    ON request_log (scheme_code, requested_at DESC);


-- =============================================================================
-- HELPER: auto-update updated_at columns
-- =============================================================================
CREATE OR REPLACE FUNCTION update_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE TRIGGER trg_schemes_updated_at
    BEFORE UPDATE ON schemes
    FOR EACH ROW EXECUTE FUNCTION update_updated_at();

CREATE OR REPLACE TRIGGER trg_sync_state_updated_at
    BEFORE UPDATE ON sync_state
    FOR EACH ROW EXECUTE FUNCTION update_updated_at();
