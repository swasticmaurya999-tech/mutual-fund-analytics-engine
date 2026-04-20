# DESIGN_DECISIONS.md — Mutual Fund Analytics Engine

---

## 1. Rate Limiting Strategy and Proof of Correctness

mfapi.in enforces three **simultaneous, independent** limits — 2 req/s, 50 req/min, and 300 req/hr. Violating any one of them, even while satisfying the other two, triggers a block.

**Algorithm chosen: Composite Triple Token Bucket.**
Each limit gets its own `tokenBucket` (continuous refill, capacity-capped). A `CompositeRateLimiter` wraps all three, and every outbound call must acquire a token from **all three** before the HTTP request is issued:

```
perSec  → capacity=2,   refillRate=2 tokens/s
perMin  → capacity=50,  refillRate=50/60 ≈ 0.833 tokens/s
perHour → capacity=300, refillRate=300/3600 ≈ 0.083 tokens/s
```

Token bucket was chosen over fixed-window counters (vulnerable to boundary bursts — 2 requests at :59 and 2 more at :00 = 4 in one second undetected) and leaky bucket (enforces strict constant pacing, penalising controlled bursts the API explicitly allows).

**Proof of correctness:** The audit table `request_log` records every outbound call with a timestamp. Rate compliance is verifiable at any time:

```sql
-- Per-second: no second may exceed 2 hits
SELECT DATE_TRUNC('second', requested_at), COUNT(*)
FROM request_log GROUP BY 1 HAVING COUNT(*) > 2;

-- Per-minute: no minute may exceed 50 hits
SELECT DATE_TRUNC('minute', requested_at), COUNT(*)
FROM request_log GROUP BY 1 HAVING COUNT(*) > 50;

-- Per-hour: no hour may exceed 300 hits
SELECT DATE_TRUNC('hour', requested_at), COUNT(*)
FROM request_log GROUP BY 1 HAVING COUNT(*) > 300;
```

Token state is **persisted to PostgreSQL on every consumption**. On restart, elapsed downtime is converted back to refilled tokens (`MIN(capacity, saved + elapsed × rate)`), so the limits are never violated across process restarts.

---

## 2. How Three Concurrent Limits Are Coordinated

Tokens are acquired sequentially in **slowest-refill-first order**:

```go
c.perHour.wait(ctx)  // most likely to exhaust first → check first
c.perMin.wait(ctx)
c.perSec.wait(ctx)
// all three granted → request is safe
```

This order matters for efficiency: if the hourly budget is exhausted (likely during a bulk backfill), the goroutine blocks immediately without consuming per-minute or per-second tokens that would then be wasted. The effective throughput at any moment is controlled by the **most constrained** bucket — all three constraints hold simultaneously by construction.

When a HTTP 429 is received (rate limit violated despite the limiter), `DrainAll()` zeroes all three buckets atomically in both memory and the database, then the client waits 65 seconds (clearing the per-minute block window) before retrying. Token state (`per_sec`, `per_min`, `per_hr`) is persisted to the `rate_limiter_state` table instead of Redis or an in-memory store — with only 10 schemes and a low request rate, the write volume is minimal and PostgreSQL (already in use) handles it with zero additional operational overhead. Adding Redis purely for this would be over-engineering at this scale.

---

## 3. Backfill Orchestration Within Quota Constraints

**Single API call per scheme.** `GET /mf/{code}?startDate=...&endDate=...` returns the complete NAV history for the requested range in one response. A 10-year backfill for one scheme = **1 API request**, not ~2,520 individual day-requests. Total backfill cost: **10 requests** for all 10 schemes — comfortably within the 300/hr quota.

**No message queues.** A queue-based worker approach (fetch → enqueue → workers process and persist) was considered and deliberately rejected. With only 10 scheme codes, the added complexity of a message broker (RabbitMQ, Kafka) would far exceed any benefit. Instead, `sync_state` acts as a lightweight DB-backed job queue: each scheme has a `status` (`pending → running → done / error`) and the backfill loop uses `FOR UPDATE SKIP LOCKED` to atomically claim the next available job. This gives the same concurrency-safe, resumable behaviour with no external dependency.

**No chunked fetching.** The API returns complete date-range data in a single payload. For 10 years of NAV data (~2,500 rows × ~20 bytes each ≈ ~50 KB per scheme), holding one scheme's payload in memory before writing is entirely safe. Chunking would introduce complexity (pagination state, partial write coordination) with no practical benefit at this data volume.

**Checkpoint-based resumability.** The crash-recovery cursor is `MAX(nav_date)` from the `nav_data` table — the actual committed data, not a separate progress field. On any restart, `SyncScheme` reads this and fetches only from `last_committed_date + 1`. If no data exists yet, it fetches from `today − 10 years`. This same logic handles both full backfill and daily incremental sync, so there is no separate code path to maintain.

---

## 4. Storage Schema for Time-Series NAV Data

**PostgreSQL over time-series or NoSQL databases.** NAV data is relational by nature: each data point belongs to a scheme, schemes belong to AMCs, analytics join across both. The access pattern is `WHERE scheme_code = ? ORDER BY nav_date ASC` — a standard range scan that any relational database handles efficiently. PostgreSQL's `NUMERIC(20,5)` type stores NAVs without floating-point rounding errors (the API returns values like `"190.52420"` — `FLOAT` would corrupt the last digit). Time-series databases optimise for very high write throughput and time-bucketed aggregation queries; neither applies here (10 schemes, ~2,500 rows each, no time-bucketed aggregation). SQLite was ruled out because it lacks `FOR UPDATE SKIP LOCKED` (needed for the job queue) and has limited concurrency under concurrent reads and writes.

**Key tables and their roles:**

| Table | Purpose | ~Rows |
|---|---|---|
| `schemes` | Fund metadata (name, AMC, category, ISIN) | 10 |
| `nav_data` | Daily NAV time-series; PRIMARY KEY `(scheme_code, nav_date)` | ~25,000 |
| `analytics` | Pre-computed metrics per scheme × window | 40 |
| `sync_state` | Job queue + crash-recovery checkpoint | 10 |
| `rate_limiter_state` | Token bucket persistence across restarts | 3 |
| `request_log` | Immutable audit trail of every outbound API call | grows daily |

Bulk NAV inserts use PostgreSQL's `COPY` protocol (the fastest available path) via a staging temp table, then `INSERT ... ON CONFLICT DO NOTHING` to merge idempotently. The three-step write (upsert scheme → bulk insert NAVs → update sync_state to `done`) runs inside a single transaction, so a mid-write crash leaves `sync_state` in `running` and the checkpoint unchanged — the next startup resumes from exactly where data was last safely committed.

---

## 5. Pre-computation vs On-Demand Trade-offs

**All analytics are pre-computed at sync time and read directly from the `analytics` table at query time.**

Computing rolling returns, max drawdown, and CAGR distribution requires a full O(n log n) pass over ~2,500 NAV rows per scheme per window. Doing this on every API request would add 50–100ms of CPU time per call — unacceptable given the <200ms requirement — and would create redundant computation since NAV data does not change between daily syncs.

Pre-computation means:
- `GET /funds/{code}/analytics` is a single indexed row read (sub-millisecond at DB level)
- `GET /funds/rank` is a single JOIN + ORDER BY across 40 pre-sorted rows
- No lock contention between readers and the analytics engine writer

**Trade-off acknowledged:** Pre-computed analytics lag the latest NAV by one sync cycle. For a mutual fund platform (NAV published once per day, analytics refreshed at the same cadence), this is entirely acceptable — the data contract is daily, not real-time.

No application-level cache (Redis, in-memory map) was added. The `analytics` table has 40 rows and `schemes` has 10 — both fit in PostgreSQL's buffer cache and are effectively served from memory already. TTL management and invalidation logic would add complexity with zero measurable latency benefit at this data volume.

---

## 6. Handling Schemes with Insufficient History

Not every scheme has 10 years of NAV data. A scheme launched in 2022 cannot produce a valid 5-year or 10-year rolling analysis.

**Strategy: compute what is possible, surface the gap explicitly.**

The analytics engine checks `len(navs) < windowDays` before computing rolling periods. If there is insufficient history for a given window, the analytics record is written with `insufficient_data = true` and `rolling_min / rolling_max / rolling_median / cagr_*` set to `NULL`. **Max drawdown is still computed** over whatever history is available — the worst peak-to-trough decline is meaningful even over a short series.

The API response for an insufficient-data window includes:
- `"insufficient_data": true`
- `"insufficient_data_reason"`: a human-readable string stating exactly how many trading days are available, how many are required, and which windows *can* be used

```json
{
  "insufficient_data": true,
  "insufficient_data_reason": "Not enough NAV history for a 10Y rolling window.
    Need at least 2520 trading days; this fund has 756 (available since 2021-03-15).
    Try one of the available windows: 1Y, 3Y."
}
```

This ensures the API never silently returns empty or misleading analytics — callers always know whether null values mean "no data" or "insufficient history" and what alternatives exist.

---

## Notes: Other Important Design Decisions

**Startup sequencing — HTTP server deferred until backfill is complete.** The HTTP server does not accept requests until the initial backfill and a `RepairAnalytics` pass finish. This guarantees the API always serves complete data from the first request and is never caught in a "data loading" state. A `RepairAnalytics` pass also runs at the start of every daily sync to catch the failure mode where the server crashed between `SaveSyncResult` (data committed) and `ComputeAll` (analytics written) — without this, a scheme could be permanently stuck with synced NAV data but no analytics, invisible to both the backfill queue (already `done`) and the API.

**Two-tier state tracking.** `sync_state` tracks both the NAV sync lifecycle (`status: pending/running/done/error`) and the analytics lifecycle (`analytics_status: pending/done/error`) independently. This separation was added after identifying the crash window between data persistence and analytics computation — a single status column could not distinguish "data synced, analytics missing" from "data not yet synced".

**Interface-based dependency injection for testability.** HTTP handlers depend on a `DataStore` interface rather than `*store.Store` directly. All handler tests use an in-process `mockStore` with no database — tests run in milliseconds. A compile-time assertion (`var _ api.DataStore = (*store.Store)(nil)`) prevents the real implementation from drifting out of sync with the interface silently.

**Structured JSON logging and request audit trail.** Every outbound API call is logged to `request_log` with full URL, HTTP status, latency, retry count, and error message. This provides an immutable, queryable audit trail that can prove rate-limit compliance after the fact and assists debugging without requiring a log aggregation service.

**Test strategy — covering all four required scenarios without a live database.**

*Rate limiter correctness* is tested in `internal/ratelimit/limiter_test.go` (white-box, same package) with 8 focused tests: each of the three limits is verified individually by timing actual wait durations, the composite limiter is tested by draining one bucket and confirming the whole `Wait()` blocks, concurrent access is verified with 20 goroutines and an atomic counter proving no double-grant (run with `-race`), state persistence is verified algebraically against the offline-refill formula, and drain-on-429 is confirmed by reading `Tokens()` after `DrainAll`.

*Analytics correctness* is tested in `internal/analytics/engine_test.go` with 10 tests against pure functions — no I/O required. Every expected value is derived by hand and documented inline so a reviewer can audit the math without running code. Key edge cases covered: empty input slices, zero-NAV defensive skips, window size equal to or larger than the series (must return 0 periods), insufficient history for a window (flag set, drawdown still computed), and a complete end-to-end 10-year NAV series where max drawdown (−38.46%), all 6 rolling returns, and the median (≈32.86%) are all manually pre-computed.

*Pipeline resumability after crash* is tested in `internal/api/pipeline_test.go` using a mock `sync_state` that pre-populates all four lifecycle states (done/pending/running/error). The test asserts that `GET /sync/status` surfaces each state accurately in both the per-scheme array and the aggregate summary — giving an operator a complete picture of which schemes survived, which stalled, and which need attention. `POST /sync/trigger` is verified to return 202 immediately (non-blocking), confirming the recovery flow can be initiated without waiting for sync to complete.

*API response time* is enforced in every test through the `do()` helper in `handlers_test.go`, which wraps every `h.ServeHTTP()` call with a `time.Since()` assertion that fails the test if the response exceeds 200ms. This applies to all subtests including validation errors and DB-error paths — not only happy paths. Since all handlers read from pre-computed tables (no analytics computed at request time), the <200ms bound is trivially met in production and demonstrably met in tests against the in-process mock.
