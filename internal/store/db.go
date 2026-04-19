package store

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the single data-access object for all DB operations.
// All methods are safe for concurrent use.
type Store struct {
	pool *pgxpool.Pool
	log  *slog.Logger
}

// New creates a Store wrapping the given connection pool.
func New(pool *pgxpool.Pool, log *slog.Logger) *Store {
	return &Store{pool: pool, log: log}
}

// NewPool opens and validates a pgx connection pool.
func NewPool(ctx context.Context, dsn string, log *slog.Logger) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}

	cfg.MaxConns = 5
	cfg.MinConns = 1

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}

	log.Info("database connected", "max_conns", cfg.MaxConns)
	return pool, nil
}

// Close gracefully shuts down the connection pool.
func (s *Store) Close() {
	s.pool.Close()
}
