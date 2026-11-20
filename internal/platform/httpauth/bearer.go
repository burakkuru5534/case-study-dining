// Package httpauth provides a minimal static bearer-token check for internal
// read APIs. It is a placeholder for real client authentication (OAuth / mTLS
// with per-tenant authorization): every valid token can read every tenant.
package httpauth

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// RequireBearer rejects requests without a valid "Authorization: Bearer <token>"
// header. Any of tokens is accepted, which allows rotation without downtime.
// Comparison is constant time per token, and token values never reach the
// response or the caller's logs.
func RequireBearer(tokens []string, next http.Handler) http.Handler {
	accepted := make([][]byte, len(tokens))
	for i, t := range tokens {
		accepted[i] = []byte(t)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !valid(accepted, r.Header.Get("Authorization")) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="api"`)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}` + "\n"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func valid(accepted [][]byte, header string) bool {
	scheme, token, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return false
	}
	got := []byte(token)
	match := 0
	// Check every token (no early return), so timing does not reveal which
	// configured token is closest or how many there are.
	for _, a := range accepted {
		match |= subtle.ConstantTimeCompare(a, got)
	}
	return match == 1
}
