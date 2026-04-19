package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"mutualfundanalysis/internal/models"
)

// UpsertAnalytics inserts or overwrites pre-computed analytics for a scheme+window.
func (s *Store) UpsertAnalytics(ctx context.Context, a *models.Analytics) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO analytics (
			scheme_code, "window",
			rolling_min, rolling_max, rolling_median, rolling_p25, rolling_p75, rolling_volatility,
			max_drawdown,
			cagr_min, cagr_max, cagr_median,
			data_start, data_end, total_days, nav_data_points,
			rolling_periods_analyzed, insufficient_data, computed_at
		) VALUES (
			$1,  $2,
			$3,  $4,  $5,  $6,  $7,  $8,
			$9,
			$10, $11, $12,
			$13, $14, $15, $16,
			$17, $18, NOW()
		)
		ON CONFLICT (scheme_code, "window") DO UPDATE SET
			rolling_min              = EXCLUDED.rolling_min,
			rolling_max              = EXCLUDED.rolling_max,
			rolling_median           = EXCLUDED.rolling_median,
			rolling_p25              = EXCLUDED.rolling_p25,
			rolling_p75              = EXCLUDED.rolling_p75,
			rolling_volatility       = EXCLUDED.rolling_volatility,
			max_drawdown             = EXCLUDED.max_drawdown,
			cagr_min                 = EXCLUDED.cagr_min,
			cagr_max                 = EXCLUDED.cagr_max,
			cagr_median              = EXCLUDED.cagr_median,
			data_start               = EXCLUDED.data_start,
			data_end                 = EXCLUDED.data_end,
			total_days               = EXCLUDED.total_days,
			nav_data_points          = EXCLUDED.nav_data_points,
			rolling_periods_analyzed = EXCLUDED.rolling_periods_analyzed,
			insufficient_data        = EXCLUDED.insufficient_data,
			computed_at              = NOW()
	`,
		a.SchemeCode, a.Window,
		a.RollingMin, a.RollingMax, a.RollingMedian, a.RollingP25, a.RollingP75, a.RollingVolatility,
		a.MaxDrawdown,
		a.CAGRMin, a.CAGRMax, a.CAGRMedian,
		a.DataStart, a.DataEnd, a.TotalDays, a.NavDataPoints,
		a.RollingPeriodsAnalyzed, a.InsufficientData,
	)
	if err != nil {
		return fmt.Errorf("upsert analytics (%s/%s): %w", a.SchemeCode, a.Window, err)
	}
	return nil
}

// GetAnalytics fetches pre-computed analytics for a specific scheme+window.
// Returns nil, nil if not found.
func (s *Store) GetAnalytics(ctx context.Context, schemeCode, window string) (*models.Analytics, error) {
	a := &models.Analytics{}
	err := s.pool.QueryRow(ctx, `
		SELECT scheme_code, "window",
		       rolling_min, rolling_max, rolling_median, rolling_p25, rolling_p75, rolling_volatility,
		       max_drawdown,
		       cagr_min, cagr_max, cagr_median,
		       data_start, data_end, total_days, nav_data_points,
		       rolling_periods_analyzed, insufficient_data, computed_at
		FROM analytics
		WHERE scheme_code = $1 AND "window" = $2
	`, schemeCode, window).Scan(
		&a.SchemeCode, &a.Window,
		&a.RollingMin, &a.RollingMax, &a.RollingMedian, &a.RollingP25, &a.RollingP75, &a.RollingVolatility,
		&a.MaxDrawdown,
		&a.CAGRMin, &a.CAGRMax, &a.CAGRMedian,
		&a.DataStart, &a.DataEnd, &a.TotalDays, &a.NavDataPoints,
		&a.RollingPeriodsAnalyzed, &a.InsufficientData, &a.ComputedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get analytics (%s/%s): %w", schemeCode, window, err)
	}
	return a, nil
}

// GetRanking returns ranked funds for a given window and category, along with
// the total number of matching funds before the limit is applied.
//
// total reflects all funds that match the window+category filter; out contains
// at most limit of them, ordered by sortBy. This matches the assignment's
// example where total_funds (28) and showing (10) can differ.
//
// sortBy: "median_return" (default) or "max_drawdown"
func (s *Store) GetRanking(ctx context.Context, category, window, sortBy string, limit int) ([]*models.RankRow, int, error) {
	orderClause := `a.rolling_median DESC NULLS LAST`
	if sortBy == "max_drawdown" {
		// Higher (less negative) drawdown = better fund = higher rank
		orderClause = `a.max_drawdown DESC NULLS LAST`
	}

	// COUNT(*) OVER() is a window function evaluated before LIMIT, so it
	// returns the total number of matching rows regardless of the LIMIT value.
	// This avoids a second round-trip to the database.
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
		SELECT
			a.scheme_code,
			COALESCE(s.name, ''),
			COALESCE(s.amc, ''),
			COALESCE(s.category, ''),
			a."window",
			a.rolling_median,
			a.max_drawdown,
			ss.latest_nav,
			ss.last_nav_date,
			a.insufficient_data,
			COUNT(*) OVER() AS total_count
		FROM analytics a
		JOIN schemes    s  ON s.code         = a.scheme_code
		JOIN sync_state ss ON ss.scheme_code  = a.scheme_code
		WHERE a."window" = $1
		  AND s.category ILIKE $2
		ORDER BY %s
		LIMIT $3
	`, orderClause), window, category, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("get ranking: %w", err)
	}
	defer rows.Close()

	var out []*models.RankRow
	total := 0
	for rows.Next() {
		r := &models.RankRow{}
		if err := rows.Scan(
			&r.SchemeCode, &r.SchemeName, &r.AMC, &r.Category,
			&r.Window, &r.RollingMedian, &r.MaxDrawdown,
			&r.LatestNAV, &r.LastNavDate, &r.InsufficientData,
			&total,
		); err != nil {
			return nil, 0, fmt.Errorf("scan rank row: %w", err)
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}
