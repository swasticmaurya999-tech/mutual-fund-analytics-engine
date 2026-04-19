package api

import (
	"context"
	"net/http"
)

// TriggerSync handles POST /sync/trigger
// Resets errored schemes and kicks off a full sync in the background.
func (h *Handler) TriggerSync(w http.ResponseWriter, r *http.Request) {
	// Reset any 'error' schemes to 'pending' so they are retried
	if err := h.store.ResetErrorsToPending(r.Context()); err != nil {
		h.log.Warn("failed to reset error schemes before trigger", "error", err)
	}

	// Run backfill in background so the HTTP response is immediate
	go func() {
		ctx := context.Background()
		h.log.Info("manual sync triggered")
		h.pipeline.RunBackfill(ctx)
		h.log.Info("manual sync complete")
	}()

	writeJSON(w, http.StatusAccepted, map[string]string{
		"status":  "accepted",
		"message": "sync triggered — check GET /sync/status for progress",
	})
}

// GetSyncStatus handles GET /sync/status
func (h *Handler) GetSyncStatus(w http.ResponseWriter, r *http.Request) {
	states, err := h.store.GetAllSyncStates(r.Context())
	if err != nil {
		h.log.Error("get sync states failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to fetch sync status", "DB_ERROR")
		return
	}

	resp := syncStatusResponse{}
	resp.Summary.Total = len(states)

	for _, ss := range states {
		item := &syncStatusItem{
			SchemeCode: ss.SchemeCode,
			Status:     ss.Status,
			LatestNAV:  ss.LatestNAV,
			NavCount:   ss.NavCount,
			ErrorMsg:   ss.ErrorMsg,
			UpdatedAt:  ss.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		}
		if ss.LastNavDate != nil {
			d := ss.LastNavDate.Format("2006-01-02")
			item.LastNavDate = &d
		}
		resp.Schemes = append(resp.Schemes, item)

		switch ss.Status {
		case "done":
			resp.Summary.Done++
		case "pending":
			resp.Summary.Pending++
		case "running":
			resp.Summary.Running++
		case "error":
			resp.Summary.Error++
		}
	}

	writeJSON(w, http.StatusOK, resp)
}
