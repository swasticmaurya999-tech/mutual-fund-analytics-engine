package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"mutualfundanalysis/internal/models"
)

// SeedScheme inserts a scheme code placeholder if it doesn't exist yet.
// Metadata (name, amc, category) is populated on the first real sync.
func (s *Store) SeedScheme(ctx context.Context, code string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO schemes (code)
		VALUES ($1)
		ON CONFLICT (code) DO NOTHING
	`, code)
	return err
}

// GetScheme fetches full metadata for a scheme by code.
// Returns nil, nil if not found.
func (s *Store) GetScheme(ctx context.Context, code string) (*models.Scheme, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT code, COALESCE(name,''), COALESCE(amc,''), COALESCE(category,''),
		       COALESCE(scheme_type,''), isin_growth, isin_div_reinvestment,
		       created_at, updated_at
		FROM schemes WHERE code = $1
	`, code)

	sc := &models.Scheme{}
	err := row.Scan(
		&sc.Code, &sc.Name, &sc.AMC, &sc.Category, &sc.SchemeType,
		&sc.ISINGrowth, &sc.ISINDivReinvestment,
		&sc.CreatedAt, &sc.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get scheme %s: %w", code, err)
	}
	return sc, nil
}

// ListSchemes returns all tracked schemes. Optionally filter by category and/or amc.
// Empty filter string means "no filter on that field".
func (s *Store) ListSchemes(ctx context.Context, category, amc string) ([]*models.Scheme, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT code, COALESCE(name,''), COALESCE(amc,''), COALESCE(category,''),
		       COALESCE(scheme_type,''), isin_growth, isin_div_reinvestment,
		       created_at, updated_at
		FROM schemes
		WHERE ($1 = '' OR category ILIKE $1)
		  AND ($2 = '' OR amc ILIKE '%' || $2 || '%')
		ORDER BY amc, name
	`, category, amc)
	if err != nil {
		return nil, fmt.Errorf("list schemes: %w", err)
	}
	defer rows.Close()

	var out []*models.Scheme
	for rows.Next() {
		sc := &models.Scheme{}
		if err := rows.Scan(
			&sc.Code, &sc.Name, &sc.AMC, &sc.Category, &sc.SchemeType,
			&sc.ISINGrowth, &sc.ISINDivReinvestment,
			&sc.CreatedAt, &sc.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan scheme: %w", err)
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}
