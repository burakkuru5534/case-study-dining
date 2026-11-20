package httpauth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	current  = "current-read-token-0123456789abcdef01"
	previous = "previous-read-token-0123456789abcdef0"
)

func TestRequireBearer(t *testing.T) {
	t.Parallel()

	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	h := RequireBearer([]string{current, previous}, ok)

	tests := []struct {
		name   string
		header string
		want   int
	}{
		{"current token", "Bearer " + current, http.StatusTeapot},
		{"previous token during rotation", "Bearer " + previous, http.StatusTeapot},
		{"scheme is case-insensitive", "bearer " + current, http.StatusTeapot},
		{"missing header", "", http.StatusUnauthorized},
		{"wrong token", "Bearer wrong-token-0123456789abcdef0123456", http.StatusUnauthorized},
		{"token prefix only", "Bearer " + current[:10], http.StatusUnauthorized},
		{"token with extra suffix", "Bearer " + current + "x", http.StatusUnauthorized},
		{"basic scheme", "Basic " + current, http.StatusUnauthorized},
		{"no scheme", current, http.StatusUnauthorized},
		{"empty token", "Bearer ", http.StatusUnauthorized},
		{"two spaces", "Bearer  " + current, http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d", rec.Code, tt.want)
			}
			if tt.want == http.StatusUnauthorized {
				if rec.Header().Get("WWW-Authenticate") == "" {
					t.Error("missing WWW-Authenticate header")
				}
				body := rec.Body.String()
				if strings.Contains(body, current) || strings.Contains(body, previous) {
					t.Fatalf("response leaks a token: %s", body)
				}
			}
		})
	}
}

func TestRequireBearerWithNoTokensRejectsEverything(t *testing.T) {
	t.Parallel()
	h := RequireBearer(nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer anything")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d: misconfiguration must fail closed", rec.Code)
	}
}
