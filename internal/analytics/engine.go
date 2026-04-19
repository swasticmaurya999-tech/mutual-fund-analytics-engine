// Package analytics pre-computes performance metrics from NAV time-series data.
// All metrics are computed over the full available NAV history; the window
// parameter determines the duration of each rolling period.
package analytics

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"time"

	"mutualfundanalysis/internal/models"
	"mutualfundanalysis/internal/store"
)

// Engine computes and persists analytics for all windows.
type Engine struct {
	store *store.Store
	log   *slog.Logger
}

// New creates an Engine.
func New(st *store.Store, log *slog.Logger) *Engine {
	return &Engine{store: st, log: log}
}

// ComputeAll computes analytics for all 4 windows for a scheme and persists
// results. Retries up to 3 times with exponential backoff so transient DB
// errors (connection blip, brief unavailability) do not permanently leave a
// scheme without analytics.
//
// Non-fatal per-window: a failure on one window is logged but does not
// prevent the other windows from being computed.
func (e *Engine) ComputeAll(ctx context.Context, schemeCode string) error {
	const maxAttempts = 3
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			// Exponential backoff: 2 s, 4 s
			wait := time.Duration(1<<uint(attempt-2)) * 2 * time.Second
			e.log.Warn("retrying analytics after failure",
				"scheme_code", schemeCode, "attempt", attempt, "wait", wait)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		if err := e.computeAllOnce(ctx, schemeCode); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return fmt.Errorf("analytics failed after %d attempts: %w", maxAttempts, lastErr)
}

// computeAllOnce is a single attempt to compute and persist analytics for all
// 4 windows. Called by ComputeAll; not safe to call directly.
//
// Time complexity  (n = valid NAV rows):
//
//	O(n)              filterValidNAVs
//	O(n)              computeMaxDrawdown — run ONCE and shared across all windows
//	O(W × n)          computeRollingStats, W = 4 windows
//	O(W × P log P)    sort rolling returns + CAGRs, P ≈ n per window
//	O(W × P)          stdDev (Welford single-pass)
//
//	Dominant term: O(n log n) per window → O(W × n log n) total
//
// Space complexity: O(n) for the two rolling-period slices (pre-allocated).
func (e *Engine) computeAllOnce(ctx context.Context, schemeCode string) error {
	navs, err := e.store.GetNAVHistory(ctx, schemeCode)
	if err != nil {
		return fmt.Errorf("load nav history for %s: %w", schemeCode, err)
	}

	if len(navs) < 2 {
		e.log.Warn("insufficient nav data for analytics", "scheme_code", schemeCode, "rows", len(navs))
		return nil
	}

	// O(n) — reuses the backing array, no extra allocation.
	navs = filterValidNAVs(navs)

	e.log.Info("computing analytics", "scheme_code", schemeCode, "nav_rows", len(navs))

	// Max drawdown is window-independent: always computed over the full NAV
	// history. Computing it once here (O(n)) and sharing it across all 4
	// windows avoids repeating the same O(n) pass 4 times.
	maxDD := computeMaxDrawdown(navs)

	var firstErr error
	for _, window := range []string{"1Y", "3Y", "5Y", "10Y"} {
		a := computeWindow(schemeCode, window, navs, maxDD)
		if err := e.store.UpsertAnalytics(ctx, a); err != nil {
			e.log.Error("failed to persist analytics",
				"scheme_code", schemeCode, "window", window, "error", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		e.log.Info("analytics saved", "scheme_code", schemeCode, "window", window,
			"periods", ptrInt(a.RollingPeriodsAnalyzed),
			"insufficient", a.InsufficientData,
		)
	}
	return firstErr
}

// computeWindow builds the Analytics struct for one scheme+window pair.
// maxDrawdown is passed in from the caller so it is only computed once across
// all windows (it is identical for every window since it covers full history).
func computeWindow(schemeCode, window string, navs []models.NAVRow, maxDrawdown float64) *models.Analytics {
	windowDays := models.WindowTradingDays[window]

	n := len(navs)
	dataStart := navs[0].NavDate
	dataEnd := navs[n-1].NavDate
	totalDays := int(dataEnd.Sub(dataStart).Hours() / 24)

	// Local copy so each Analytics struct owns its pointer independently.
	dd := maxDrawdown
	a := &models.Analytics{
		SchemeCode:    schemeCode,
		Window:        window,
		DataStart:     &dataStart,
		DataEnd:       &dataEnd,
		TotalDays:     &totalDays,
		NavDataPoints: &n,
		MaxDrawdown:   &dd,
		ComputedAt:    time.Now(),
	}

	if n < windowDays {
		// Not enough data for even one complete rolling period.
		a.InsufficientData = true
		zero := 0
		a.RollingPeriodsAnalyzed = &zero
		return a
	}

	// O(n) sliding window — slices are pre-allocated to avoid reallocation.
	rollingReturns, cagrs := computeRollingStats(navs, windowDays)

	if len(rollingReturns) == 0 {
		a.InsufficientData = true
		zero := 0
		a.RollingPeriodsAnalyzed = &zero
		return a
	}

	// O(P log P) — sort once each; min/max/percentiles are then O(1).
	sort.Float64s(rollingReturns)
	sort.Float64s(cagrs)

	periods := len(rollingReturns)
	a.RollingPeriodsAnalyzed = &periods

	// Min and max are free after sorting (first and last elements).
	rMin := rollingReturns[0]
	rMax := rollingReturns[len(rollingReturns)-1]
	rMed := percentile(rollingReturns, 50)
	rP25 := percentile(rollingReturns, 25)
	rP75 := percentile(rollingReturns, 75)
	// O(P) Welford single-pass — computed on the sorted slice.
	rVol := stdDev(rollingReturns)
	a.RollingMin = &rMin
	a.RollingMax = &rMax
	a.RollingMedian = &rMed
	a.RollingP25 = &rP25
	a.RollingP75 = &rP75
	a.RollingVolatility = &rVol

	// cagrs is already sorted above — no second sort needed.
	if len(cagrs) > 0 {
		cMin := cagrs[0]
		cMax := cagrs[len(cagrs)-1]
		cMed := percentile(cagrs, 50)
		a.CAGRMin = &cMin
		a.CAGRMax = &cMax
		a.CAGRMedian = &cMed
	}

	return a
}

// computeRollingStats slides a window of windowDays across the NAV slice and
// collects total returns and annualised CAGRs for every complete period.
//
// Time complexity: O(n) — single pass, one division and one math.Pow per step.
// Space complexity: O(P) where P = n − windowDays, pre-allocated upfront.
func computeRollingStats(navs []models.NAVRow, windowDays int) (returns, cagrs []float64) {
	capacity := len(navs) - windowDays
	if capacity <= 0 {
		return
	}
	// Pre-allocate to the exact number of expected periods.
	// Avoids the repeated doubling reallocations that bare append() would cause
	// (~11 reallocations for 2 000 periods if starting from nil).
	returns = make([]float64, 0, capacity)
	cagrs = make([]float64, 0, capacity)

	for i := windowDays; i < len(navs); i++ {
		start := navs[i-windowDays]
		end := navs[i]

		// filterValidNAVs already removed zero/negative rows, but guard
		// defensively against any gap in the input data.
		if start.NAV <= 0 || end.NAV <= 0 {
			continue
		}

		ratio := end.NAV / start.NAV

		// Total return (%) — simple, non-annualised.
		returns = append(returns, (ratio-1)*100)

		// CAGR (%) — annualised using actual calendar days so leap years
		// are handled correctly. math.Pow(ratio, 1/years) = e^(ln(ratio)/years).
		calDays := end.NavDate.Sub(start.NavDate).Hours() / 24
		if calDays > 0 {
			cagr := (math.Pow(ratio, 365.25/calDays) - 1) * 100
			cagrs = append(cagrs, cagr)
		}
	}
	return
}

// computeMaxDrawdown returns the worst peak-to-trough percentage decline
// over the full NAV slice. Result is always <= 0.
//
// Time complexity: O(n) — single pass tracking a running peak.
// Space complexity: O(1).
func computeMaxDrawdown(navs []models.NAVRow) float64 {
	if len(navs) == 0 {
		return 0
	}
	peak := navs[0].NAV
	maxDD := 0.0
	for _, n := range navs {
		if n.NAV > peak {
			peak = n.NAV
		}
		if peak > 0 {
			dd := (n.NAV - peak) / peak * 100
			if dd < maxDD {
				maxDD = dd
			}
		}
	}
	return maxDD
}

// percentile returns the p-th percentile of a sorted slice using linear
// interpolation between adjacent ranks (the same method Excel uses).
//
// Time complexity: O(1) — requires a pre-sorted input.
// Space complexity: O(1).
func percentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return sorted[0]
	}
	index := p / 100.0 * float64(n-1)
	lower := int(math.Floor(index))
	upper := int(math.Ceil(index))
	if lower == upper {
		return sorted[lower]
	}
	frac := index - float64(lower)
	return sorted[lower]*(1-frac) + sorted[upper]*frac
}

// stdDev returns the sample standard deviation using Welford's online
// algorithm (single pass, numerically stable).
//
// Welford accumulates the mean and the sum of squared deviations from the
// running mean in one loop, avoiding the catastrophic cancellation that the
// naive two-pass (Σx then Σ(x-mean)²) can suffer when values are tightly
// clustered around a large mean.
//
// Time complexity: O(n) — one pass instead of the previous two.
// Space complexity: O(1).
func stdDev(values []float64) float64 {
	n := len(values)
	if n < 2 {
		return 0
	}
	var mean, m2 float64
	for i, v := range values {
		// Welford's update step:
		//   delta  = new value − old mean
		//   mean  += delta / count            (update mean)
		//   delta2 = new value − new mean
		//   m2    += delta * delta2            (accumulate squared deviation)
		delta := v - mean
		mean += delta / float64(i+1)
		m2 += delta * (v - mean)
	}
	return math.Sqrt(m2 / float64(n-1))
}

// filterValidNAVs removes rows with zero or negative NAV values.
// Uses navs[:0] to reuse the backing array — no heap allocation.
//
// Time complexity: O(n).
// Space complexity: O(1) extra.
func filterValidNAVs(navs []models.NAVRow) []models.NAVRow {
	out := navs[:0]
	for _, n := range navs {
		if n.NAV > 0 {
			out = append(out, n)
		}
	}
	return out
}

func ptrInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
