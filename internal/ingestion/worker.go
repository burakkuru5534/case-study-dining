package ingestion

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// EventProcessor is what the worker drives. *Processor implements it.
type EventProcessor interface {
	ProcessNext(ctx context.Context) (claimed bool, err error)
}

// WorkerConfig tunes the polling loop. Zero values use defaults.
type WorkerConfig struct {
	Concurrency  int           // parallel loops; default 2
	PollInterval time.Duration // wait when the queue is empty; default 1s
	EventTimeout time.Duration // upper bound for one event, also during shutdown; default 30s
	MaxErrorWait time.Duration // cap for the wait after consecutive errors; default 30s
}

// Worker runs Concurrency loops that drain the inbox.
//
// Shutdown: when the Run context is cancelled, loops stop claiming new events,
// but an event already being processed runs to completion (bounded by
// EventTimeout), because its context is deliberately not derived from Run's.
type Worker struct {
	proc   EventProcessor
	cfg    WorkerConfig
	logger *slog.Logger
}

func NewWorker(proc EventProcessor, cfg WorkerConfig, logger *slog.Logger) *Worker {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 2
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = time.Second
	}
	if cfg.EventTimeout <= 0 {
		cfg.EventTimeout = 30 * time.Second
	}
	if cfg.MaxErrorWait <= 0 {
		cfg.MaxErrorWait = 30 * time.Second
	}
	return &Worker{proc: proc, cfg: cfg, logger: logger}
}

// Run blocks until ctx is cancelled and every loop has finished its current event.
func (w *Worker) Run(ctx context.Context) {
	w.logger.Info("worker started", "concurrency", w.cfg.Concurrency, "poll_interval", w.cfg.PollInterval.String())
	var wg sync.WaitGroup
	for i := range w.cfg.Concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.loop(ctx, w.logger.With("loop", i))
		}()
	}
	wg.Wait()
	w.logger.Info("worker stopped")
}

func (w *Worker) loop(ctx context.Context, log *slog.Logger) {
	errorWait := w.cfg.PollInterval
	for ctx.Err() == nil {
		claimed, err := w.processOne()

		switch {
		case err != nil:
			// Infrastructure trouble (e.g. database down). The event, if any,
			// was left untouched. Back off so a dead database is not hammered.
			log.Error("processing loop error", "error", err, "retry_in", errorWait.String())
			sleep(ctx, errorWait)
			errorWait = min(errorWait*2, w.cfg.MaxErrorWait)
		case claimed:
			errorWait = w.cfg.PollInterval
			// More work is likely queued: take the next event immediately.
		default:
			errorWait = w.cfg.PollInterval
			sleep(ctx, w.cfg.PollInterval)
		}
	}
}

// processOne runs one ProcessNext with its own bounded context, independent
// of shutdown, so an in-flight event is finished rather than abandoned.
func (w *Worker) processOne() (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), w.cfg.EventTimeout)
	defer cancel()
	claimed, err := w.proc.ProcessNext(ctx)
	if errors.Is(err, context.DeadlineExceeded) {
		// The event was left untouched and will be claimed again.
		return claimed, errors.New("event processing exceeded EventTimeout")
	}
	return claimed, err
}

// sleep waits for d or until ctx is cancelled.
func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
