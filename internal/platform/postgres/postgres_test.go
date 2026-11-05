package postgres

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestNewPool_InvalidURLDoesNotLeakPassword(t *testing.T) {
	t.Parallel()

	const secret = "s3cr3t-password"
	// Invalid port forces a parse error while the URL still contains a password.
	_, err := NewPool(context.Background(), "postgres://user:"+secret+"@localhost:notaport/db")
	if err == nil {
		t.Fatal("expected error for invalid url")
	}
	if !strings.Contains(err.Error(), "parse database url") {
		t.Errorf("unexpected error: %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error leaks password: %v", err)
	}
}

func TestNewPool_UnreachableDatabaseFails(t *testing.T) {
	t.Parallel()

	// Port 1 on loopback: connection is refused immediately, no database needed.
	start := time.Now()
	_, err := NewPool(context.Background(), "postgres://user:pass@127.0.0.1:1/db?sslmode=disable")
	if err == nil {
		t.Fatal("expected error for unreachable database")
	}
	if !strings.Contains(err.Error(), "ping database") {
		t.Errorf("unexpected error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > pingTimeout+time.Second {
		t.Errorf("took %s, expected to be bounded by ping timeout %s", elapsed, pingTimeout)
	}
}

// TestNewPool_Integration runs against a real PostgreSQL only when
// TEST_DATABASE_URL is set, e.g. against the docker compose database:
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:55432/dining?sslmode=disable go test ./...
func TestNewPool_Integration(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := NewPool(ctx, url)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(pool.Close)

	var one int
	if err := pool.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("query: %v", err)
	}
	if one != 1 {
		t.Fatalf("got %d, want 1", one)
	}
}
