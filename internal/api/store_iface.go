package api

import (
	"context"

	"mutualfundanalysis/internal/models"
)

// DataStore is the minimal interface over store.Store that HTTP handlers need.
// Defining it here (rather than in the store package) follows the Go idiom of
// declaring interfaces at the point of use and keeps the store package free of
// any HTTP concerns.
//
// The concrete *store.Store satisfies this interface automatically; a mock
// implementation is used during unit tests.
type DataStore interface {
	ListSchemes(ctx context.Context, category, amc string) ([]*models.Scheme, error)
	GetScheme(ctx context.Context, code string) (*models.Scheme, error)
	GetAllSyncStates(ctx context.Context) ([]*models.SyncState, error)
	GetAnalytics(ctx context.Context, code, window string) (*models.Analytics, error)
	GetRanking(ctx context.Context, category, window, sortBy string, limit int) ([]*models.RankRow, int, error)
	ResetErrorsToPending(ctx context.Context) error
}

// PipelineRunner is the minimal interface over ingestion.Pipeline that HTTP
// handlers need. Separating this from the concrete type lets the sync handler
// be tested with a lightweight stub.
type PipelineRunner interface {
	RunBackfill(ctx context.Context)
}
