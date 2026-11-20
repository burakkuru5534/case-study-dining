// Command worker processes pending webhook events from the inbox: it creates
// orders and awards loyalty points. Several instances can run at once;
// FOR UPDATE SKIP LOCKED gives each event to exactly one of them.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/burakkuru5534/case-study-dining/internal/ingestion"
	"github.com/burakkuru5534/case-study-dining/internal/ingestion/reference"
	"github.com/burakkuru5534/case-study-dining/internal/platform/config"
	"github.com/burakkuru5534/case-study-dining/internal/platform/postgres"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("worker exited with error", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.LoadWorker()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	// Closed after the worker has finished its in-flight events.
	defer pool.Close()

	connCfg := pool.Config().ConnConfig
	logger.Info("connected to database", "host", connCfg.Host, "port", connCfg.Port, "database", connCfg.Database)

	// Each loop holds one connection while processing. More loops than
	// connections just queue for the pool; raise pool_max_conns in the URL.
	if maxConns := int(pool.Config().MaxConns); cfg.Concurrency > maxConns {
		logger.Warn("WORKER_CONCURRENCY exceeds pool size; extra loops will wait for connections",
			"concurrency", cfg.Concurrency, "pool_max_conns", maxConns)
	}

	processor := ingestion.NewProcessor(pool,
		map[string]ingestion.Adapter{reference.ProviderName: reference.Adapter{}},
		ingestion.ProcessorConfig{MaxAttempts: cfg.MaxAttempts},
		logger)

	ingestion.NewWorker(processor, ingestion.WorkerConfig{
		Concurrency:  cfg.Concurrency,
		PollInterval: cfg.PollInterval,
	}, logger).Run(ctx)

	return nil
}
