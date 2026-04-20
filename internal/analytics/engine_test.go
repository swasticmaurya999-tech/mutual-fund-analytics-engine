// Package analytics tests computation functions — pure math, no database needed.
// All expected values are manually derived and documented inline.
//
// Covers (assignment requirement — analytics correctness):
//   - filterValidNAVs: zero/negative removal, empty input
//   - computeMaxDrawdown: no drawdown, single trough, multiple peaks, empty
//   - percentile: boundary values, linear interpolation
//   - stdDev: Welford algorithm vs Wikipedia reference dataset
//   - computeRollingStats: period count, return values, edge cases
//     (window > series, window = series length, zero-NAV defensive skip)
//   - CAGR annualisation: leap-year-aware calendar-day formula
//   - computeWindow: insufficient data flag + drawdown still computed;
//     sufficient data: valid rolling stats, correct period count
//   - End-to-end manual verification: 10-year series, all metrics hand-checked
package analytics

import (
	"math"
	"sort"
	"testing"
	"time"

	"mutualfundanalysis/internal/models"
)

const eps = 1e-6

func near(a, b float64) bool { return math.Abs(a-b) < eps }

func navRow(date string, nav float64) models.NAVRow {
	t, _ := time.Parse("2006-01-02", date)
	return models.NAVRow{NavDate: t, NAV: nav}
}

func makeRows(navs []float64) []models.NAVRow {
	rows := make([]models.NAVRow, len(navs))
	for i, v := range navs {
		rows[i] = models.NAVRow{NAV: v}
	}
	return rows
}

// ─── filterValidNAVs ─────────────────────────────────────────────────────────

func TestFilterValidNAVs(t *testing.T) {
	cases := []struct {
		name    string
		input   []float64
		wantLen int
	}{
		{"empty input", []float64{}, 0},
		{"all valid", []float64{10, 20, 30}, 3},
		{"zeros removed", []float64{0, 10, 0, 20}, 2},
		{"negatives removed", []float64{-5, 10, -1, 25}, 2},
		{"all invalid returns empty", []float64{0, -1, 0}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := filterValidNAVs(makeRows(tc.input))
			if len(got) != tc.wantLen {
				t.Errorf("len = %d; want %d", len(got), tc.wantLen)
			}
		})
	}
}

// ─── computeMaxDrawdown ──────────────────────────────────────────────────────
//
// Manual derivations:
//
//	single trough:          (50-100)/100*100 = -50%
//	multiple peaks [100,110,90,120,80]: worst = (80-120)/120*100 = -33.33%
//	strictly decreasing:    (10-100)/100*100 = -90%

func TestComputeMaxDrawdown(t *testing.T) {
	cases := []struct {
		name string
		navs []float64
		want float64
	}{
		{"empty returns 0", []float64{}, 0},
		{"monotonic increase — no drawdown", []float64{100, 110, 120}, 0},
		{"single trough -50pct", []float64{100, 50}, -50.0},
		{"multiple peaks — worst wins", []float64{100, 110, 90, 120, 80}, (-40.0 / 120.0) * 100},
		{"strictly decreasing -90pct", []float64{100, 80, 60, 10}, -90.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeMaxDrawdown(makeRows(tc.navs))
			if !near(got, tc.want) {
				t.Errorf("maxDD = %v; want %v", got, tc.want)
			}
			if got > 0 {
				t.Errorf("drawdown must be ≤ 0, got %v", got)
			}
		})
	}
}

// ─── percentile ──────────────────────────────────────────────────────────────
//
// Reference [1,2,3,4,5]:   P25→index=1.0→2, P50→index=2.0→3
// Reference [10,20,30,40]: P50→index=1.5→0.5*20+0.5*30=25

func TestPercentile(t *testing.T) {
	s5 := []float64{1, 2, 3, 4, 5}
	s4 := []float64{10, 20, 30, 40}

	cases := []struct {
		sorted []float64
		p, want float64
	}{
		{[]float64{}, 50, 0},       // edge: empty
		{[]float64{42}, 50, 42},    // edge: single element
		{s5, 0, 1}, {s5, 100, 5},   // boundaries
		{s5, 25, 2}, {s5, 50, 3}, {s5, 75, 4},
		{s4, 50, 25},   // interpolated: 0.5*20+0.5*30
		{s4, 75, 32.5}, // interpolated: index=2.25 → 30*0.75+40*0.25
	}
	for _, tc := range cases {
		if got := percentile(tc.sorted, tc.p); !near(got, tc.want) {
			t.Errorf("percentile(p=%v) = %v; want %v", tc.p, got, tc.want)
		}
	}
}

// ─── stdDev ──────────────────────────────────────────────────────────────────
//
// Wikipedia Welford: [2,4,4,4,5,5,7,9], mean=5, Σ(x-mean)²=32 → √(32/7)≈2.138

func TestStdDev(t *testing.T) {
	cases := []struct {
		in   []float64
		want float64
	}{
		{[]float64{}, 0},
		{[]float64{5}, 0},
		{[]float64{3, 3}, 0},
		{[]float64{2, 4, 4, 4, 5, 5, 7, 9}, math.Sqrt(32.0 / 7.0)},
		{[]float64{1, 3}, math.Sqrt2}, // mean=2, s=√2
	}
	for _, tc := range cases {
		if got := stdDev(tc.in); !near(got, tc.want) {
			t.Errorf("stdDev(%v) = %v; want %v", tc.in, got, tc.want)
		}
	}
}

// ─── computeRollingStats ─────────────────────────────────────────────────────
//
// NAV series [100,110,121,133.10,146.41] — constant 10% step growth.
// windowDays=2 → 3 periods, each ratio=1.21 → return=21%.
//
// Edge cases:
//   window > len(navs)    → 0 periods (no complete rolling window exists)
//   window = len(navs)    → 0 periods (need strictly more data than window)
//   zero NAV in series    → that start/end pair is skipped defensively

func TestRollingStats(t *testing.T) {
	navs := []models.NAVRow{
		navRow("2020-01-01", 100.00),
		navRow("2020-01-02", 110.00),
		navRow("2020-01-03", 121.00),
		navRow("2020-01-04", 133.10),
		navRow("2020-01-05", 146.41),
	}

	t.Run("3 periods each with 21pct return", func(t *testing.T) {
		returns, cagrs := computeRollingStats(navs, 2)
		if len(returns) != 3 || len(cagrs) != 3 {
			t.Fatalf("expected 3 periods/CAGRs, got %d/%d", len(returns), len(cagrs))
		}
		for i, r := range returns {
			if !near(r, 21.0) {
				t.Errorf("returns[%d] = %v; want 21.0", i, r)
			}
		}
	})

	t.Run("window larger than series returns empty", func(t *testing.T) {
		r, c := computeRollingStats(navs, 10)
		if len(r) != 0 || len(c) != 0 {
			t.Errorf("oversized window: got %d returns %d cagrs; want 0", len(r), len(c))
		}
	})

	t.Run("window equal to series length returns empty — need strictly more", func(t *testing.T) {
		r, c := computeRollingStats(navs, len(navs))
		if len(r) != 0 || len(c) != 0 {
			t.Errorf("window==len: got %d returns %d cagrs; want 0", len(r), len(c))
		}
	})

	t.Run("zero NAV in middle — that pair skipped defensively", func(t *testing.T) {
		// Series: [100, 0, 120]; windowDays=2
		// i=2: start=navs[0]=100 (ok), end=navs[2]=120 (ok) → valid
		// No pair uses navs[1].NAV=0 as both start AND end → 1 valid period
		mixed := []models.NAVRow{
			navRow("2020-01-01", 100),
			navRow("2020-01-02", 0), // invalid NAV
			navRow("2020-01-03", 120),
		}
		ret, _ := computeRollingStats(mixed, 2)
		if len(ret) != 1 {
			t.Errorf("expected 1 valid period, got %d", len(ret))
		}
		if len(ret) > 0 && !near(ret[0], 20.0) {
			t.Errorf("return = %v; want 20.0 ((120/100-1)*100)", ret[0])
		}
	})
}

// ─── CAGR annualisation ───────────────────────────────────────────────────────
//
// NAV doubles in exactly 1 year → CAGR = 2^(365.25/calDays)-1 ≈ 100.05%.
// Uses actual calendar days (not a fixed 365) so leap years are handled.

func TestCAGRAnnualisation(t *testing.T) {
	start := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(1, 0, 0)
	calDays := end.Sub(start).Hours() / 24
	rows := []models.NAVRow{{NavDate: start, NAV: 100}, {NavDate: end, NAV: 200}}

	_, cagrs := computeRollingStats(rows, 1)
	if len(cagrs) != 1 {
		t.Fatalf("expected 1 CAGR, got %d", len(cagrs))
	}
	expected := (math.Pow(2.0, 365.25/calDays) - 1) * 100
	if !near(cagrs[0], expected) {
		t.Errorf("CAGR = %v; want %v", cagrs[0], expected)
	}
}

// ─── computeWindow ────────────────────────────────────────────────────────────

// TestComputeWindow_InsufficientData — fewer rows than windowDays must set
// InsufficientData=true, leave rolling metrics nil, but still set MaxDrawdown.
func TestComputeWindow_InsufficientData(t *testing.T) {
	navs := []models.NAVRow{
		navRow("2024-01-01", 100),
		navRow("2024-01-02", 110),
		navRow("2024-01-03", 120),
	}
	dd := computeMaxDrawdown(navs)
	for _, w := range []string{"1Y", "3Y", "5Y", "10Y"} {
		t.Run(w, func(t *testing.T) {
			a := computeWindow("TEST", w, navs, dd)
			if !a.InsufficientData {
				t.Errorf("window %s: expected InsufficientData=true with 3 rows", w)
			}
			if a.RollingMin != nil || a.RollingMax != nil || a.RollingMedian != nil {
				t.Errorf("window %s: rolling metrics must be nil for insufficient data", w)
			}
			if a.MaxDrawdown == nil {
				t.Errorf("window %s: MaxDrawdown must always be set", w)
			}
		})
	}
}

// TestComputeWindow_SufficientData — 300-row monotonically-growing series;
// 1Y window (252 trading days) should produce 48 valid periods with all stats set.
//
// Periods = 300 − 252 = 48
// Monotonically increasing NAVs → max drawdown = 0.
func TestComputeWindow_SufficientData(t *testing.T) {
	base := time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC)
	navs := make([]models.NAVRow, 300)
	nav := 100.0
	for i := range navs {
		navs[i] = models.NAVRow{NavDate: base.AddDate(0, 0, i), NAV: nav}
		nav *= 1.001
	}
	dd := computeMaxDrawdown(navs)
	a := computeWindow("TEST", "1Y", navs, dd)

	if a.InsufficientData {
		t.Fatal("expected sufficient data for 1Y with 300 rows")
	}
	if a.RollingMin == nil || a.RollingMax == nil || a.RollingMedian == nil {
		t.Fatal("expected non-nil rolling stats")
	}
	if *a.RollingMin <= 0 {
		t.Errorf("RollingMin should be positive for growing NAVs, got %v", *a.RollingMin)
	}
	if *a.RollingMedian < *a.RollingMin || *a.RollingMedian > *a.RollingMax {
		t.Errorf("median %v not in [%v, %v]", *a.RollingMedian, *a.RollingMin, *a.RollingMax)
	}
	if *a.MaxDrawdown != 0 {
		t.Errorf("max drawdown should be 0 for monotonically increasing NAVs, got %v", *a.MaxDrawdown)
	}
	if *a.RollingPeriodsAnalyzed != 48 {
		t.Errorf("expected 48 periods (300-252), got %d", *a.RollingPeriodsAnalyzed)
	}
}

// ─── end-to-end manual verification ─────────────────────────────────────────
//
// 10-year NAV series (annual steps):
//
//	Yr0=100, Yr1=120, Yr2=110, Yr3=130, Yr4=80,
//	Yr5=150, Yr6=140, Yr7=180, Yr8=200, Yr9=220
//
// Max drawdown: peak=Yr3=130, trough=Yr4=80 → (80-130)/130*100 = -38.46%
//
// Rolling returns (windowDays=4, 6 periods):
//
//	[0→4]=−20%, [1→5]=+25%, [2→6]=+27.27%,
//	[3→7]=+38.46%, [4→8]=+150%, [5→9]=+46.67%
//
// Sorted: [−20, 25, 27.27, 38.46, 46.67, 150]
// Median (linear interp of idx 2 & 3) = (27.27+38.46)/2 ≈ 32.86%

func TestManualVerification(t *testing.T) {
	navs := []models.NAVRow{
		navRow("2014-01-01", 100), navRow("2015-01-01", 120),
		navRow("2016-01-01", 110), navRow("2017-01-01", 130),
		navRow("2018-01-01", 80), navRow("2019-01-01", 150),
		navRow("2020-01-01", 140), navRow("2021-01-01", 180),
		navRow("2022-01-01", 200), navRow("2023-01-01", 220),
	}

	t.Run("max drawdown = -38.46pct", func(t *testing.T) {
		want := (80.0 - 130.0) / 130.0 * 100
		if got := computeMaxDrawdown(navs); !near(got, want) {
			t.Errorf("got %v; want %v", got, want)
		}
	})

	t.Run("6 rolling periods with correct individual returns", func(t *testing.T) {
		returns, _ := computeRollingStats(navs, 4)
		if len(returns) != 6 {
			t.Fatalf("expected 6 periods, got %d", len(returns))
		}
		want := []float64{
			(80.0/100.0 - 1) * 100,
			(150.0/120.0 - 1) * 100,
			(140.0/110.0 - 1) * 100,
			(180.0/130.0 - 1) * 100,
			(200.0/80.0 - 1) * 100,
			(220.0/150.0 - 1) * 100,
		}
		for i, w := range want {
			if !near(returns[i], w) {
				t.Errorf("returns[%d] = %v; want %v", i, returns[i], w)
			}
		}
	})

	t.Run("sorted returns: min=-20 max=150 median≈32.86", func(t *testing.T) {
		returns, _ := computeRollingStats(navs, 4)
		sort.Float64s(returns)

		if !near(returns[0], (80.0/100.0-1)*100) {
			t.Errorf("min = %v; want -20pct", returns[0])
		}
		if !near(returns[len(returns)-1], (200.0/80.0-1)*100) {
			t.Errorf("max = %v; want 150pct", returns[len(returns)-1])
		}
		wantMedian := ((140.0/110.0-1)*100 + (180.0/130.0-1)*100) / 2.0
		if med := percentile(returns, 50); !near(med, wantMedian) {
			t.Errorf("median = %v; want ~%v", med, wantMedian)
		}
	})
}
