package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"mutualfundanalysis/internal/models"
)

// SeedSyncState inserts a 'pending' sync state for a scheme if not yet present.
func (s *Store) SeedSyncState(ctx context.Context, schemeCode string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO sync_state (scheme_code, status)
		VALUES ($1, 'pending')
		ON CONFLICT (scheme_code) DO NOTHING
	`, schemeCode)
	return err
}

// AcquireNextPendingJob atomically selects and locks the next scheme that needs syncing.
// Eligible: status='pending', status='error', or stale status='running' (>10 min old).
// Returns empty string if no jobs are available.
func (s *Store) AcquireNextPendingJob(ctx context.Context) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var code string
	err = tx.QueryRow(ctx, `
		SELECT scheme_code FROM sync_state
		WHERE status = 'pending'
		   OR status = 'error'
		   OR (status = 'running' AND updated_at < NOW() - INTERVAL '10 minutes')
		ORDER BY created_at ASC
		LIMIT 1
		FOR UPDATE SKIP LOCKED
	`).Scan(&code)
	if err == pgx.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("query next job: %w", err)
	}

	if _, err = tx.Exec(ctx, `
		UPDATE sync_state
		SET status = 'running', error_msg = NULL, updated_at = NOW()
		WHERE scheme_code = $1
	`, code); err != nil {
		return "", fmt.Errorf("mark running: %w", err)
	}

	return code, tx.Commit(ctx)
}

// MarkRunning sets a scheme's sync status to 'running'.
// Used by the daily scheduler (which doesn't go through AcquireNextPendingJob).
func (s *Store) MarkRunning(ctx context.Context, schemeCode string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE sync_state
		SET status = 'running', error_msg = NULL, updated_at = NOW()
		WHERE scheme_code = $1
	`, schemeCode)
	return err
}

// MarkError sets a scheme's status to 'error' with a descriptive message.
func (s *Store) MarkError(ctx context.Context, schemeCode, errMsg string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE sync_state
		SET status = 'error', error_msg = $2, updated_at = NOW()
		WHERE scheme_code = $1
	`, schemeCode, errMsg)
	return err
}

// MarkAnalyticsDone records that analytics were successfully computed for a scheme.
// Clears any previous analytics_error and stamps analytics_computed_at.
func (s *Store) MarkAnalyticsDone(ctx context.Context, schemeCode string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE sync_state
		SET analytics_status      = 'done',
		    analytics_error       = NULL,
		    analytics_computed_at = NOW(),
		    updated_at            = NOW()
		WHERE scheme_code = $1
	`, schemeCode)
	return err
}

// MarkAnalyticsError records that analytics computation failed for a scheme.
// The scheme will be retried automatically by the next RepairAnalytics pass.
func (s *Store) MarkAnalyticsError(ctx context.Context, schemeCode, errMsg string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE sync_state
		SET analytics_status = 'error',
		    analytics_error  = $2,
		    updated_at       = NOW()
		WHERE scheme_code = $1
	`, schemeCode, errMsg)
	return err
}

// GetSchemesNeedingAnalytics returns scheme codes where NAV data is fully
// synced (status = 'done') but analytics have not yet been successfully
// computed (analytics_status != 'done'). Used by RepairAnalytics.
func (s *Store) GetSchemesNeedingAnalytics(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT scheme_code FROM sync_state
		WHERE status = 'done' AND analytics_status != 'done'
		ORDER BY scheme_code
	`)
	if err != nil {
		return nil, fmt.Errorf("get schemes needing analytics: %w", err)
	}
	defer rows.Close()

	var codes []string
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			return nil, fmt.Errorf("scan scheme code: %w", err)
		}
		codes = append(codes, code)
	}
	return codes, rows.Err()
}

// ResetErrorsToPending resets all 'error' schemes back to 'pending'
// so they are retried on the next sync trigger.
func (s *Store) ResetErrorsToPending(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE sync_state
		SET status = 'pending', error_msg = NULL, updated_at = NOW()
		WHERE status = 'error'
	`)
	return err
}

// GetAllSyncStates returns the sync status of all tracked schemes.
func (s *Store) GetAllSyncStates(ctx context.Context) ([]*models.SyncState, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT scheme_code, status, last_nav_date, latest_nav,
		       nav_count, error_msg, created_at, updated_at
		FROM sync_state
		ORDER BY scheme_code
	`)
	if err != nil {
		return nil, fmt.Errorf("get sync states: %w", err)
	}
	defer rows.Close()

	var out []*models.SyncState
	for rows.Next() {
		st := &models.SyncState{}
		if err := rows.Scan(
			&st.SchemeCode, &st.Status, &st.LastNavDate, &st.LatestNAV,
			&st.NavCount, &st.ErrorMsg, &st.CreatedAt, &st.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan sync state: %w", err)
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// SaveSyncResult atomically:
//  1. Upserts scheme metadata
//  2. Bulk-inserts nav_data (idempotent, skips duplicates)
//  3. Updates sync_state to 'done' with checkpoint and latest NAV
//
// This is the critical transaction that must succeed or fully roll back.
func (s *Store) SaveSyncResult(
	ctx context.Context,
	scheme *models.Scheme,
	navRows []models.NAVRow,
) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// 1. Upsert scheme metadata (fills in name/amc/category from API response)
	if err := UpsertSchemeInTx(ctx, tx, scheme); err != nil {
		return fmt.Errorf("upsert scheme: %w", err)
	}

	// 2. Bulk insert nav_data via temp table + ON CONFLICT DO NOTHING
	inserted, err := bulkInsertNAV(ctx, tx, navRows)
	if err != nil {
		return fmt.Errorf("bulk insert nav: %w", err)
	}

	// 3. Determine checkpoint values from the fetched data
	var lastNavDate *time.Time
	var latestNAV *float64
	if len(navRows) > 0 {
		d := navRows[len(navRows)-1].NavDate // rows are sorted asc by the API (or we sort before passing)
		nav := navRows[len(navRows)-1].NAV
		lastNavDate = &d
		latestNAV = &nav
	}

	// 4. Update sync_state to 'done'
	if _, err := tx.Exec(ctx, `
		UPDATE sync_state
		SET status        = 'done',
		    last_nav_date = COALESCE($2, last_nav_date),
		    latest_nav    = COALESCE($3, latest_nav),
		    nav_count     = nav_count + $4,
		    error_msg     = NULL,
		    updated_at    = NOW()
		WHERE scheme_code = $1
	`, scheme.Code, lastNavDate, latestNAV, inserted); err != nil {
		return fmt.Errorf("update sync_state: %w", err)
	}

	return tx.Commit(ctx)
}

// UpsertSchemeInTx is the tx-scoped version of UpsertScheme, used internally.
func UpsertSchemeInTx(ctx context.Context, tx pgx.Tx, scheme *models.Scheme) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO schemes (code, name, amc, category, scheme_type, isin_growth, isin_div_reinvestment, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())
		ON CONFLICT (code) DO UPDATE SET
			name                  = EXCLUDED.name,
			amc                   = EXCLUDED.amc,
			category              = EXCLUDED.category,
			scheme_type           = EXCLUDED.scheme_type,
			isin_growth           = EXCLUDED.isin_growth,
			isin_div_reinvestment = EXCLUDED.isin_div_reinvestment,
			updated_at            = NOW()
	`, scheme.Code, scheme.Name, scheme.AMC, scheme.Category,
		scheme.SchemeType, scheme.ISINGrowth, scheme.ISINDivReinvestment)
	return err
}
