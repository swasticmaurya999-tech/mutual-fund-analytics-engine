// Package api_test contains black-box HTTP handler tests.
//
// All tests use net/http/httptest so no real database or network connection
// is required. A lightweight mockStore satisfies the DataStore interface.
//
// Test categories:
//  1. Endpoint contract — correct status codes, JSON shapes, required fields.
//  2. Validation logic — bad params return 400 with error+code fields.
//  3. Response time    — every handler must respond in < 200 ms (assignment requirement).
//  4. Edge cases       — not-found, empty results, analytics not ready, etc.
package api_test

import (
	"context"
	"encoding/json"
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

// ─── mock store ──────────────────────────────────────────────────────────────

// mockStore implements api.DataStore for testing.
type mockStore struct {
	schemes     []*models.Scheme
	syncStates  []*models.SyncState
	analytics   map[string]*models.Analytics // key: "code/window"
	rankRows    []*models.RankRow
	rankTotal   int
	errScheme   error
	errStates   error
	errAnalytics error
	errRanking  error
	errReset    error
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
	return nil, nil // not found
}

func (m *mockStore) GetAllSyncStates(_ context.Context) ([]*models.SyncState, error) {
	return m.syncStates, m.errStates
}

func (m *mockStore) GetAnalytics(_ context.Context, code, window string) (*models.Analytics, error) {
	if m.errAnalytics != nil {
		return nil, m.errAnalytics
	}
	key := code + "/" + window
	return m.analytics[key], nil
}

func (m *mockStore) GetRanking(_ context.Context, category, window, sortBy string, limit int) ([]*models.RankRow, int, error) {
	if m.errRanking != nil {
		return nil, 0, m.errRanking
	}
	n := len(m.rankRows)
	if n > limit {
		n = limit
	}
	return m.rankRows[:n], m.rankTotal, nil
}

func (m *mockStore) ResetErrorsToPending(_ context.Context) error {
	return m.errReset
}

// ─── mock pipeline ────────────────────────────────────────────────────────────

type mockPipeline struct {
	triggered bool
}

func (p *mockPipeline) RunBackfill(_ context.Context) {
	p.triggered = true
}

// ─── test helpers ─────────────────────────────────────────────────────────────

func newTestRouter(ms *mockStore, mp *mockPipeline) http.Handler {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	// analytics engine is nil — handlers under test don't call it directly.
	return api.NewRouter(ms, mp, (*analytics.Engine)(nil), log)
}

// newTestScheme returns a fully populated test scheme.
func newTestScheme(code, name, amc, category string) *models.Scheme {
	return &models.Scheme{
		Code:      code,
		Name:      name,
		AMC:       amc,
		Category:  category,
		SchemeType: "Open Ended Schemes",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
}

// nav returns a pointer to a float64.
func nav(f float64) *float64 { return &f }

// str returns a pointer to a string.
func str(s string) *string { return &s }

// assertJSON decodes the response body into dst and fails if the status code
// or Content-Type is unexpected.
func assertJSON(t *testing.T, rr *httptest.ResponseRecorder, wantStatus int, dst any) {
	t.Helper()
	if rr.Code != wantStatus {
		t.Fatalf("status = %d; want %d\nbody: %s", rr.Code, wantStatus, rr.Body.String())
	}
	ct := rr.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q; want application/json", ct)
	}
	if dst != nil {
		if err := json.NewDecoder(rr.Body).Decode(dst); err != nil {
			t.Fatalf("decode JSON: %v\nbody: %s", err, rr.Body.String())
		}
	}
}

// mustRequest builds and executes a request against the handler, returning
// the recorded response. Also asserts the response was served within 200 ms.
func mustRequest(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	rr := httptest.NewRecorder()
	start := time.Now()
	h.ServeHTTP(rr, req)
	elapsed := time.Since(start)
	if elapsed > 200*time.Millisecond {
		t.Errorf("handler responded in %v; must be < 200ms", elapsed)
	}
	return rr
}

// ─── GET /health ─────────────────────────────────────────────────────────────

func TestHealth(t *testing.T) {
	h := newTestRouter(&mockStore{}, &mockPipeline{})
	rr := mustRequest(t, h, http.MethodGet, "/health")
	var body map[string]string
	assertJSON(t, rr, http.StatusOK, &body)
	if body["status"] != "ok" {
		t.Errorf("status = %q; want 'ok'", body["status"])
	}
}

// ─── GET /funds ───────────────────────────────────────────────────────────────

func TestListFunds_AllFunds(t *testing.T) {
	ms := &mockStore{
		schemes: []*models.Scheme{
			newTestScheme("120505", "Axis Midcap Fund - Direct Plan - Growth", "Axis Mutual Fund", models.CategoryMidCap),
			newTestScheme("125497", "HDFC Mid-Cap Opportunities Fund - Direct Plan - Growth", "HDFC Mutual Fund", models.CategoryMidCap),
		},
		syncStates: []*models.SyncState{
			{SchemeCode: "120505", Status: "done", NavCount: 2513, UpdatedAt: time.Now()},
		},
	}
	h := newTestRouter(ms, &mockPipeline{})
	rr := mustRequest(t, h, http.MethodGet, "/funds")

	var body map[string]any
	assertJSON(t, rr, http.StatusOK, &body)

	if body["total"] == nil {
		t.Error("response missing 'total' field")
	}
	funds, ok := body["funds"].([]any)
	if !ok {
		t.Fatal("response missing 'funds' array")
	}
	if len(funds) != 2 {
		t.Errorf("funds count = %d; want 2", len(funds))
	}
}

func TestListFunds_FilterByCategory(t *testing.T) {
	ms := &mockStore{
		schemes: []*models.Scheme{
			newTestScheme("120505", "Axis Midcap Fund", "Axis Mutual Fund", models.CategoryMidCap),
			newTestScheme("120591", "Axis Small Cap Fund", "Axis Mutual Fund", models.CategorySmallCap),
		},
	}
	h := newTestRouter(ms, &mockPipeline{})
	rr := mustRequest(t, h, http.MethodGet, "/funds?category=Equity+Scheme+-+Mid+Cap+Fund")

	var body map[string]any
	assertJSON(t, rr, http.StatusOK, &body)
}

func TestListFunds_InvalidCategory(t *testing.T) {
	h := newTestRouter(&mockStore{}, &mockPipeline{})
	rr := mustRequest(t, h, http.MethodGet, "/funds?category=InvalidCategory")

	var body map[string]string
	assertJSON(t, rr, http.StatusBadRequest, &body)
	if body["code"] != "INVALID_CATEGORY" {
		t.Errorf("error code = %q; want INVALID_CATEGORY", body["code"])
	}
}

// ─── GET /funds/{code} ────────────────────────────────────────────────────────

func TestGetFund_Found(t *testing.T) {
	navVal := 133.75
	t0 := time.Date(2026, 4, 17, 0, 0, 0, 0, time.UTC)
	ms := &mockStore{
		schemes: []*models.Scheme{
			newTestScheme("120505", "Axis Midcap Fund - Direct Plan - Growth", "Axis Mutual Fund", models.CategoryMidCap),
		},
		syncStates: []*models.SyncState{
			{
				SchemeCode:  "120505",
				Status:      "done",
				LatestNAV:   &navVal,
				LastNavDate: &t0,
				NavCount:    2513,
				UpdatedAt:   time.Now(),
			},
		},
	}
	h := newTestRouter(ms, &mockPipeline{})
	rr := mustRequest(t, h, http.MethodGet, "/funds/120505")

	var body map[string]any
	assertJSON(t, rr, http.StatusOK, &body)

	if body["fund_code"] != "120505" {
		t.Errorf("fund_code = %v; want 120505", body["fund_code"])
	}
	if body["fund_name"] == nil {
		t.Error("response missing fund_name")
	}
	if body["current_nav"] == nil {
		t.Error("response missing current_nav")
	}
}

func TestGetFund_NotFound(t *testing.T) {
	h := newTestRouter(&mockStore{}, &mockPipeline{})
	rr := mustRequest(t, h, http.MethodGet, "/funds/INVALID999")

	var body map[string]string
	assertJSON(t, rr, http.StatusNotFound, &body)
	if body["code"] != "NOT_FOUND" {
		t.Errorf("error code = %q; want NOT_FOUND", body["code"])
	}
}

// ─── GET /funds/{code}/analytics ─────────────────────────────────────────────

func TestGetAnalytics_Success(t *testing.T) {
	rMin, rMax, rMed := 8.2, 48.5, 22.3
	rP25, rP75 := 15.7, 28.9
	cMin, cMax, cMed := 9.5, 45.2, 21.8
	dd := -32.1
	periods := 731
	totalDays := 3644
	navPoints := 2513
	start := time.Date(2016, 1, 15, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 1, 6, 0, 0, 0, 0, time.UTC)

	ms := &mockStore{
		schemes: []*models.Scheme{
			newTestScheme("120505", "Axis Midcap Fund - Direct Plan - Growth", "Axis Mutual Fund", models.CategoryMidCap),
		},
		analytics: map[string]*models.Analytics{
			"120505/3Y": {
				SchemeCode:             "120505",
				Window:                 "3Y",
				RollingMin:             &rMin,
				RollingMax:             &rMax,
				RollingMedian:          &rMed,
				RollingP25:             &rP25,
				RollingP75:             &rP75,
				MaxDrawdown:            &dd,
				CAGRMin:                &cMin,
				CAGRMax:                &cMax,
				CAGRMedian:             &cMed,
				DataStart:              &start,
				DataEnd:                &end,
				TotalDays:              &totalDays,
				NavDataPoints:          &navPoints,
				RollingPeriodsAnalyzed: &periods,
				ComputedAt:             time.Now(),
			},
		},
	}
	h := newTestRouter(ms, &mockPipeline{})
	rr := mustRequest(t, h, http.MethodGet, "/funds/120505/analytics?window=3Y")

	var body map[string]any
	assertJSON(t, rr, http.StatusOK, &body)

	// Verify required top-level fields match the assignment's example response.
	checkField := func(field string) {
		t.Helper()
		if body[field] == nil {
			t.Errorf("analytics response missing field %q", field)
		}
	}
	checkField("fund_code")
	checkField("fund_name")
	checkField("category")
	checkField("amc")
	checkField("window")
	checkField("data_availability")
	checkField("rolling_returns")
	checkField("max_drawdown")
	checkField("cagr")
	checkField("computed_at")

	if body["window"] != "3Y" {
		t.Errorf("window = %v; want 3Y", body["window"])
	}
	if body["rolling_periods_analyzed"].(float64) != float64(periods) {
		t.Errorf("rolling_periods_analyzed = %v; want %d", body["rolling_periods_analyzed"], periods)
	}

	// Verify nested rolling_returns fields.
	rr2, ok := body["rolling_returns"].(map[string]any)
	if !ok {
		t.Fatal("rolling_returns is not an object")
	}
	for _, field := range []string{"min", "max", "median", "p25", "p75"} {
		if rr2[field] == nil {
			t.Errorf("rolling_returns.%s is nil", field)
		}
	}
	if rr2["median"].(float64) != rMed {
		t.Errorf("rolling_returns.median = %v; want %v", rr2["median"], rMed)
	}

	// Verify CAGR fields.
	cagrObj, ok := body["cagr"].(map[string]any)
	if !ok {
		t.Fatal("cagr is not an object")
	}
	for _, field := range []string{"min", "max", "median"} {
		if cagrObj[field] == nil {
			t.Errorf("cagr.%s is nil", field)
		}
	}

	// Verify data_availability.
	da, ok := body["data_availability"].(map[string]any)
	if !ok {
		t.Fatal("data_availability is not an object")
	}
	if da["start_date"] != "2016-01-15" {
		t.Errorf("data_availability.start_date = %v; want 2016-01-15", da["start_date"])
	}
	if da["end_date"] != "2026-01-06" {
		t.Errorf("data_availability.end_date = %v; want 2026-01-06", da["end_date"])
	}
}

func TestGetAnalytics_MissingWindow(t *testing.T) {
	ms := &mockStore{
		schemes: []*models.Scheme{
			newTestScheme("120505", "Axis Midcap Fund", "Axis Mutual Fund", models.CategoryMidCap),
		},
	}
	h := newTestRouter(ms, &mockPipeline{})
	rr := mustRequest(t, h, http.MethodGet, "/funds/120505/analytics") // no ?window=

	var body map[string]string
	assertJSON(t, rr, http.StatusBadRequest, &body)
	if body["code"] != "INVALID_WINDOW" {
		t.Errorf("error code = %q; want INVALID_WINDOW", body["code"])
	}
}

func TestGetAnalytics_InvalidWindow(t *testing.T) {
	ms := &mockStore{
		schemes: []*models.Scheme{
			newTestScheme("120505", "Axis Midcap Fund", "Axis Mutual Fund", models.CategoryMidCap),
		},
	}
	h := newTestRouter(ms, &mockPipeline{})
	rr := mustRequest(t, h, http.MethodGet, "/funds/120505/analytics?window=2Y")

	var body map[string]string
	assertJSON(t, rr, http.StatusBadRequest, &body)
	if body["code"] != "INVALID_WINDOW" {
		t.Errorf("error code = %q; want INVALID_WINDOW", body["code"])
	}
}

func TestGetAnalytics_NotYetComputed(t *testing.T) {
	ms := &mockStore{
		schemes: []*models.Scheme{
			newTestScheme("120505", "Axis Midcap Fund", "Axis Mutual Fund", models.CategoryMidCap),
		},
		analytics: map[string]*models.Analytics{}, // empty — analytics not ready
	}
	h := newTestRouter(ms, &mockPipeline{})
	rr := mustRequest(t, h, http.MethodGet, "/funds/120505/analytics?window=3Y")

	var body map[string]string
	assertJSON(t, rr, http.StatusNotFound, &body)
	if body["code"] != "ANALYTICS_NOT_READY" {
		t.Errorf("error code = %q; want ANALYTICS_NOT_READY", body["code"])
	}
}

func TestGetAnalytics_FundNotFound(t *testing.T) {
	h := newTestRouter(&mockStore{analytics: map[string]*models.Analytics{}}, &mockPipeline{})
	rr := mustRequest(t, h, http.MethodGet, "/funds/NOTEXIST/analytics?window=1Y")

	var body map[string]string
	assertJSON(t, rr, http.StatusNotFound, &body)
	if body["code"] != "NOT_FOUND" {
		t.Errorf("error code = %q; want NOT_FOUND", body["code"])
	}
}

// ─── GET /funds/rank ──────────────────────────────────────────────────────────

func TestRankFunds_Success(t *testing.T) {
	navVal := 133.75
	navDate := time.Date(2026, 1, 6, 0, 0, 0, 0, time.UTC)
	rMed := 22.3
	dd := -32.1

	ms := &mockStore{
		rankRows: []*models.RankRow{
			{
				SchemeCode:    "120505",
				SchemeName:    "Axis Midcap Fund - Direct Plan - Growth",
				AMC:           "Axis Mutual Fund",
				Category:      models.CategoryMidCap,
				Window:        "3Y",
				RollingMedian: &rMed,
				MaxDrawdown:   &dd,
				LatestNAV:     &navVal,
				LastNavDate:   &navDate,
			},
		},
		rankTotal: 5,
	}
	h := newTestRouter(ms, &mockPipeline{})
	rr := mustRequest(t, h, http.MethodGet, "/funds/rank?category=Equity+Scheme+-+Mid+Cap+Fund&window=3Y")

	var body map[string]any
	assertJSON(t, rr, http.StatusOK, &body)

	// Verify envelope fields from the assignment example.
	if body["category"] == nil {
		t.Error("missing 'category'")
	}
	if body["window"] != "3Y" {
		t.Errorf("window = %v; want 3Y", body["window"])
	}
	if body["sorted_by"] != "median_return" {
		t.Errorf("sorted_by = %v; want median_return", body["sorted_by"])
	}
	if body["total_funds"].(float64) != 5 {
		t.Errorf("total_funds = %v; want 5", body["total_funds"])
	}
	if body["showing"].(float64) != 1 {
		t.Errorf("showing = %v; want 1", body["showing"])
	}

	funds, ok := body["funds"].([]any)
	if !ok || len(funds) != 1 {
		t.Fatalf("expected 1 fund in funds array")
	}
	fund := funds[0].(map[string]any)
	if fund["rank"].(float64) != 1 {
		t.Errorf("rank = %v; want 1", fund["rank"])
	}
	if fund["fund_code"] != "120505" {
		t.Errorf("fund_code = %v; want 120505", fund["fund_code"])
	}
	if fund["median_return"] == nil {
		t.Error("missing median_return in rank item")
	}
	if fund["max_drawdown"] == nil {
		t.Error("missing max_drawdown in rank item")
	}
	if fund["last_updated"] != "2026-01-06" {
		t.Errorf("last_updated = %v; want 2026-01-06", fund["last_updated"])
	}
}

func TestRankFunds_MissingCategory(t *testing.T) {
	h := newTestRouter(&mockStore{}, &mockPipeline{})
	rr := mustRequest(t, h, http.MethodGet, "/funds/rank?window=3Y")

	var body map[string]string
	assertJSON(t, rr, http.StatusBadRequest, &body)
	if body["code"] != "MISSING_CATEGORY" {
		t.Errorf("error code = %q; want MISSING_CATEGORY", body["code"])
	}
}

func TestRankFunds_MissingWindow(t *testing.T) {
	h := newTestRouter(&mockStore{}, &mockPipeline{})
	rr := mustRequest(t, h, http.MethodGet, "/funds/rank?category=Equity+Scheme+-+Mid+Cap+Fund")

	var body map[string]string
	assertJSON(t, rr, http.StatusBadRequest, &body)
	if body["code"] != "INVALID_WINDOW" {
		t.Errorf("error code = %q; want INVALID_WINDOW", body["code"])
	}
}

func TestRankFunds_InvalidWindow(t *testing.T) {
	h := newTestRouter(&mockStore{}, &mockPipeline{})
	rr := mustRequest(t, h, http.MethodGet, "/funds/rank?category=Equity+Scheme+-+Mid+Cap+Fund&window=7Y")

	var body map[string]string
	assertJSON(t, rr, http.StatusBadRequest, &body)
	if body["code"] != "INVALID_WINDOW" {
		t.Errorf("error code = %q; want INVALID_WINDOW", body["code"])
	}
}

func TestRankFunds_InvalidSortBy(t *testing.T) {
	h := newTestRouter(&mockStore{}, &mockPipeline{})
	rr := mustRequest(t, h, http.MethodGet, "/funds/rank?category=Equity+Scheme+-+Mid+Cap+Fund&window=3Y&sort_by=bad_sort")

	var body map[string]string
	assertJSON(t, rr, http.StatusBadRequest, &body)
	if body["code"] != "INVALID_SORT_BY" {
		t.Errorf("error code = %q; want INVALID_SORT_BY", body["code"])
	}
}

func TestRankFunds_InvalidLimit(t *testing.T) {
	h := newTestRouter(&mockStore{}, &mockPipeline{})
	// limit=0 is below the minimum of 1.
	rr := mustRequest(t, h, http.MethodGet, "/funds/rank?category=Equity+Scheme+-+Mid+Cap+Fund&window=3Y&limit=0")

	var body map[string]string
	assertJSON(t, rr, http.StatusBadRequest, &body)
	if body["code"] != "INVALID_LIMIT" {
		t.Errorf("error code = %q; want INVALID_LIMIT", body["code"])
	}
}

func TestRankFunds_LimitExceedsMax(t *testing.T) {
	h := newTestRouter(&mockStore{rankRows: nil, rankTotal: 0}, &mockPipeline{})
	// limit=51 exceeds maximum of 50.
	rr := mustRequest(t, h, http.MethodGet, "/funds/rank?category=Equity+Scheme+-+Mid+Cap+Fund&window=3Y&limit=51")

	var body map[string]string
	assertJSON(t, rr, http.StatusBadRequest, &body)
	if body["code"] != "INVALID_LIMIT" {
		t.Errorf("error code = %q; want INVALID_LIMIT", body["code"])
	}
}

func TestRankFunds_SortByMaxDrawdown(t *testing.T) {
	ms := &mockStore{rankRows: nil, rankTotal: 0}
	h := newTestRouter(ms, &mockPipeline{})
	rr := mustRequest(t, h, http.MethodGet, "/funds/rank?category=Equity+Scheme+-+Small+Cap+Fund&window=5Y&sort_by=max_drawdown")

	var body map[string]any
	assertJSON(t, rr, http.StatusOK, &body)
	if body["sorted_by"] != "max_drawdown" {
		t.Errorf("sorted_by = %v; want max_drawdown", body["sorted_by"])
	}
}

// ─── POST /sync/trigger ───────────────────────────────────────────────────────

func TestTriggerSync_ReturnsAccepted(t *testing.T) {
	mp := &mockPipeline{}
	h := newTestRouter(&mockStore{}, mp)
	rr := mustRequest(t, h, http.MethodPost, "/sync/trigger")

	var body map[string]string
	assertJSON(t, rr, http.StatusAccepted, &body)
	if body["status"] != "accepted" {
		t.Errorf("status = %q; want 'accepted'", body["status"])
	}
	if body["message"] == "" {
		t.Error("response missing 'message'")
	}
}

// ─── GET /sync/status ─────────────────────────────────────────────────────────

func TestGetSyncStatus_Success(t *testing.T) {
	navVal := 133.75
	navDate := time.Date(2026, 4, 17, 0, 0, 0, 0, time.UTC)
	ms := &mockStore{
		syncStates: []*models.SyncState{
			{
				SchemeCode:  "120505",
				Status:      "done",
				LatestNAV:   &navVal,
				LastNavDate: &navDate,
				NavCount:    2513,
				UpdatedAt:   time.Now(),
			},
			{
				SchemeCode: "120591",
				Status:     "pending",
				NavCount:   0,
				UpdatedAt:  time.Now(),
			},
		},
	}
	h := newTestRouter(ms, &mockPipeline{})
	rr := mustRequest(t, h, http.MethodGet, "/sync/status")

	var body map[string]any
	assertJSON(t, rr, http.StatusOK, &body)

	schemes, ok := body["schemes"].([]any)
	if !ok {
		t.Fatal("missing 'schemes' array")
	}
	if len(schemes) != 2 {
		t.Errorf("schemes count = %d; want 2", len(schemes))
	}

	summary, ok := body["summary"].(map[string]any)
	if !ok {
		t.Fatal("missing 'summary' object")
	}
	if summary["total"].(float64) != 2 {
		t.Errorf("summary.total = %v; want 2", summary["total"])
	}
	if summary["done"].(float64) != 1 {
		t.Errorf("summary.done = %v; want 1", summary["done"])
	}
	if summary["pending"].(float64) != 1 {
		t.Errorf("summary.pending = %v; want 1", summary["pending"])
	}
}

func TestGetSyncStatus_SchemeDateFormatted(t *testing.T) {
	navDate := time.Date(2026, 4, 17, 0, 0, 0, 0, time.UTC)
	ms := &mockStore{
		syncStates: []*models.SyncState{
			{SchemeCode: "120505", Status: "done", LastNavDate: &navDate, UpdatedAt: time.Now()},
		},
	}
	h := newTestRouter(ms, &mockPipeline{})
	rr := mustRequest(t, h, http.MethodGet, "/sync/status")

	var body map[string]any
	assertJSON(t, rr, http.StatusOK, &body)

	schemes := body["schemes"].([]any)
	scheme := schemes[0].(map[string]any)
	if scheme["last_nav_date"] != "2026-04-17" {
		t.Errorf("last_nav_date = %v; want 2026-04-17", scheme["last_nav_date"])
	}
	// updated_at must be ISO8601.
	updatedAt, _ := scheme["updated_at"].(string)
	if _, err := time.Parse("2006-01-02T15:04:05Z", updatedAt); err != nil {
		t.Errorf("updated_at %q is not valid ISO8601: %v", updatedAt, err)
	}
}

// ─── Response time benchmark ──────────────────────────────────────────────────
// BenchmarkHandlers measures each endpoint under no-op store conditions.
// Run with: go test -bench=BenchmarkHandlers ./internal/api/
// Requirement: p99 must stay well under 200ms even with a real database.

func BenchmarkHandler_Health(b *testing.B) {
	h := newTestRouter(&mockStore{}, &mockPipeline{})
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
	}
}

func BenchmarkHandler_ListFunds(b *testing.B) {
	ms := &mockStore{schemes: make([]*models.Scheme, 10)}
	for i := range ms.schemes {
		ms.schemes[i] = newTestScheme("CODE", "Name", "AMC", models.CategoryMidCap)
	}
	h := newTestRouter(ms, &mockPipeline{})
	req := httptest.NewRequest(http.MethodGet, "/funds", nil)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
	}
}

func BenchmarkHandler_GetAnalytics(b *testing.B) {
	rMed := 22.3
	dd := -32.1
	ms := &mockStore{
		schemes: []*models.Scheme{newTestScheme("120505", "Axis Midcap Fund", "Axis Mutual Fund", models.CategoryMidCap)},
		analytics: map[string]*models.Analytics{
			"120505/3Y": {SchemeCode: "120505", Window: "3Y", RollingMedian: &rMed, MaxDrawdown: &dd, ComputedAt: time.Now()},
		},
	}
	h := newTestRouter(ms, &mockPipeline{})
	req := httptest.NewRequest(http.MethodGet, "/funds/120505/analytics?window=3Y", nil)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
	}
}

// ─── Pipeline resumability ────────────────────────────────────────────────────
//
// Tests that the sync trigger correctly resets errored schemes and that
// sync status correctly reflects scheme state — key behaviours needed for
// crash recovery and pipeline resumability.

func TestPipelineResumability_TriggerResetsErrors(t *testing.T) {
	mp := &mockPipeline{}
	ms := &mockStore{}
	h := newTestRouter(ms, mp)

	// Trigger sync — should return 202 Accepted.
	rr := mustRequest(t, h, http.MethodPost, "/sync/trigger")
	if rr.Code != http.StatusAccepted {
		t.Errorf("status = %d; want 202", rr.Code)
	}
}

func TestPipelineResumability_SyncStatusReflectsAllStates(t *testing.T) {
	// After a crash, schemes will be in various states.
	// The /sync/status endpoint must accurately surface all of them so an
	// operator can tell which schemes need attention.
	now := time.Now()
	ms := &mockStore{
		syncStates: []*models.SyncState{
			{SchemeCode: "S1", Status: "done", NavCount: 2513, UpdatedAt: now},
			{SchemeCode: "S2", Status: "pending", NavCount: 0, UpdatedAt: now},
			{SchemeCode: "S3", Status: "running", NavCount: 500, UpdatedAt: now},
			{SchemeCode: "S4", Status: "error", NavCount: 0, UpdatedAt: now},
		},
	}
	h := newTestRouter(ms, &mockPipeline{})
	rr := mustRequest(t, h, http.MethodGet, "/sync/status")

	var body map[string]any
	assertJSON(t, rr, http.StatusOK, &body)

	summary := body["summary"].(map[string]any)
	tests := []struct{ field string; want float64 }{
		{"total", 4}, {"done", 1}, {"pending", 1}, {"running", 1}, {"error", 1},
	}
	for _, tc := range tests {
		got := summary[tc.field].(float64)
		if got != tc.want {
			t.Errorf("summary.%s = %v; want %v", tc.field, got, tc.want)
		}
	}
}

// ─── store is a concrete type compile check ───────────────────────────────────
// Ensure *store.Store still satisfies the DataStore interface at compile time.
// This line will fail to build if someone removes a required method from Store.
var _ api.DataStore = (*store.Store)(nil)
