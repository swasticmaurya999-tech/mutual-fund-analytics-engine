package store

import (
	"context"

	"mutualfundanalysis/internal/models"
)

// LogRequest appends one outbound API call record to request_log.
// Failures here are non-fatal — we log a warning but never block a sync.
func (s *Store) LogRequest(ctx context.Context, req *models.RequestLog) {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO request_log
		    (scheme_code, endpoint, full_url, http_status, duration_ms, error_msg, retry_attempt)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, req.SchemeCode, req.Endpoint, req.FullURL,
		req.HTTPStatus, req.DurationMS, req.ErrorMsg, req.RetryAttempt)
	if err != nil {
		s.log.Warn("failed to write request log", "error", err)
	}
}
