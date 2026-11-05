package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		method     string
		wantStatus int
		wantBody   string
	}{
		{name: "GET returns ok", method: http.MethodGet, wantStatus: http.StatusOK, wantBody: `{"status":"ok"}`},
		{name: "HEAD is served by GET route", method: http.MethodHead, wantStatus: http.StatusOK},
		{name: "POST is not allowed", method: http.MethodPost, wantStatus: http.StatusMethodNotAllowed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Go through the mux, not the handler directly, so routing and
			// method matching are covered too.
			req := httptest.NewRequest(tt.method, "/health", nil)
			rec := httptest.NewRecorder()

			newMux().ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status: got %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantStatus != http.StatusOK {
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
