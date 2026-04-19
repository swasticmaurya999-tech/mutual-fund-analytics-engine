package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"mutualfundanalysis/internal/models"
)

// ListFunds handles GET /funds
// Query params: ?category=, ?amc=
func (h *Handler) ListFunds(w http.ResponseWriter, r *http.Request) {
	category := r.URL.Query().Get("category")
	amc := r.URL.Query().Get("amc")

	// Validate category if provided — only the two tracked categories are valid.
	if category != "" {
		normalized, err := models.NormalizeCategory(category)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error(), "INVALID_CATEGORY")
			return
		}
		category = normalized
	}

	schemes, err := h.store.ListSchemes(r.Context(), category, amc)
	if err != nil {
		h.log.Error("list schemes failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list funds", "DB_ERROR")
		return
	}

	// Enrich each scheme with its current sync state
	states, err := h.store.GetAllSyncStates(r.Context())
	if err != nil {
		h.log.Warn("could not load sync states for list", "error", err)
	}
	stateMap := make(map[string]*syncStateShim, len(states))
	for _, ss := range states {
		stateMap[ss.SchemeCode] = &syncStateShim{Status: ss.Status, LatestNAV: ss.LatestNAV, LastNavDate: ss.LastNavDate}
	}

	funds := make([]*fundResponse, 0, len(schemes))
	for _, sc := range schemes {
		if sc.Name == "" {
			continue // scheme seeded but not yet synced
		}
		ss := stateMap[sc.Code]
		fund := toFundResponseShim(sc, ss)
		funds = append(funds, fund)
	}

	writeJSON(w, http.StatusOK, fundListResponse{Total: len(funds), Funds: funds})
}

// GetFund handles GET /funds/{code}
func (h *Handler) GetFund(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code")
	if code == "" {
		writeError(w, http.StatusBadRequest, "scheme code is required", "MISSING_CODE")
		return
	}

	scheme, err := h.store.GetScheme(r.Context(), code)
	if err != nil {
		h.log.Error("get scheme failed", "code", code, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to fetch fund", "DB_ERROR")
		return
	}
	if scheme == nil {
		writeError(w, http.StatusNotFound, "fund not found", "NOT_FOUND")
		return
	}

	states, err := h.store.GetAllSyncStates(r.Context())
	if err != nil {
		h.log.Warn("could not load sync state", "code", code, "error", err)
	}

	// Find matching sync state
	var matchedSS *syncStateShim
	for _, ss := range states {
		if ss.SchemeCode == code {
			matchedSS = &syncStateShim{
				Status:      ss.Status,
				LatestNAV:   ss.LatestNAV,
				LastNavDate: ss.LastNavDate,
			}
			break
		}
	}

	writeJSON(w, http.StatusOK, toFundResponseShim(scheme, matchedSS))
}
