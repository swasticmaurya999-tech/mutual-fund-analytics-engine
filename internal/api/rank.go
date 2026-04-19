package api

import (
	"net/http"
	"strconv"

	"mutualfundanalysis/internal/models"
)

// RankFunds handles GET /funds/rank
// Required: ?category=, ?window=
// Optional: ?sort_by=median_return|max_drawdown, ?limit=5
func (h *Handler) RankFunds(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	category := q.Get("category")
	window := q.Get("window")
	sortBy := q.Get("sort_by")
	limitStr := q.Get("limit")

	// Validate required params
	if category == "" {
		writeError(w, http.StatusBadRequest,
			"category is required — only two categories are tracked: \"Equity Scheme - Mid Cap Fund\" and \"Equity Scheme - Small Cap Fund\"",
			"MISSING_CATEGORY",
		)
		return
	}
	normalized, catErr := models.NormalizeCategory(category)
	if catErr != nil {
		writeError(w, http.StatusBadRequest, catErr.Error(), "INVALID_CATEGORY")
		return
	}
	category = normalized

	if !models.ValidWindows[window] {
		writeError(w, http.StatusBadRequest, "window must be one of: 1Y, 3Y, 5Y, 10Y", "INVALID_WINDOW")
		return
	}

	// Validate optional sort_by
	if sortBy == "" {
		sortBy = "median_return"
	}
	if sortBy != "median_return" && sortBy != "max_drawdown" {
		writeError(w, http.StatusBadRequest, "sort_by must be median_return or max_drawdown", "INVALID_SORT_BY")
		return
	}

	// Validate optional limit (default 5, max 50)
	limit := 5
	if limitStr != "" {
		var err error
		limit, err = strconv.Atoi(limitStr)
		if err != nil || limit < 1 || limit > 50 {
			writeError(w, http.StatusBadRequest, "limit must be an integer between 1 and 50", "INVALID_LIMIT")
			return
		}
	}

	rows, totalFunds, err := h.store.GetRanking(r.Context(), category, window, sortBy, limit)
	if err != nil {
		h.log.Error("get ranking failed", "category", category, "window", window, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to fetch ranking", "DB_ERROR")
		return
	}

	// Build ranked list
	funds := make([]*rankFundItem, 0, len(rows))
	for i, row := range rows {
		funds = append(funds, toRankItem(i+1, row))
	}

	resp := &rankingResponse{
		Category:   category,
		Window:     window,
		SortedBy:   sortBy,
		TotalFunds: totalFunds, // pre-limit count — matches all funds in category+window
		Showing:    len(funds), // how many are actually returned (≤ limit)
		Funds:      funds,
	}

	writeJSON(w, http.StatusOK, resp)
}
