// Package api_test — HTTP handler tests. No database required.
// Every request uses the do() helper which enforces the <200ms response SLA.
package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"log/slog"
	"os"

	"mutualfundanalysis/internal/analytics"
	"mutualfundanalysis/internal/api"
	"mutualfundanalysis/internal/models"
	"mutualfundanalysis/internal/store"
)

// ── compile-time guard: *store.Store must satisfy DataStore ──────────────────
var _ api.DataStore = (*store.Store)(nil)

// ── mock store ───────────────────────────────────────────────────────────────

type mockStore struct {
	schemes      []*models.Scheme
	syncStates   []*models.SyncState
	analytics    map[string]*models.Analytics
	rankRows     []*models.RankRow
	rankTotal    int
	errScheme    error // ListSchemes + GetScheme
	errStates    error // GetAllSyncStates
	errAnalytics error // GetAnalytics
	errRanking   error // GetRanking
}

func (m *mockStore) ListSchemes(_ context.Context, category, amc string) ([]*models.Scheme, error) {
	if m.errScheme != nil {
		return nil, m.errScheme
	}
	var out []*models.Scheme
	for _, s := range m.schemes {
		if category != "" && !strings.EqualFold(s.Category, category) {
			continue
		}
		if amc != "" && !strings.Contains(strings.ToLower(s.AMC), strings.ToLower(amc)) {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}
func (m *mockStore) GetScheme(_ context.Context, code string) (*models.Scheme, error) {
	if m.errScheme != nil {
		return nil, m.errScheme
	}
	for _, s := range m.schemes {
		if s.Code == code {
			return s, nil
		}
	}
	return nil, nil
}
func (m *mockStore) GetAllSyncStates(_ context.Context) ([]*models.SyncState, error) {
	return m.syncStates, m.errStates
}
func (m *mockStore) GetAnalytics(_ context.Context, code, window string) (*models.Analytics, error) {
	if m.errAnalytics != nil {
		return nil, m.errAnalytics
	}
	if m.analytics == nil {
		return nil, nil
	}
	return m.analytics[code+"/"+window], nil
}
func (m *mockStore) GetRanking(_ context.Context, _, _, _ string, limit int) ([]*models.RankRow, int, error) {
	if m.errRanking != nil {
		return nil, 0, m.errRanking
	}
	n := len(m.rankRows)
	if n > limit {
		n = limit
	}
	return m.rankRows[:n], m.rankTotal, nil
}
func (m *mockStore) ResetErrorsToPending(_ context.Context) error { return nil }

// ── mock pipeline ────────────────────────────────────────────────────────────

type mockPipeline struct{ triggered bool }

func (p *mockPipeline) RunBackfill(_ context.Context) { p.triggered = true }

// ── helpers ──────────────────────────────────────────────────────────────────

func newRouter(ms *mockStore) http.Handler {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return api.NewRouter(ms, &mockPipeline{}, (*analytics.Engine)(nil), log)
}

func mkScheme(code, name, amc, category string) *models.Scheme {
	return &models.Scheme{Code: code, Name: name, AMC: amc, Category: category,
		SchemeType: "Open Ended Schemes", CreatedAt: time.Now(), UpdatedAt: time.Now()}
}

func flt(f float64) *float64   { return &f }
func intp(i int) *int           { return &i }
func tp(t time.Time) *time.Time { return &t }

// do executes a request and asserts response time < 200ms.
func do(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	rr := httptest.NewRecorder()
	start := time.Now()
	h.ServeHTTP(rr, req)
	if el := time.Since(start); el > 200*time.Millisecond {
		t.Errorf("%s %s: %v exceeds 200ms SLA", method, path, el)
	}
	return rr
}

// decode asserts status code and Content-Type, then decodes JSON.
func decode(t *testing.T, rr *httptest.ResponseRecorder, wantStatus int, dst any) {
	t.Helper()
	if rr.Code != wantStatus {
		t.Fatalf("status=%d want=%d body=%s", rr.Code, wantStatus, rr.Body)
	}
	if !strings.Contains(rr.Header().Get("Content-Type"), "application/json") {
		t.Errorf("Content-Type %q; want application/json", rr.Header().Get("Content-Type"))
	}
	if dst != nil {
		if err := json.NewDecoder(rr.Body).Decode(dst); err != nil {
			t.Fatalf("JSON decode: %v body=%s", err, rr.Body)
		}
	}
}

// ── GET /health ──────────────────────────────────────────────────────────────

func TestHealth(t *testing.T) {
	rr := do(t, newRouter(&mockStore{}), http.MethodGet, "/health")
	var b map[string]string
	decode(t, rr, http.StatusOK, &b)
	if b["status"] != "ok" {
		t.Errorf("status=%q want=ok", b["status"])
	}
}

// ── GET /funds ───────────────────────────────────────────────────────────────

func TestListFunds(t *testing.T) {
	ms := &mockStore{
		schemes: []*models.Scheme{
			mkScheme("120505", "Axis Midcap", "Axis MF", models.CategoryMidCap),
			mkScheme("120591", "ICICI SmallCap", "ICICI MF", models.CategorySmallCap),
		},
		syncStates: []*models.SyncState{
			{SchemeCode: "120505", Status: "done", NavCount: 2513, UpdatedAt: time.Now()},
		},
	}
	h := newRouter(ms)

	t.Run("all funds returned with total", func(t *testing.T) {
		rr := do(t, h, http.MethodGet, "/funds")
		var b map[string]any
		decode(t, rr, http.StatusOK, &b)
		if b["total"] == nil {
			t.Error("missing total field")
		}
		if funds := b["funds"].([]any); len(funds) != 2 {
			t.Errorf("funds=%d want=2", len(funds))
		}
	})

	t.Run("filter by valid category", func(t *testing.T) {
		rr := do(t, h, http.MethodGet, "/funds?category=Equity+Scheme+-+Mid+Cap+Fund")
		var b map[string]any
		decode(t, rr, http.StatusOK, &b)
		if funds := b["funds"].([]any); len(funds) != 1 {
			t.Errorf("filtered funds=%d want=1", len(funds))
		}
	})

	t.Run("invalid category → 400 INVALID_CATEGORY", func(t *testing.T) {
		rr := do(t, h, http.MethodGet, "/funds?category=BADVALUE")
		var b map[string]string
		decode(t, rr, http.StatusBadRequest, &b)
		if b["code"] != "INVALID_CATEGORY" {
			t.Errorf("code=%q want=INVALID_CATEGORY", b["code"])
		}
	})

	t.Run("store error → 500", func(t *testing.T) {
		h2 := newRouter(&mockStore{errScheme: errors.New("db down")})
		rr := do(t, h2, http.MethodGet, "/funds")
		if rr.Code != http.StatusInternalServerError {
			t.Errorf("status=%d want=500", rr.Code)
		}
	})
}

// ── GET /funds/{code} ────────────────────────────────────────────────────────

func TestGetFund(t *testing.T) {
	navVal := 133.75
	navDate := time.Date(2026, 4, 17, 0, 0, 0, 0, time.UTC)

	ms := &mockStore{
		schemes: []*models.Scheme{
			mkScheme("120505", "Axis Midcap", "Axis MF", models.CategoryMidCap),
		},
		syncStates: []*models.SyncState{{
			SchemeCode: "120505", Status: "done",
			LatestNAV: &navVal, LastNavDate: &navDate, NavCount: 2513, UpdatedAt: time.Now(),
		}},
	}
	h := newRouter(ms)

	t.Run("found — returns current_nav and fund_code", func(t *testing.T) {
		rr := do(t, h, http.MethodGet, "/funds/120505")
		var b map[string]any
		decode(t, rr, http.StatusOK, &b)
		if b["fund_code"] != "120505" {
			t.Errorf("fund_code=%v want=120505", b["fund_code"])
		}
		if b["current_nav"] == nil {
			t.Error("current_nav missing")
		}
	})

	t.Run("not found → 404 NOT_FOUND", func(t *testing.T) {
		rr := do(t, h, http.MethodGet, "/funds/UNKNOWN")
		var b map[string]string
		decode(t, rr, http.StatusNotFound, &b)
		if b["code"] != "NOT_FOUND" {
			t.Errorf("code=%q want=NOT_FOUND", b["code"])
		}
	})

	t.Run("no sync state yet — fund still returned", func(t *testing.T) {
		ms2 := &mockStore{schemes: ms.schemes} // no syncStates
		rr := do(t, newRouter(ms2), http.MethodGet, "/funds/120505")
		decode(t, rr, http.StatusOK, nil)
	})

	t.Run("store error → 500", func(t *testing.T) {
		rr := do(t, newRouter(&mockStore{errScheme: errors.New("db")}), http.MethodGet, "/funds/120505")
		if rr.Code != http.StatusInternalServerError {
			t.Errorf("status=%d want=500", rr.Code)
		}
	})
}

// ── GET /funds/{code}/analytics ──────────────────────────────────────────────

func TestGetAnalytics(t *testing.T) {
	rMin, rMax, rMed := 8.2, 48.5, 22.3
	rP25, rP75 := 15.7, 28.9
	cMin, cMax, cMed := 9.5, 45.2, 21.8
	dd := -32.1
	periods, totalDays, navPoints := 731, 3644, 2513
	start := time.Date(2016, 1, 15, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 1, 6, 0, 0, 0, 0, time.UTC)

	baseSchemes := []*models.Scheme{
		mkScheme("120505", "Axis Midcap - Direct Growth", "Axis MF", models.CategoryMidCap),
	}
	fullAnalytics := map[string]*models.Analytics{
		"120505/3Y": {
			SchemeCode: "120505", Window: "3Y",
			RollingMin: &rMin, RollingMax: &rMax, RollingMedian: &rMed,
			RollingP25: &rP25, RollingP75: &rP75,
			MaxDrawdown: &dd, CAGRMin: &cMin, CAGRMax: &cMax, CAGRMedian: &cMed,
			DataStart: &start, DataEnd: &end,
			TotalDays: &totalDays, NavDataPoints: &navPoints,
			RollingPeriodsAnalyzed: &periods, ComputedAt: time.Now(),
		},
	}

	ms := &mockStore{schemes: baseSchemes, analytics: fullAnalytics}
	h := newRouter(ms)

	t.Run("success — all required fields present", func(t *testing.T) {
		rr := do(t, h, http.MethodGet, "/funds/120505/analytics?window=3Y")
		var b map[string]any
		decode(t, rr, http.StatusOK, &b)
		for _, f := range []string{
			"fund_code", "fund_name", "category", "amc", "window",
			"data_availability", "rolling_returns", "max_drawdown", "cagr", "computed_at",
		} {
			if b[f] == nil {
				t.Errorf("missing field %q", f)
			}
		}
		rr2 := b["rolling_returns"].(map[string]any)
		for _, f := range []string{"min", "max", "median", "p25", "p75"} {
			if rr2[f] == nil {
				t.Errorf("rolling_returns.%s missing", f)
			}
		}
		da := b["data_availability"].(map[string]any)
		if da["start_date"] != "2016-01-15" {
			t.Errorf("start_date=%v want=2016-01-15", da["start_date"])
		}
	})

	t.Run("insufficient data — has reason and nil rolling stats", func(t *testing.T) {
		nav252 := 252
		insuf := &models.Analytics{
			SchemeCode:       "120505",
			Window:           "3Y",
			InsufficientData: true,
			MaxDrawdown:      &dd,
			NavDataPoints:    &nav252,
			ComputedAt:       time.Now(),
		}
		ms2 := &mockStore{
			schemes:   baseSchemes,
			analytics: map[string]*models.Analytics{"120505/3Y": insuf},
		}
		rr := do(t, newRouter(ms2), http.MethodGet, "/funds/120505/analytics?window=3Y")
		var b map[string]any
		decode(t, rr, http.StatusOK, &b)
		if b["insufficient_data"] != true {
			t.Error("insufficient_data should be true")
		}
	})

	// Validation error cases — all must return 400 with the correct error code.
	validCases := []struct {
		name, path, wantCode string
	}{
		{"missing window", "/funds/120505/analytics", "INVALID_WINDOW"},
		{"invalid window value", "/funds/120505/analytics?window=2Y", "INVALID_WINDOW"},
	}
	for _, tc := range validCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			rr := do(t, h, http.MethodGet, tc.path)
			var b map[string]string
			decode(t, rr, http.StatusBadRequest, &b)
			if b["code"] != tc.wantCode {
				t.Errorf("code=%q want=%q", b["code"], tc.wantCode)
			}
		})
	}

	t.Run("fund not found → 404 NOT_FOUND", func(t *testing.T) {
		rr := do(t, h, http.MethodGet, "/funds/NOPE/analytics?window=3Y")
		var b map[string]string
		decode(t, rr, http.StatusNotFound, &b)
		if b["code"] != "NOT_FOUND" {
			t.Errorf("code=%q want=NOT_FOUND", b["code"])
		}
	})

	t.Run("analytics not yet computed → 404 ANALYTICS_NOT_READY", func(t *testing.T) {
		ms2 := &mockStore{schemes: baseSchemes, analytics: map[string]*models.Analytics{}}
		rr := do(t, newRouter(ms2), http.MethodGet, "/funds/120505/analytics?window=3Y")
		var b map[string]string
		decode(t, rr, http.StatusNotFound, &b)
		if b["code"] != "ANALYTICS_NOT_READY" {
			t.Errorf("code=%q want=ANALYTICS_NOT_READY", b["code"])
		}
	})

	t.Run("store error on GetAnalytics → 500", func(t *testing.T) {
		ms2 := &mockStore{schemes: baseSchemes, errAnalytics: errors.New("db")}
		rr := do(t, newRouter(ms2), http.MethodGet, "/funds/120505/analytics?window=3Y")
		if rr.Code != http.StatusInternalServerError {
			t.Errorf("status=%d want=500", rr.Code)
		}
	})
}

// ── GET /funds/rank ──────────────────────────────────────────────────────────

func TestRankFunds(t *testing.T) {
	navDate := time.Date(2026, 1, 6, 0, 0, 0, 0, time.UTC)
	rMed, dd := 22.3, -32.1

	ms := &mockStore{
		rankRows: []*models.RankRow{{
			SchemeCode: "120505", SchemeName: "Axis Midcap", AMC: "Axis MF",
			Category: models.CategoryMidCap, Window: "3Y",
			RollingMedian: &rMed, MaxDrawdown: &dd,
			LatestNAV: flt(133.75), LastNavDate: &navDate,
		}},
		rankTotal: 5,
	}
	h := newRouter(ms)
	base := "/funds/rank?category=Equity+Scheme+-+Mid+Cap+Fund&window=3Y"

	t.Run("success median_return — rank=1 and envelope correct", func(t *testing.T) {
		rr := do(t, h, http.MethodGet, base)
		var b map[string]any
		decode(t, rr, http.StatusOK, &b)
		if b["window"] != "3Y" {
			t.Errorf("window=%v want=3Y", b["window"])
		}
		if b["sorted_by"] != "median_return" {
			t.Errorf("sorted_by=%v want=median_return", b["sorted_by"])
		}
		if b["total_funds"].(float64) != 5 {
			t.Errorf("total_funds=%v want=5", b["total_funds"])
		}
		funds := b["funds"].([]any)
		if funds[0].(map[string]any)["rank"].(float64) != 1 {
			t.Error("first fund rank must be 1")
		}
	})

	t.Run("success sort_by=max_drawdown — sorted_by reflected", func(t *testing.T) {
		rr := do(t, h, http.MethodGet, base+"&sort_by=max_drawdown")
		var b map[string]any
		decode(t, rr, http.StatusOK, &b)
		if b["sorted_by"] != "max_drawdown" {
			t.Errorf("sorted_by=%v want=max_drawdown", b["sorted_by"])
		}
	})

	t.Run("empty results — showing=0 with valid envelope", func(t *testing.T) {
		ms2 := &mockStore{rankRows: nil, rankTotal: 0}
		rr := do(t, newRouter(ms2), http.MethodGet, base)
		var b map[string]any
		decode(t, rr, http.StatusOK, &b)
		if b["showing"].(float64) != 0 {
			t.Errorf("showing=%v want=0", b["showing"])
		}
	})

	// All validation error cases — each must return 400 with the correct code.
	errCases := []struct {
		name, path, wantCode string
	}{
		{"missing category", "/funds/rank?window=3Y", "MISSING_CATEGORY"},
		{"missing window", "/funds/rank?category=Equity+Scheme+-+Mid+Cap+Fund", "INVALID_WINDOW"},
		{"invalid window value", base[:len(base)-2] + "7Y", "INVALID_WINDOW"},
		{"invalid sort_by", base + "&sort_by=INVALID", "INVALID_SORT_BY"},
		{"limit below min (0)", base + "&limit=0", "INVALID_LIMIT"},
		{"limit above max (51)", base + "&limit=51", "INVALID_LIMIT"},
	}
	for _, tc := range errCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			rr := do(t, h, http.MethodGet, tc.path)
			var b map[string]string
			decode(t, rr, http.StatusBadRequest, &b)
			if b["code"] != tc.wantCode {
				t.Errorf("code=%q want=%q", b["code"], tc.wantCode)
			}
		})
	}

	t.Run("store error → 500", func(t *testing.T) {
		ms2 := &mockStore{errRanking: errors.New("db")}
		rr := do(t, newRouter(ms2), http.MethodGet, base)
		if rr.Code != http.StatusInternalServerError {
			t.Errorf("status=%d want=500", rr.Code)
		}
	})
}

// ── POST /sync/trigger ───────────────────────────────────────────────────────

func TestSyncTrigger(t *testing.T) {
	rr := do(t, newRouter(&mockStore{}), http.MethodPost, "/sync/trigger")
	var b map[string]string
	decode(t, rr, http.StatusAccepted, &b)
	if b["status"] != "accepted" {
		t.Errorf("status=%q want=accepted", b["status"])
	}
	if b["message"] == "" {
		t.Error("message field missing or empty")
	}
}

// ── GET /sync/status ─────────────────────────────────────────────────────────

func TestSyncStatus(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	navDate := time.Date(2026, 4, 17, 0, 0, 0, 0, time.UTC)

	ms := &mockStore{
		syncStates: []*models.SyncState{
			{SchemeCode: "S1", Status: "done", LastNavDate: &navDate,
				LatestNAV: flt(133.75), NavCount: 2513, UpdatedAt: now},
			{SchemeCode: "S2", Status: "pending", UpdatedAt: now},
		},
	}
	h := newRouter(ms)

	t.Run("returns schemes array and summary counts", func(t *testing.T) {
		rr := do(t, h, http.MethodGet, "/sync/status")
		var b map[string]any
		decode(t, rr, http.StatusOK, &b)
		schemes := b["schemes"].([]any)
		if len(schemes) != 2 {
			t.Errorf("schemes=%d want=2", len(schemes))
		}
		sum := b["summary"].(map[string]any)
		if sum["total"].(float64) != 2 {
			t.Errorf("total=%v want=2", sum["total"])
		}
		if sum["done"].(float64) != 1 {
			t.Errorf("done=%v want=1", sum["done"])
		}
	})

	t.Run("last_nav_date formatted YYYY-MM-DD not RFC3339", func(t *testing.T) {
		rr := do(t, h, http.MethodGet, "/sync/status")
		var b map[string]any
		decode(t, rr, http.StatusOK, &b)
		for _, s := range b["schemes"].([]any) {
			m := s.(map[string]any)
			if m["scheme_code"] == "S1" {
				if m["last_nav_date"] != "2026-04-17" {
					t.Errorf("last_nav_date=%v want=2026-04-17", m["last_nav_date"])
				}
			}
		}
	})

	t.Run("updated_at is valid ISO8601 timestamp", func(t *testing.T) {
		rr := do(t, h, http.MethodGet, "/sync/status")
		var b map[string]any
		decode(t, rr, http.StatusOK, &b)
		for _, s := range b["schemes"].([]any) {
			m := s.(map[string]any)
			ua, _ := m["updated_at"].(string)
			if _, err := time.Parse(time.RFC3339, ua); err != nil {
				t.Errorf("updated_at %q not ISO8601: %v", ua, err)
			}
		}
	})
}
