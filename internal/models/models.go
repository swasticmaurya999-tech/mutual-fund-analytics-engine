package models

import (
	"fmt"
	"strings"
	"time"
)

// Scheme holds metadata for a tracked mutual fund.
// Populated from the `meta` block of GET /mf/{code} API response.
type Scheme struct {
	Code                string
	Name                string
	AMC                 string
	Category            string
	SchemeType          string
	ISINGrowth          *string
	ISINDivReinvestment *string
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// NAVRow is a single daily NAV data point.
type NAVRow struct {
	SchemeCode string
	NavDate    time.Time
	NAV        float64
}

// SyncState tracks the pipeline state for a single scheme.
// Acts as both a job queue entry and a crash-recovery checkpoint.
type SyncState struct {
	SchemeCode  string
	Status      string     // pending | running | done | error
	LastNavDate *time.Time // checkpoint: latest NAV date successfully committed
	LatestNAV   *float64   // NAV value on LastNavDate (for ranking API)
	NavCount    int        // total nav_data rows stored for this scheme
	ErrorMsg    *string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Analytics holds pre-computed performance metrics for a scheme+window pair.
// All percentage values (returns, drawdown) are stored as percentages, e.g. 22.3 = 22.3%.
type Analytics struct {
	SchemeCode             string
	Window                 string // 1Y | 3Y | 5Y | 10Y
	RollingMin             *float64
	RollingMax             *float64
	RollingMedian          *float64
	RollingP25             *float64
	RollingP75             *float64
	RollingVolatility      *float64
	MaxDrawdown            *float64
	CAGRMin                *float64
	CAGRMax                *float64
	CAGRMedian             *float64
	DataStart              *time.Time
	DataEnd                *time.Time
	TotalDays              *int
	NavDataPoints          *int
	RollingPeriodsAnalyzed *int
	InsufficientData       bool
	ComputedAt             time.Time
}

// RateLimiterState persists a token bucket's state across restarts.
type RateLimiterState struct {
	LimiterID   string
	Tokens      float64
	Capacity    float64
	RefillRate  float64 // tokens per second
	LastUpdated time.Time
}

// RequestLog is an immutable audit record of one outbound API call.
type RequestLog struct {
	SchemeCode   *string
	Endpoint     string
	FullURL      string
	RequestedAt  time.Time
	HTTPStatus   *int
	DurationMS   *int
	ErrorMsg     *string
	RetryAttempt int
}

// RankRow is a denormalized row used by the ranking API.
// Produced by joining analytics + schemes + sync_state.
type RankRow struct {
	SchemeCode             string
	SchemeName             string
	AMC                    string
	Category               string
	Window                 string
	RollingMedian          *float64
	MaxDrawdown            *float64
	LatestNAV              *float64
	LastNavDate            *time.Time
	InsufficientData       bool
}

// Valid window values accepted by analytics and ranking endpoints.
var ValidWindows = map[string]bool{
	"1Y": true, "3Y": true, "5Y": true, "10Y": true,
}

// WindowTradingDays maps each window to its approximate trading day count.
var WindowTradingDays = map[string]int{
	"1Y": 252, "3Y": 756, "5Y": 1260, "10Y": 2520,
}

// Canonical category names exactly as stored in the DB (from mfapi.in).
const (
	CategoryMidCap   = "Equity Scheme - Mid Cap Fund"
	CategorySmallCap = "Equity Scheme - Small Cap Fund"
)

// ValidCategoryNames is the ordered list used in error messages.
var ValidCategoryNames = []string{CategoryMidCap, CategorySmallCap}

// NormalizeCategory validates a category query param against the two allowed
// values. Matching is case-insensitive so "equity scheme - mid cap fund" and
// "Equity Scheme - Mid Cap Fund" both resolve to the canonical form.
//
// Returns the canonical category string on success, or a descriptive error
// if the input does not exactly match one of the two tracked categories.
func NormalizeCategory(input string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(input)) {
	case strings.ToLower(CategoryMidCap):
		return CategoryMidCap, nil
	case strings.ToLower(CategorySmallCap):
		return CategorySmallCap, nil
	default:
		return "", fmt.Errorf(
			"invalid category %q — only two categories are tracked: %q and %q",
			input, CategoryMidCap, CategorySmallCap,
		)
	}
}
