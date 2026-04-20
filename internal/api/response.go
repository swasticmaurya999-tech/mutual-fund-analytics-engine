package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"mutualfundanalysis/internal/models"
)

// errorResponse is the standard JSON error envelope.
type errorResponse struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

// fundResponse is the item shape in GET /funds list.
type fundResponse struct {
	Code        string   `json:"fund_code"`
	Name        string   `json:"fund_name"`
	AMC         string   `json:"amc"`
	Category    string   `json:"category"`
	SchemeType  string   `json:"scheme_type"`
	ISINGrowth  *string  `json:"isin_growth,omitempty"`
	SyncStatus  string   `json:"sync_status"`
	CurrentNAV  *float64 `json:"current_nav,omitempty"`
	LastUpdated *string  `json:"last_updated,omitempty"`
}

// fundListResponse is the GET /funds response envelope.
type fundListResponse struct {
	Total int             `json:"total"`
	Funds []*fundResponse `json:"funds"`
}

// dataAvailability mirrors the assignment's example analytics response block.
type dataAvailability struct {
	StartDate     string `json:"start_date"`
	EndDate       string `json:"end_date"`
	TotalDays     int    `json:"total_days"`
	NavDataPoints int    `json:"nav_data_points"`
}

// returnStats holds rolling return distribution.
type returnStats struct {
	Min    *float64 `json:"min"`
	Max    *float64 `json:"max"`
	Median *float64 `json:"median"`
	P25    *float64 `json:"p25"`
	P75    *float64 `json:"p75"`
}

// cagrStats holds CAGR distribution.
type cagrStats struct {
	Min    *float64 `json:"min"`
	Max    *float64 `json:"max"`
	Median *float64 `json:"median"`
}

// analyticsResponse mirrors the assignment's example analytics JSON exactly.
type analyticsResponse struct {
	FundCode               string           `json:"fund_code"`
	FundName               string           `json:"fund_name"`
	Category               string           `json:"category"`
	AMC                    string           `json:"amc"`
	Window                 string           `json:"window"`
	DataAvailability       dataAvailability `json:"data_availability"`
	RollingPeriodsAnalyzed int              `json:"rolling_periods_analyzed"`
	RollingReturns         returnStats      `json:"rolling_returns"`
	MaxDrawdown            *float64         `json:"max_drawdown"`
	CAGR                   cagrStats        `json:"cagr"`
	Volatility             *float64         `json:"volatility,omitempty"`
	InsufficientData       bool             `json:"insufficient_data,omitempty"`
	// InsufficientDataReason is only present when InsufficientData is true.
	// It explains exactly why rolling metrics are null and what windows are usable.
	InsufficientDataReason string           `json:"insufficient_data_reason,omitempty"`
	ComputedAt             string           `json:"computed_at"`
}

// rankFundItem is one entry in the GET /funds/rank response.
type rankFundItem struct {
	Rank        int      `json:"rank"`
	FundCode    string   `json:"fund_code"`
	FundName    string   `json:"fund_name"`
	AMC         string   `json:"amc"`
	MedianReturn *float64 `json:"median_return"`
	MaxDrawdown *float64 `json:"max_drawdown"`
	CurrentNAV  *float64 `json:"current_nav"`
	LastUpdated *string  `json:"last_updated"`
}

// rankingResponse is the GET /funds/rank response envelope.
type rankingResponse struct {
	Category   string          `json:"category"`
	Window     string          `json:"window"`
	SortedBy   string          `json:"sorted_by"`
	TotalFunds int             `json:"total_funds"`
	Showing    int             `json:"showing"`
	Funds      []*rankFundItem `json:"funds"`
}

// syncStatusItem is one scheme entry in GET /sync/status.
type syncStatusItem struct {
	SchemeCode  string   `json:"scheme_code"`
	Status      string   `json:"status"`
	LastNavDate *string  `json:"last_nav_date,omitempty"`
	LatestNAV   *float64 `json:"latest_nav,omitempty"`
	NavCount    int      `json:"nav_count"`
	ErrorMsg    *string  `json:"error_msg,omitempty"`
	UpdatedAt   string   `json:"updated_at"`
}

// syncStatusResponse is the GET /sync/status response.
type syncStatusResponse struct {
	Schemes []*syncStatusItem `json:"schemes"`
	Summary struct {
		Total   int `json:"total"`
		Done    int `json:"done"`
		Pending int `json:"pending"`
		Running int `json:"running"`
		Error   int `json:"error"`
	} `json:"summary"`
}

func toAnalyticsResponse(sc *models.Scheme, a *models.Analytics) *analyticsResponse {
	r := &analyticsResponse{
		FundCode:   sc.Code,
		FundName:   sc.Name,
		Category:   sc.Category,
		AMC:        sc.AMC,
		Window:     a.Window,
		MaxDrawdown: a.MaxDrawdown,
		Volatility: a.RollingVolatility,
		InsufficientData: a.InsufficientData,
		ComputedAt: a.ComputedAt.UTC().Format(time.RFC3339),
	}

	if a.DataStart != nil {
		r.DataAvailability.StartDate = a.DataStart.Format("2006-01-02")
	}
	if a.DataEnd != nil {
		r.DataAvailability.EndDate = a.DataEnd.Format("2006-01-02")
	}
	if a.TotalDays != nil {
		r.DataAvailability.TotalDays = *a.TotalDays
	}
	if a.NavDataPoints != nil {
		r.DataAvailability.NavDataPoints = *a.NavDataPoints
	}
	if a.RollingPeriodsAnalyzed != nil {
		r.RollingPeriodsAnalyzed = *a.RollingPeriodsAnalyzed
	}

	r.RollingReturns = returnStats{
		Min: a.RollingMin, Max: a.RollingMax, Median: a.RollingMedian,
		P25: a.RollingP25, P75: a.RollingP75,
	}
	r.CAGR = cagrStats{Min: a.CAGRMin, Max: a.CAGRMax, Median: a.CAGRMedian}

	if a.InsufficientData {
		needed := models.WindowTradingDays[a.Window]
		have := 0
		if a.NavDataPoints != nil {
			have = *a.NavDataPoints
		}
		since := ""
		if a.DataStart != nil {
			since = a.DataStart.Format("2006-01-02")
		}

		// Tell the caller exactly what's missing and which windows are usable.
		available := availableWindows(have)
		r.InsufficientDataReason = fmt.Sprintf(
			"Not enough NAV history for a %s rolling window. "+
				"Need at least %d trading days of data; this fund has %d (available since %s). "+
				"Rolling metrics (returns, CAGR) are unavailable for this window. "+
				"Max drawdown is still computed over the full history. "+
				"Try one of the available windows: %s.",
			a.Window, needed, have, since, available,
		)
	}

	return r
}

// availableWindows returns a human-readable list of windows for which
// the given number of NAV data points is sufficient.
func availableWindows(navPoints int) string {
	var windows []string
	for _, w := range []string{"1Y", "3Y", "5Y", "10Y"} {
		if navPoints >= models.WindowTradingDays[w] {
			windows = append(windows, w)
		}
	}
	if len(windows) == 0 {
		return "none (fund has very limited history)"
	}
	result := ""
	for i, w := range windows {
		if i > 0 {
			result += ", "
		}
		result += w
	}
	return result
}

func toRankItem(rank int, r *models.RankRow) *rankFundItem {
	item := &rankFundItem{
		Rank:         rank,
		FundCode:     r.SchemeCode,
		FundName:     r.SchemeName,
		AMC:          r.AMC,
		MedianReturn: r.RollingMedian,
		MaxDrawdown:  r.MaxDrawdown,
		CurrentNAV:   r.LatestNAV,
	}
	if r.LastNavDate != nil {
		d := r.LastNavDate.Format("2006-01-02")
		item.LastUpdated = &d
	}
	return item
}


func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg, code string) {
	writeJSON(w, status, errorResponse{Error: msg, Code: code})
}
