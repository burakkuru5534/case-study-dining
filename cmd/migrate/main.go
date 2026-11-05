// Command migrate applies pending database migrations and exits.
// It is run as a one-shot step before the API starts.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/burakkuru5534/case-study-dining/internal/platform/config"
	"github.com/burakkuru5534/case-study-dining/internal/platform/migrate"
	"github.com/burakkuru5534/case-study-dining/migrations"
)

const (
	connectTimeout = 10 * time.Second
	// Upper bound for the whole run, including waiting for another runner's lock.
	runTimeout = 5 * time.Minute
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	if err := run(logger); err != nil {
		logger.Error("migrate failed", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	all, err := migrate.Load(migrations.FS)
	if err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()

	connectCtx, cancelConnect := context.WithTimeout(ctx, connectTimeout)
	defer cancelConnect()

	// A dedicated connection, not a pool: the advisory lock is tied to this session,
	// and closing it releases the lock even if the explicit unlock fails.
	conn, err := pgx.Connect(connectCtx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()

	applied, err := migrate.Run(ctx, conn, all, logger)
	if err != nil {
		return err
	}

	logger.Info("migrations complete", "applied", applied, "total", len(all))
	return nil
}
