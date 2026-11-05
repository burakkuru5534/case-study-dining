package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fakePinger returns err, or blocks until ctx is done when block is true.
type fakePinger struct {
	err   error
	block bool
}

func (f fakePinger) Ping(ctx context.Context) error {
	if f.block {
		<-ctx.Done()
		return ctx.Err()
	}
	return f.err
}

func TestRoutes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		db         fakePinger
		method     string
		path       string
		wantStatus int
		wantBody   string
	}{
		{name: "health GET returns ok", method: http.MethodGet, path: "/health", wantStatus: http.StatusOK, wantBody: `{"status":"ok"}`},
		{name: "health HEAD is served by GET route", method: http.MethodHead, path: "/health", wantStatus: http.StatusOK},
		{name: "health POST is not allowed", method: http.MethodPost, path: "/health", wantStatus: http.StatusMethodNotAllowed},
		{
			name:   "health ignores database state",
			db:     fakePinger{err: errors.New("db down")},
			method: http.MethodGet, path: "/health",
			wantStatus: http.StatusOK, wantBody: `{"status":"ok"}`,
		},
		{name: "ready when database is reachable", method: http.MethodGet, path: "/ready", wantStatus: http.StatusOK, wantBody: `{"status":"ready"}`},
		{
			name:   "not ready when ping fails and error is not leaked",
			db:     fakePinger{err: errors.New("dial tcp 10.0.0.5:5432: user=admin refused")},
			method: http.MethodGet, path: "/ready",
			wantStatus: http.StatusServiceUnavailable, wantBody: `{"status":"unavailable"}`,
		},
		{name: "ready POST is not allowed", method: http.MethodPost, path: "/ready", wantStatus: http.StatusMethodNotAllowed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Go through the mux, not the handler directly, so routing and
			// method matching are covered too.
			req := httptest.NewRequest(tt.method, tt.path, nil)
			rec := httptest.NewRecorder()

			newMux(tt.db, slog.New(slog.DiscardHandler)).ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status: got %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantStatus == http.StatusMethodNotAllowed {
				return
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type: got %q, want application/json", ct)
			}
			if tt.wantBody != "" && rec.Body.String() != tt.wantBody {
				t.Errorf("body: got %q, want %q", rec.Body.String(), tt.wantBody)
			}
		})
	}
}

func TestReadyIsBoundedByTimeout(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	rec := httptest.NewRecorder()

	start := time.Now()
	newMux(fakePinger{block: true}, slog.New(slog.DiscardHandler)).ServeHTTP(rec, req)
	elapsed := time.Since(start)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if elapsed < readyTimeout || elapsed > readyTimeout+time.Second {
		t.Errorf("took %s, expected about %s", elapsed, readyTimeout)
	}
}
