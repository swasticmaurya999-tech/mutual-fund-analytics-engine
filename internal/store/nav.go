package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"mutualfundanalysis/internal/models"
)

// GetMaxNavDate returns the latest nav_date stored for a scheme.
// Returns nil if no data exists yet — used as the backfill checkpoint.
func (s *Store) GetMaxNavDate(ctx context.Context, schemeCode string) (*time.Time, error) {
	var d *time.Time
	err := s.pool.QueryRow(ctx,
		`SELECT MAX(nav_date) FROM nav_data WHERE scheme_code = $1`,
		schemeCode,
	).Scan(&d)
	if err != nil {
		return nil, fmt.Errorf("get max nav date for %s: %w", schemeCode, err)
	}
	return d, nil
}

// GetNAVHistory returns all NAV rows for a scheme sorted by date ascending.
// Used by the analytics engine to compute rolling metrics.
func (s *Store) GetNAVHistory(ctx context.Context, schemeCode string) ([]models.NAVRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT scheme_code, nav_date, nav::float8
		FROM nav_data
		WHERE scheme_code = $1
		ORDER BY nav_date ASC
	`, schemeCode)
	if err != nil {
		return nil, fmt.Errorf("get nav history for %s: %w", schemeCode, err)
	}
	defer rows.Close()

	var out []models.NAVRow
	for rows.Next() {
		var r models.NAVRow
		if err := rows.Scan(&r.SchemeCode, &r.NavDate, &r.NAV); err != nil {
			return nil, fmt.Errorf("scan nav row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// bulkInsertNAV copies nav rows into a temp table then inserts with conflict resolution.
// Must be called within an active transaction.
func bulkInsertNAV(ctx context.Context, tx pgx.Tx, navRows []models.NAVRow) (int64, error) {
	if len(navRows) == 0 {
		return 0, nil
	}

	// Staging table: dropped automatically when the transaction ends (ON COMMIT DROP)
	if _, err := tx.Exec(ctx, `
		CREATE TEMP TABLE tmp_nav_insert (
			scheme_code TEXT,
			nav_date    DATE,
			nav         NUMERIC(20,5)
		) ON COMMIT DROP
	`); err != nil {
		return 0, fmt.Errorf("create temp table: %w", err)
	}

	// COPY into staging (fast, no constraint checks)
	rows := make([][]any, len(navRows))
	for i, r := range navRows {
		rows[i] = []any{r.SchemeCode, r.NavDate, r.NAV}
	}
	if _, err := tx.CopyFrom(
		ctx,
		pgx.Identifier{"tmp_nav_insert"},
		[]string{"scheme_code", "nav_date", "nav"},
		pgx.CopyFromRows(rows),
	); err != nil {
		return 0, fmt.Errorf("copy nav data: %w", err)
	}

	// Merge into real table; skip rows that already exist (idempotent)
	result, err := tx.Exec(ctx, `
		INSERT INTO nav_data (scheme_code, nav_date, nav)
		SELECT scheme_code, nav_date, nav FROM tmp_nav_insert
		ON CONFLICT (scheme_code, nav_date) DO NOTHING
	`)
	if err != nil {
		return 0, fmt.Errorf("insert nav data: %w", err)
	}
	return result.RowsAffected(), nil
}
