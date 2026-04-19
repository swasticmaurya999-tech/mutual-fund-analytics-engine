// Package analytics_test verifies the correctness of every analytics
// computation function using manually pre-computed reference values.
//
// Test philosophy:
//   - All inputs and expected outputs are computed by hand and documented
//     inline so a reviewer can audit the math without running code.
//   - Table-driven tests keep coverage broad; edge cases (empty, single,
//     all-zero) are always included.
//   - No database is needed: all functions under test are pure (no I/O).
package analytics

import (
	"math"
	"sort"
	"testing"
	"time"

	"mutualfundanalysis/internal/models"
)

// ─── helpers ─────────────────────────────────────────────────────────────────

const epsilon = 1e-6 // tolerance for floating-point comparisons

func almostEqual(a, b float64) bool {
	return math.Abs(a-b) < epsilon
}

func mustFloat(p *float64) float64 {
	if p == nil {
		return math.NaN()
	}
	return *p
}

// navRow is a helper that builds a models.NAVRow from a date string and NAV.
func navRow(dateStr string, nav float64) models.NAVRow {
	t, err := time.Parse("2006-01-02", dateStr)
	if err != nil {
		panic(err)
	}
	return models.NAVRow{NavDate: t, NAV: nav}
}

// ─── filterValidNAVs ─────────────────────────────────────────────────────────

func TestFilterValidNAVs(t *testing.T) {
	tests := []struct {
		name     string
		input    []models.NAVRow
		wantLen  int
		wantNAVs []float64
	}{
		{
			name:    "empty slice returns empty",
			input:   []models.NAVRow{},
			wantLen: 0,
		},
		{
			name: "all valid — nothing filtered",
			input: []models.NAVRow{
				{NAV: 10}, {NAV: 20}, {NAV: 30},
			},
			wantLen:  3,
			wantNAVs: []float64{10, 20, 30},
		},
		{
			name: "zero NAV rows removed",
			input: []models.NAVRow{
				{NAV: 10}, {NAV: 0}, {NAV: 30},
			},
			wantLen:  2,
			wantNAVs: []float64{10, 30},
		},
		{
			name: "negative NAV rows removed",
			input: []models.NAVRow{
				{NAV: -5}, {NAV: 15}, {NAV: 0}, {NAV: 25},
			},
			wantLen:  2,
			wantNAVs: []float64{15, 25},
		},
		{
			name: "all invalid returns empty",
			input: []models.NAVRow{
				{NAV: 0}, {NAV: -1}, {NAV: 0},
			},
			wantLen: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// filterValidNAVs reuses the backing array; copy first to avoid
			// mutating the test-case slice and polluting subsequent sub-tests.
			input := make([]models.NAVRow, len(tc.input))
			copy(input, tc.input)

			got := filterValidNAVs(input)
			if len(got) != tc.wantLen {
				t.Fatalf("len = %d; want %d", len(got), tc.wantLen)
			}
			for i, nav := range tc.wantNAVs {
				if !almostEqual(got[i].NAV, nav) {
					t.Errorf("got[%d].NAV = %v; want %v", i, got[i].NAV, nav)
				}
			}
		})
	}
}

// ─── computeMaxDrawdown ──────────────────────────────────────────────────────
//
// Manual verification of expected values:
//
//	navs = [100, 110, 90, 120, 80]
//	peak at each step:  100, 110, 110, 120, 120
//	drawdown at step 2: (90  - 110) / 110 * 100 = -18.1818…%
//	drawdown at step 4: (80  - 120) / 120 * 100 = -33.3333…%
//	max drawdown = -33.3333…%

func TestComputeMaxDrawdown(t *testing.T) {
	tests := []struct {
		name     string
		navs     []models.NAVRow
		wantDD   float64
		wantZero bool // expect exactly 0
	}{
		{
			name:     "empty slice returns 0",
			navs:     []models.NAVRow{},
			wantZero: true,
		},
		{
			name: "monotonically increasing — no drawdown",
			navs: []models.NAVRow{
				{NAV: 100}, {NAV: 110}, {NAV: 120},
			},
			wantZero: true,
		},
		{
			name: "single peak-to-trough",
			// 100 → 50: (50-100)/100 = -50%
			navs: []models.NAVRow{
				{NAV: 100}, {NAV: 50},
			},
			wantDD: -50.0,
		},
		{
			name: "standard five-point series",
			// peak sequence: 100, 110, 110, 120, 120
			// worst: (80-120)/120*100 = -33.3333…
			navs: []models.NAVRow{
				{NAV: 100}, {NAV: 110}, {NAV: 90}, {NAV: 120}, {NAV: 80},
			},
			wantDD: (-40.0 / 120.0) * 100, // -33.3333…
		},
		{
			name: "strictly decreasing — full decline from first peak",
			// peak always = first element (100)
			// worst: (10-100)/100*100 = -90%
			navs: []models.NAVRow{
				{NAV: 100}, {NAV: 80}, {NAV: 60}, {NAV: 10},
			},
			wantDD: -90.0,
		},
		{
			name: "multiple drawdowns — deepest wins",
			// First valley: (40-100)/100 = -60%
			// Second valley: (10-100)/100 = -90%  ← deepest
			navs: []models.NAVRow{
				{NAV: 100}, {NAV: 40}, {NAV: 100}, {NAV: 10},
			},
			wantDD: -90.0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := computeMaxDrawdown(tc.navs)
			if tc.wantZero {
				if got != 0 {
					t.Errorf("expected 0, got %v", got)
				}
				return
			}
			if !almostEqual(got, tc.wantDD) {
				t.Errorf("got %v; want %v (diff %v)", got, tc.wantDD, math.Abs(got-tc.wantDD))
			}
			// Drawdown must always be ≤ 0
			if got > 0 {
				t.Errorf("drawdown must be ≤ 0, got %v", got)
			}
		})
	}
}

// ─── percentile ──────────────────────────────────────────────────────────────
//
// Reference: linear interpolation, same as Excel PERCENTILE.INC.
//
//	sorted = [1, 2, 3, 4, 5]  (n=5, indices 0-4)
//	P(0)  : index = 0.00*(5-1) = 0.0 → sorted[0] = 1
//	P(25) : index = 0.25*(5-1) = 1.0 → sorted[1] = 2
//	P(50) : index = 0.50*(5-1) = 2.0 → sorted[2] = 3
//	P(75) : index = 0.75*(5-1) = 3.0 → sorted[3] = 4
//	P(100): index = 1.00*(5-1) = 4.0 → sorted[4] = 5
//
//	For interpolation: sorted = [10, 20, 30, 40] (n=4)
//	P(50) : index = 0.5*3 = 1.5 → 0.5*sorted[1] + 0.5*sorted[2] = 0.5*20+0.5*30 = 25

func TestPercentile(t *testing.T) {
	tests := []struct {
		name   string
		sorted []float64
		p      float64
		want   float64
	}{
		{"empty returns 0", []float64{}, 50, 0},
		{"single element always returns it", []float64{42}, 50, 42},
		{"p0 is minimum", []float64{1, 2, 3, 4, 5}, 0, 1},
		{"p100 is maximum", []float64{1, 2, 3, 4, 5}, 100, 5},
		{"p25 of [1,2,3,4,5]", []float64{1, 2, 3, 4, 5}, 25, 2},
		{"p50 of [1,2,3,4,5]", []float64{1, 2, 3, 4, 5}, 50, 3},
		{"p75 of [1,2,3,4,5]", []float64{1, 2, 3, 4, 5}, 75, 4},
		// Interpolated case: index = 1.5 → 20*0.5 + 30*0.5 = 25
		{"p50 of [10,20,30,40] interpolates", []float64{10, 20, 30, 40}, 50, 25},
		// p75 of [10,20,30,40]: index = 0.75*3 = 2.25 → 30*(1-0.25)+40*0.25 = 32.5
		{"p75 of [10,20,30,40] interpolates", []float64{10, 20, 30, 40}, 75, 32.5},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := percentile(tc.sorted, tc.p)
			if !almostEqual(got, tc.want) {
				t.Errorf("percentile(%v, %v) = %v; want %v", tc.sorted, tc.p, got, tc.want)
			}
		})
	}
}

// ─── stdDev ──────────────────────────────────────────────────────────────────
//
// Reference dataset (Wikipedia Welford example):
//
//	values = [2, 4, 4, 4, 5, 5, 7, 9]   n=8
//	mean   = 40/8 = 5
//	Σ(x-mean)² = 9+1+1+1+0+0+4+16 = 32
//	sample std dev = sqrt(32/7) ≈ 2.13809

func TestStdDev(t *testing.T) {
	tests := []struct {
		name   string
		values []float64
		want   float64
	}{
		{"empty returns 0", []float64{}, 0},
		{"single element returns 0", []float64{5}, 0},
		{"two identical elements returns 0", []float64{3, 3}, 0},
		{
			// Wikipedia Welford dataset — manually verified
			"wikipedia example",
			[]float64{2, 4, 4, 4, 5, 5, 7, 9},
			math.Sqrt(32.0 / 7.0), // ≈ 2.1380899352993946
		},
		{
			// Trivial two-element case: values=[1,3], mean=2, Σ=(1-2)²+(3-2)²=2, s=sqrt(2/1)=√2
			"two elements",
			[]float64{1, 3},
			math.Sqrt2, // ≈ 1.4142135623730951
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := stdDev(tc.values)
			if !almostEqual(got, tc.want) {
				t.Errorf("stdDev(%v) = %v; want %v (diff %.10f)", tc.values, got, tc.want, math.Abs(got-tc.want))
			}
		})
	}
}

// ─── computeRollingStats ─────────────────────────────────────────────────────
//
// Manually constructed test case:
//
//	NAV series (daily, 5 points):
//	  Day 0 (2020-01-01): 100.00
//	  Day 1 (2020-01-02): 110.00  (+10%)
//	  Day 2 (2020-01-03): 121.00  (+10%)
//	  Day 3 (2020-01-04): 133.10  (+10%)
//	  Day 4 (2020-01-05): 146.41  (+10%)
//
//	windowDays = 2:
//	  Period [0→2]: start=100, end=121
//	    total return = (121/100 - 1)*100 = 21%
//	    calDays = 2
//	    CAGR = (121/100)^(365.25/2) - 1) * 100
//	         = 1.21^182.625 - 1) * 100
//	  Period [1→3]: start=110, end=133.10  → same ratio 1.21  → same return/CAGR
//	  Period [2→4]: start=121, end=146.41  → same ratio 1.21  → same return/CAGR
//
//	3 periods expected; all returns = 21%; all CAGRs identical (same ratio, same days).

func TestComputeRollingStats(t *testing.T) {
	navs := []models.NAVRow{
		navRow("2020-01-01", 100.00),
		navRow("2020-01-02", 110.00),
		navRow("2020-01-03", 121.00),
		navRow("2020-01-04", 133.10),
		navRow("2020-01-05", 146.41),
	}

	t.Run("windowDays=2 yields 3 periods with 21% return each", func(t *testing.T) {
		returns, cagrs := computeRollingStats(navs, 2)

		if len(returns) != 3 {
			t.Fatalf("expected 3 periods, got %d", len(returns))
		}
		if len(cagrs) != 3 {
			t.Fatalf("expected 3 cagrs, got %d", len(cagrs))
		}

		// All total returns should be 21%
		for i, r := range returns {
			if !almostEqual(r, 21.0) {
				t.Errorf("returns[%d] = %v; want 21.0", i, r)
			}
		}

		// All CAGRs should be equal (same NAV ratio, same calendar distance = 1 day)
		for i := 1; i < len(cagrs); i++ {
			if !almostEqual(cagrs[i], cagrs[0]) {
				t.Errorf("cagrs[%d] = %v; want %v (equal to cagrs[0])", i, cagrs[i], cagrs[0])
			}
		}
	})

	t.Run("windowDays larger than navs returns empty", func(t *testing.T) {
		ret, cag := computeRollingStats(navs, 10)
		if len(ret) != 0 || len(cag) != 0 {
			t.Errorf("expected empty slices for oversized window, got %d returns, %d cagrs", len(ret), len(cag))
		}
	})

	t.Run("windowDays equals navs length returns empty (need strictly more)", func(t *testing.T) {
		ret, cag := computeRollingStats(navs, len(navs))
		if len(ret) != 0 || len(cag) != 0 {
			t.Errorf("expected empty for window==len(navs), got %d, %d", len(ret), len(cag))
		}
	})

	t.Run("CAGR annualisation: 1-year doubling should give ~100pct CAGR", func(t *testing.T) {
		// Start: 100, End: 200 exactly 365 calendar days later
		// CAGR = (200/100)^(365.25/365) - 1 ≈ 100.05%  (slight leap-year factor)
		startDate := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
		endDate := startDate.AddDate(1, 0, 0) // exactly 365 or 366 days
		calDays := endDate.Sub(startDate).Hours() / 24
		twoRows := []models.NAVRow{
			{NavDate: startDate, NAV: 100},
			{NavDate: endDate, NAV: 200},
		}
		_, cagrs := computeRollingStats(twoRows, 1)
		if len(cagrs) != 1 {
			t.Fatalf("expected 1 cagr, got %d", len(cagrs))
		}
		expected := (math.Pow(2.0, 365.25/calDays) - 1) * 100
		if !almostEqual(cagrs[0], expected) {
			t.Errorf("CAGR = %v; want %v", cagrs[0], expected)
		}
	})

	t.Run("zero or negative NAV rows are skipped defensively", func(t *testing.T) {
		mixed := []models.NAVRow{
			navRow("2020-01-01", 100),
			navRow("2020-01-02", 0),   // invalid — should be skipped
			navRow("2020-01-03", 120),
		}
		// windowDays=2: only period [0→2] but start=100, end=0 at index 1 is skipped;
		// period [0→2] has start=navs[0]=100, end=navs[2]=120 but index 1 has NAV=0
		// so the guard in computeRollingStats skips start.NAV=0 rows.
		// Because mixed[1].NAV == 0, the period [1→3] (i=2, start=mixed[0], end=mixed[2])
		// checks start.NAV=mixed[0].NAV=100 (ok) and end.NAV=mixed[2].NAV=120 (ok) → valid.
		// Period [0→2] with window=2: start=mixed[0]=100, end=mixed[2]=120 → valid.
		// Only one complete period: i=2, start=mixed[0]=100, end=mixed[2]=120
		// Wait - i iterates from windowDays(2) to len(mixed)(3), so only i=2:
		//   start = mixed[0] NAV=100, end = mixed[2] NAV=120 → valid period
		ret, _ := computeRollingStats(mixed, 2)
		if len(ret) != 1 {
			t.Errorf("expected 1 period (zero NAV at index 1 is bridged), got %d", len(ret))
		}
		// Period [0→2]: (120/100-1)*100 = 20%
		if len(ret) > 0 && !almostEqual(ret[0], 20.0) {
			t.Errorf("return = %v; want 20.0", ret[0])
		}
	})
}

// ─── computeWindow ───────────────────────────────────────────────────────────

func TestComputeWindow_InsufficientData(t *testing.T) {
	// 3 NAV rows — not enough for any window (min is 1Y = 252 trading days).
	navs := []models.NAVRow{
		navRow("2024-01-01", 100),
		navRow("2024-01-02", 110),
		navRow("2024-01-03", 120),
	}
	maxDD := computeMaxDrawdown(navs) // -0 (monotonically increasing)

	for _, window := range []string{"1Y", "3Y", "5Y", "10Y"} {
		t.Run("insufficient data for "+window, func(t *testing.T) {
			a := computeWindow("TEST001", window, navs, maxDD)
			if !a.InsufficientData {
				t.Errorf("expected InsufficientData=true for window %s with only 3 rows", window)
			}
			if a.RollingMin != nil {
				t.Errorf("expected nil RollingMin for insufficient data, got %v", *a.RollingMin)
			}
		})
	}
}

func TestComputeWindow_SufficientData(t *testing.T) {
	// Build a synthetic 300-row NAV series (more than the 252 needed for 1Y window).
	// Each row has NAV that grows ~0.1% daily (compound).
	base := time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC)
	navs := make([]models.NAVRow, 300)
	nav := 100.0
	for i := range navs {
		navs[i] = models.NAVRow{
			NavDate: base.AddDate(0, 0, i),
			NAV:     nav,
		}
		nav *= 1.001 // 0.1% daily growth
	}

	maxDD := computeMaxDrawdown(navs)

	t.Run("1Y window produces valid analytics", func(t *testing.T) {
		a := computeWindow("TEST001", "1Y", navs, maxDD)

		if a.InsufficientData {
			t.Fatal("expected sufficient data for 1Y with 300 rows")
		}
		if a.RollingMin == nil || a.RollingMax == nil || a.RollingMedian == nil {
			t.Fatal("expected non-nil rolling stats")
		}
		// For monotonically increasing NAVs: min return > 0, max return > 0
		if *a.RollingMin <= 0 {
			t.Errorf("RollingMin should be positive for growing NAVs, got %v", *a.RollingMin)
		}
		// MedianReturn should be between min and max
		if *a.RollingMedian < *a.RollingMin || *a.RollingMedian > *a.RollingMax {
			t.Errorf("median %v not in [%v, %v]", *a.RollingMedian, *a.RollingMin, *a.RollingMax)
		}
		// Max drawdown should be 0 (all NAVs increasing)
		if *a.MaxDrawdown != 0 {
			t.Errorf("expected max_drawdown=0 for monotonically increasing NAVs, got %v", *a.MaxDrawdown)
		}
		// RollingPeriodsAnalyzed should equal len(navs) - windowDays = 300 - 252 = 48
		if *a.RollingPeriodsAnalyzed != 48 {
			t.Errorf("expected 48 periods, got %d", *a.RollingPeriodsAnalyzed)
		}
	})
}

// ─── end-to-end: round-trip manual verification ──────────────────────────────
//
// This test simulates a complete analytics computation using a known NAV series
// and cross-checks each metric against manually derived values.
//
// NAV series (10 points spaced 1 year apart):
//
//	Yr 0 (2014-01-01): 100
//	Yr 1 (2015-01-01): 120  (20% gain)
//	Yr 2 (2016-01-01): 110  (drawdown from 120 to 110 = -8.33%)
//	Yr 3 (2017-01-01): 130  (new high)
//	Yr 4 (2018-01-01): 80   (drawdown from 130 to 80 = -38.46%)  ← deepest
//	Yr 5 (2019-01-01): 150  (new high)
//	Yr 6 (2020-01-01): 140  (small drawdown from 150 to 140 = -6.67%)
//	Yr 7 (2021-01-01): 180  (new high)
//	Yr 8 (2022-01-01): 200  (new high)
//	Yr 9 (2023-01-01): 220  (new high)
//
//	Max drawdown: (80-130)/130*100 = -38.46%
//
//	With windowDays = 4 (approximating a very short "window"):
//	Period 0 [Yr0→Yr4]: 100→80,  return = -20%
//	Period 1 [Yr1→Yr5]: 120→150, return = +25%
//	Period 2 [Yr2→Yr6]: 110→140, return = +27.27%
//	Period 3 [Yr3→Yr7]: 130→180, return = +38.46%
//	Period 4 [Yr4→Yr8]: 80→200,  return = +150%
//	Period 5 [Yr5→Yr9]: 150→220, return = +46.67%
//
//	Sorted returns: [-20, 25, 27.27, 38.46, 46.67, 150]
//	Min = -20%, Max = 150%, Median (P50) = (27.27+38.46)/2 ≈ 32.865%

func TestComputeRollingStats_ManualVerification(t *testing.T) {
	navs := []models.NAVRow{
		navRow("2014-01-01", 100),
		navRow("2015-01-01", 120),
		navRow("2016-01-01", 110),
		navRow("2017-01-01", 130),
		navRow("2018-01-01", 80),
		navRow("2019-01-01", 150),
		navRow("2020-01-01", 140),
		navRow("2021-01-01", 180),
		navRow("2022-01-01", 200),
		navRow("2023-01-01", 220),
	}

	t.Run("max drawdown is -38.46pct", func(t *testing.T) {
		got := computeMaxDrawdown(navs)
		want := (80.0 - 130.0) / 130.0 * 100 // -38.461538…
		if !almostEqual(got, want) {
			t.Errorf("maxDD = %v; want %v", got, want)
		}
	})

	t.Run("rolling returns with window=4 — 6 periods", func(t *testing.T) {
		returns, _ := computeRollingStats(navs, 4)
		if len(returns) != 6 {
			t.Fatalf("expected 6 periods, got %d", len(returns))
		}

		// Manually computed returns (unordered output matches the slide order):
		wantReturns := []float64{
			(80.0/100.0 - 1) * 100,   // period 0: -20%
			(150.0/120.0 - 1) * 100,  // period 1: 25%
			(140.0/110.0 - 1) * 100,  // period 2: ~27.27%
			(180.0/130.0 - 1) * 100,  // period 3: ~38.46%
			(200.0/80.0 - 1) * 100,   // period 4: 150%
			(220.0/150.0 - 1) * 100,  // period 5: ~46.67%
		}
		for i, want := range wantReturns {
			if !almostEqual(returns[i], want) {
				t.Errorf("returns[%d] = %v; want %v", i, returns[i], want)
			}
		}
	})

	t.Run("sorted rolling returns — min max median", func(t *testing.T) {
		returns, _ := computeRollingStats(navs, 4)
		sort.Float64s(returns)

		wantMin := (80.0/100.0 - 1) * 100 // -20%
		wantMax := (200.0/80.0 - 1) * 100 // 150%

		if !almostEqual(returns[0], wantMin) {
			t.Errorf("min = %v; want %v", returns[0], wantMin)
		}
		if !almostEqual(returns[len(returns)-1], wantMax) {
			t.Errorf("max = %v; want %v", returns[len(returns)-1], wantMax)
		}

		// Median of 6 elements: average of indices 2 and 3 after sorting
		// sorted: [-20, 25, ~27.27, ~38.46, ~46.67, 150]
		median := percentile(returns, 50)
		wantMedian := ((140.0/110.0-1)*100 + (180.0/130.0-1)*100) / 2.0
		if !almostEqual(median, wantMedian) {
			t.Errorf("median = %v; want ~%v", median, wantMedian)
		}
	})
}
