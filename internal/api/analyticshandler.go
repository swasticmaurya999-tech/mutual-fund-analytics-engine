package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"mutualfundanalysis/internal/models"
)

// GetAnalytics handles GET /funds/{code}/analytics?window=3Y
func (h *Handler) GetAnalytics(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code")
	window := r.URL.Query().Get("window")

	if code == "" {
		writeError(w, http.StatusBadRequest, "scheme code is required", "MISSING_CODE")
		return
	}
	if !models.ValidWindows[window] {
		writeError(w, http.StatusBadRequest, "window must be one of: 1Y, 3Y, 5Y, 10Y", "INVALID_WINDOW")
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

	a, err := h.store.GetAnalytics(r.Context(), code, window)
	if err != nil {
		h.log.Error("get analytics failed", "code", code, "window", window, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to fetch analytics", "DB_ERROR")
		return
	}
	if a == nil {
		writeError(w, http.StatusNotFound,
			"analytics not yet computed for this fund and window — sync may still be in progress",
			"ANALYTICS_NOT_READY",
		)
		return
	}

	writeJSON(w, http.StatusOK, toAnalyticsResponse(scheme, a))
}
