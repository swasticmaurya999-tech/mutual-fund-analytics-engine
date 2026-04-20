# Mutual Fund Analytics Engine

A production-ready Go backend service that ingests historical NAV (Net Asset Value) data from [mfapi.in](https://api.mfapi.in), computes rolling performance analytics across multiple time windows, and serves fast ranking and analytics queries over a REST API.

---

## Features

- **Data pipeline** — automatic 10-year backfill on first startup, daily incremental sync at 20:00 IST, crash-resumable from the last committed checkpoint
- **Rate limiter** — composite triple token-bucket enforcing 2 req/s + 50 req/min + 300 req/hr simultaneously, with state persisted across restarts
- **Analytics engine** — pre-computes rolling returns, max drawdown, and CAGR distribution for 1Y / 3Y / 5Y / 10Y windows per fund
- **REST API** — 6 endpoints: fund listing/filtering, fund detail, analytics by window, fund ranking, sync trigger, sync status
- **OpenAPI docs** — interactive Swagger UI served at `/docs`
- **Fully tested** — rate limiter, analytics math, and all API handlers tested without a live database

---

## Tracked Schemes

10 schemes across 5 AMCs and 2 categories:

| AMC | Mid Cap (Direct Growth) | Small Cap (Direct Growth) |
|---|---|---|
| Axis Mutual Fund | 120505 | 125354 |
| HDFC Mutual Fund | 118989 | 130503 |
| ICICI Prudential | 120381 | 120591 |
| SBI Mutual Fund | 119716 | 125497 |
| Kotak Mahindra | 119775 | 120164 |

---

## Prerequisites

- **Go 1.21+** — see [Installation](#installing-go) below if not already installed
- **Git** — to clone the repository
- A **PostgreSQL database** (Supabase free tier works)

### Installing Go

If Go is not installed on your system:

**Windows:**
1. Download the installer from [go.dev/dl](https://go.dev/dl/) (pick the `.msi` for Windows)
2. Run the installer — it adds Go to your `PATH` automatically
3. Open a **new** terminal (Command Prompt or PowerShell) and verify:
   ```cmd
   go version
   ```
   You should see something like `go version go1.22.2 windows/amd64`.

**macOS / Linux:**
```bash
# macOS (Homebrew)
brew install go

# Ubuntu/Debian
sudo apt update && sudo apt install golang-go

# Verify
go version
```

---

## Setup

### 1. Clone the repository

```cmd
git clone <repo-url>
cd mutual-fund-analytics-engine
```

### 2. Configure environment

**Windows (Command Prompt):**
```cmd
copy .env.example .env
```

**Windows (PowerShell):**
```powershell
Copy-Item .env.example .env
```

**macOS / Linux:**
```bash
cp .env.example .env
```

Now edit `.env` with your database credentials:

```env
DATABASE_URL=postgresql://postgres:YOUR_PASSWORD@aws-0-ap-south-1.pooler.supabase.com:5432/postgres
PORT=8080
LOG_LEVEL=info
```

> **Note:** Use the Supabase **Session Pooler** connection string (found under **Connect → Session mode** in your Supabase dashboard). The direct connection URL resolves to IPv6, which may not work on all ISPs. The session pooler URL resolves to IPv4 and works everywhere.

> **Note:** If your database password contains special characters like `[`, `]`, `@`, `#`, or `%`, they must be URL-encoded (e.g. `[` → `%5B`, `]` → `%5D`).

### 3. Install Go dependencies

```cmd
go mod download
```

This downloads all required Go packages (`chi`, `pgx`, `godotenv`) specified in `go.mod`.

### 4. Run database migration

Open your Supabase dashboard → **SQL Editor** → **New Query**, then paste the contents of `db/migrations/001_initial_schema.sql` and click **Run**.

To view the file contents locally:
```cmd
type db\migrations\001_initial_schema.sql
```

The migration is idempotent (`IF NOT EXISTS`, `ON CONFLICT DO NOTHING`) and safe to re-run.

> **If you are using the provided `.env` file:** The database already has the schema and data pre-loaded. You can **skip this step** entirely — the service will connect and find everything ready.

### 5. Start the service

```cmd
go run ./cmd/server
```

On first startup the service will:
1. Connect to the database and verify the schema
2. Seed all 10 scheme codes into `schemes` and `sync_state`
3. Run a full 10-year backfill for each scheme (respecting rate limits)
4. Pre-compute analytics for all 4 windows
5. Start the HTTP server on the configured port

> **Note:** The backfill runs in the background — the HTTP server starts immediately and is ready to serve requests while data syncs behind the scenes. If the database already has data from a previous run, the checkpoint logic detects already-synced schemes and skips them, so subsequent starts are fast.

---

## API Reference

All responses are `Content-Type: application/json`. Interactive docs are available at `GET /docs`.

### `GET /health`

Liveness check.

```json
{ "status": "ok" }
```

---

### `GET /funds`

List all tracked funds. Optionally filter by category or AMC.

**Query parameters:**

| Parameter  | Description                                  | Example                              |
|------------|----------------------------------------------|--------------------------------------|
| `category` | Filter by category (case-insensitive)         | `Equity Scheme - Mid Cap Fund`       |
| `amc`      | Filter by AMC name (partial match)            | `HDFC`                               |

**Example:**
```
GET /funds?category=Equity+Scheme+-+Mid+Cap+Fund
```

---

### `GET /funds/{code}`

Fund details and latest NAV.

```
GET /funds/120505
```

---

### `GET /funds/{code}/analytics`

Pre-computed analytics for a specific fund and time window.

**Query parameters:**

| Parameter | Required | Values              |
|-----------|----------|---------------------|
| `window`  | ✅       | `1Y`, `3Y`, `5Y`, `10Y` |

**Example:**
```
GET /funds/120505/analytics?window=3Y
```

**Example response:**
```json
{
  "fund_code": "120505",
  "fund_name": "Axis Midcap Fund - Direct Plan - Growth",
  "category": "Equity Scheme - Mid Cap Fund",
  "amc": "Axis Mutual Fund",
  "window": "3Y",
  "data_availability": {
    "start_date": "2016-01-15",
    "end_date": "2026-01-06",
    "total_days": 3644,
    "nav_data_points": 2513
  },
  "rolling_periods_analyzed": 731,
  "rolling_returns": {
    "min": 8.2,
    "max": 48.5,
    "median": 22.3,
    "p25": 15.7,
    "p75": 28.9
  },
  "max_drawdown": -32.1,
  "cagr": {
    "min": 9.5,
    "max": 45.2,
    "median": 21.8
  },
  "computed_at": "2026-01-06T02:30:15Z"
}
```

---

### `GET /funds/rank`

Rank funds within a category by a performance metric.

**Query parameters:**

| Parameter  | Required | Default          | Values / Notes                              |
|------------|----------|------------------|---------------------------------------------|
| `category` | ✅       | —                | `Equity Scheme - Mid Cap Fund` or `Equity Scheme - Small Cap Fund` |
| `window`   | ✅       | —                | `1Y`, `3Y`, `5Y`, `10Y`                     |
| `sort_by`  | ❌       | `median_return`  | `median_return` or `max_drawdown`           |
| `limit`    | ❌       | `5`              | 1 – 50                                      |

**Example:**
```
GET /funds/rank?category=Equity+Scheme+-+Mid+Cap+Fund&window=3Y&sort_by=median_return&limit=5
```

---

### `POST /sync/trigger`

Manually trigger an incremental data sync for all schemes. Returns immediately with `202 Accepted`; sync runs in the background.

```
POST /sync/trigger
```

---

### `GET /sync/status`

Pipeline status for every tracked scheme.

```
GET /sync/status
```

**Example response:**
```json
{
  "schemes": [
    {
      "scheme_code": "120505",
      "status": "done",
      "last_nav_date": "2026-04-17",
      "latest_nav": 133.75,
      "nav_count": 2513,
      "updated_at": "2026-04-18T14:30:00Z"
    }
  ],
  "summary": {
    "total": 10,
    "done": 10,
    "pending": 0,
    "running": 0,
    "error": 0
  }
}
```

---

## Running Tests

```powershell
# All tests
go test ./...

# With race detector — required for rate limiter concurrent-access tests
go test -race ./...

# With per-package coverage
go test -cover ./internal/analytics/... ./internal/api/... ./internal/ratelimit/...

# Verbose — see every individual test and subtest name
go test -v ./internal/analytics/... ./internal/api/... ./internal/ratelimit/...

# Single package
go test ./internal/ratelimit/...
go test ./internal/analytics/...
go test ./internal/api/...

# Single test by name
go test -run TestPerSecondLimit ./internal/ratelimit/
go test -run TestManualVerification ./internal/analytics/
go test -run TestGetAnalytics ./internal/api/
```

**Observed coverage (no live database required):**

| Package | Coverage | Notes |
|---|---|---|
| `internal/analytics` | 69.2% | Pure functions — all paths exercised without I/O |
| `internal/api` | 84.8% | Mock store — no DB or network |
| `internal/ratelimit` | 44.1% | Token bucket internals partially unexported; exported `Wait()` fully covered |

**Test strategy overview:**

- **Rate limiter** (`limiter_test.go`) — 8 tests: each of the three limits verified individually, composite enforcement (all three simultaneously), concurrent access with race detector, state-persistence offline-refill math, drain-on-429, context cancellation
- **Analytics** (`engine_test.go`) — 10 tests: every computation function verified with manually pre-computed reference values (documented inline); edge cases include empty input, zero-NAV rows, window ≥ series length, insufficient history, and a full 10-year end-to-end verification
- **API handlers** (`handlers_test.go`) — 7 test functions, ~30 subtests: all 6 endpoints covered with happy-path responses, every validation error code (`INVALID_CATEGORY`, `INVALID_WINDOW`, `NOT_FOUND`, `ANALYTICS_NOT_READY`, `MISSING_CATEGORY`, `INVALID_SORT_BY`, `INVALID_LIMIT`), DB error paths (→ 500), and edge cases; every request asserts `<200ms` response time
- **Pipeline resumability** (`pipeline_test.go`) — 2 tests: all four `sync_state` lifecycle states (done/pending/running/error) surfaced correctly after a simulated crash; trigger endpoint returns 202 non-blocking

---

## Project Structure

```
mutualFundAnalysis/
├── cmd/
│   └── server/
│       └── main.go              # Entry point: background sync + HTTP server
├── db/
│   └── migrations/
│       └── 001_initial_schema.sql
├── internal/
│   ├── analytics/
│   │   ├── engine.go            # Rolling returns, drawdown, CAGR computation
│   │   └── engine_test.go       # Manually verified reference values
│   ├── api/
│   │   ├── router.go            # Chi route registration
│   │   ├── funds.go             # GET /funds, GET /funds/{code}
│   │   ├── analyticshandler.go  # GET /funds/{code}/analytics
│   │   ├── rank.go              # GET /funds/rank
│   │   ├── synchandler.go       # POST /sync/trigger, GET /sync/status
│   │   ├── response.go          # JSON response types and converters
│   │   ├── store_iface.go       # DataStore interface (enables mock testing)
│   │   ├── swagger.go           # Swagger UI handler
│   │   ├── handlers_test.go     # All endpoint tests: happy path + validation + error + <200ms SLA
│   │   ├── pipeline_test.go     # Pipeline resumability: crash states, trigger non-blocking
│   │   └── docs/
│   │       └── openapi.yaml     # OpenAPI 3.0 specification
│   ├── client/
│   │   └── mfapi.go             # mfapi.in HTTP client with retry + rate limiting
│   ├── config/
│   │   └── config.go            # Environment config loading
│   ├── ingestion/
│   │   ├── pipeline.go          # SyncScheme + RepairAnalytics core logic
│   │   ├── backfill.go          # Backfill runner (FOR UPDATE SKIP LOCKED queue)
│   │   ├── scheduler.go         # Daily sync scheduler (20:00 IST)
│   │   └── seeds.go             # Hardcoded list of 10 tracked scheme codes
│   ├── models/
│   │   └── models.go            # Domain types (Scheme, NAVRow, Analytics, ...)
│   ├── ratelimit/
│   │   ├── limiter.go           # Composite triple token bucket rate limiter
│   │   └── limiter_test.go      # Rate limiter test suite (all three limits + concurrency)
│   └── store/
│       ├── db.go                # pgx connection pool setup
│       ├── schemes.go           # Scheme CRUD
│       ├── nav.go               # NAV data bulk insert + history query
│       ├── analytics.go         # Analytics upsert + ranking query
│       ├── syncstate.go         # Job queue + checkpoint operations
│       ├── ratelimiter.go       # Token bucket state persistence
│       └── requestlog.go        # Audit log writes
├── .env.example
├── go.mod
├── DESIGN_DECISIONS.md
└── README.md
```

---

## Database Schema (Overview)

Managed by a single migration file. Key design choices:

- `nav_data` uses `NUMERIC(20,5)` (not `FLOAT`) to store NAVs without floating-point rounding errors
- `nav_data` primary key is `(scheme_code, nav_date)` — ensures idempotent inserts via `ON CONFLICT DO NOTHING`
- `sync_state` doubles as a job queue and crash-recovery checkpoint; `FOR UPDATE SKIP LOCKED` enables safe concurrent access
- `rate_limiter_state` persists token bucket counts so rate limits are never violated across restarts
- `request_log` is an append-only audit table proving rate limit compliance

See `DESIGN_DECISIONS.md` for a full discussion of schema decisions and trade-offs.

---

## Environment Variables

| Variable       | Required | Default | Description                                     |
|----------------|----------|---------|-------------------------------------------------|
| `DATABASE_URL` | ✅       | —       | PostgreSQL connection string (pgx format)        |
| `PORT`         | ❌       | `8080`  | HTTP server port                                |
| `LOG_LEVEL`    | ❌       | `info`  | Structured log level (`debug`, `info`, `warn`, `error`) |

---

## Dependencies

| Package | Purpose |
|---|---|
| `go-chi/chi/v5` | HTTP router with middleware and path parameters |
| `jackc/pgx/v5` | PostgreSQL driver with native COPY protocol support |
| `joho/godotenv` | `.env` file loading for local development |

All dependencies are vendored in `go.sum`. No external services (Redis, message broker, etc.) are required.
