package store

import (
	"context"
	"fmt"

	"mutualfundanalysis/internal/models"
)

// LoadAllRateLimiterStates reads the persisted token bucket state for all 3 limiters.
func (s *Store) LoadAllRateLimiterStates(ctx context.Context) ([]*models.RateLimiterState, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT limiter_id, tokens, capacity, refill_rate, last_updated
		FROM rate_limiter_state
		ORDER BY limiter_id
	`)
	if err != nil {
		return nil, fmt.Errorf("load rate limiter states: %w", err)
	}
	defer rows.Close()

	var out []*models.RateLimiterState
	for rows.Next() {
		st := &models.RateLimiterState{}
		if err := rows.Scan(&st.LimiterID, &st.Tokens, &st.Capacity, &st.RefillRate, &st.LastUpdated); err != nil {
			return nil, fmt.Errorf("scan rate limiter state: %w", err)
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// SaveRateLimiterState persists current token count for one limiter bucket.
func (s *Store) SaveRateLimiterState(ctx context.Context, limiterID string, tokens float64) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE rate_limiter_state
		SET tokens = $2, last_updated = NOW()
		WHERE limiter_id = $1
	`, limiterID, tokens)
	if err != nil {
		return fmt.Errorf("save rate limiter state %s: %w", limiterID, err)
	}
	return nil
}

// DrainAllRateLimiters sets all bucket token counts to 0.
// Called when a HTTP 429 is received to force a full cooldown.
func (s *Store) DrainAllRateLimiters(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE rate_limiter_state SET tokens = 0, last_updated = NOW()
	`)
	return err
}
