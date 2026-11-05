package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/burakkuru5534/case-study-dining/internal/platform/config"
	"github.com/burakkuru5534/case-study-dining/internal/platform/postgres"
)

const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 10 * time.Second
	writeTimeout      = 15 * time.Second
	idleTimeout       = 60 * time.Second
	shutdownTimeout   = 10 * time.Second
	readyTimeout      = 2 * time.Second
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("api exited with error", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// Created before connecting to the database so Ctrl+C also aborts a slow startup.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	// Deferred, so it runs after server.Shutdown below has drained in-flight requests.
	defer pool.Close()

	// Never log the URL itself: it contains credentials.
	connCfg := pool.Config().ConnConfig
	logger.Info("connected to database", "host", connCfg.Host, "port", connCfg.Port, "database", connCfg.Database)

	// Bind synchronously so a port conflict fails fast, before we log "listening".
	ln, err := net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(cfg.HTTPPort)))
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	server := &http.Server{
		Handler:           newMux(pool, logger),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("api server listening", "addr", ln.Addr().String(), "env", cfg.AppEnv)
		serverErr <- server.Serve(ln)
	}()

	select {
	case err := <-serverErr:
		// Serve returned before any shutdown was requested.
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	}

	// Restore default signal handling so a second Ctrl+C kills the process immediately.
	stop()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}

	// After a successful Shutdown, Serve has returned ErrServerClosed.
	if err := <-serverErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server: %w", err)
	}

	logger.Info("api server stopped")
	return nil
}

// pinger is the only thing the readiness check needs from the database.
// *pgxpool.Pool satisfies it; tests use a fake.
type pinger interface {
	Ping(ctx context.Context) error
}

func newMux(db pinger, logger *slog.Logger) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("GET /ready", handleReady(db, logger))
	return mux
}

// handleHealth is a liveness check: it never touches dependencies, so a
// database outage does not make the orchestrator restart every instance.
func handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, `{"status":"ok"}`)
}

// handleReady reports whether this instance can serve traffic that needs the database.
func handleReady(db pinger, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
		defer cancel()

		if err := db.Ping(ctx); err != nil {
			// The error can contain host/user details: log it, don't return it.
			logger.Warn("readiness check failed", "error", err)
			writeJSON(w, http.StatusServiceUnavailable, `{"status":"unavailable"}`)
			return
		}
		writeJSON(w, http.StatusOK, `{"status":"ready"}`)
	}
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}
