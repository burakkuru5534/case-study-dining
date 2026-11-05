// Package postgres creates the PostgreSQL connection pool used by the application.
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// pingTimeout bounds the startup connectivity check so a wrong host or a
// blackholed network fails fast instead of hanging the process.
const pingTimeout = 5 * time.Second

// NewPool parses databaseURL, creates a pool and verifies connectivity.
//
// Pool sizing uses pgx defaults and can be tuned through URL parameters
// such as pool_max_conns, without code changes.
func NewPool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}

	// NewWithConfig does not connect eagerly; Ping below forces the first connection.
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()

	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	return pool, nil
}
