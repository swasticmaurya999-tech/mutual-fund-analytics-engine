package api

import (
	"time"

	"mutualfundanalysis/internal/models"
)

// syncStateShim is a lightweight struct used internally by API handlers
// to avoid importing models.SyncState everywhere.
type syncStateShim struct {
	Status      string
	LatestNAV   *float64
	LastNavDate *time.Time
}

// toFundResponseShim builds a fundResponse from a Scheme and optional sync state.
func toFundResponseShim(sc *models.Scheme, ss *syncStateShim) *fundResponse {
	f := &fundResponse{
		Code:       sc.Code,
		Name:       sc.Name,
		AMC:        sc.AMC,
		Category:   sc.Category,
		SchemeType: sc.SchemeType,
		ISINGrowth: sc.ISINGrowth,
		SyncStatus: "unknown",
	}
	if ss != nil {
		f.SyncStatus = ss.Status
		f.CurrentNAV = ss.LatestNAV
		if ss.LastNavDate != nil {
			d := ss.LastNavDate.Format("2006-01-02")
			f.LastUpdated = &d
		}
	}
	return f
}
