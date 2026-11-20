// Package testdb gives integration tests an isolated, fully migrated schema.
// It is imported only from _test.go files.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/burakkuru5534/case-study-dining/internal/platform/migrate"
	"github.com/burakkuru5534/case-study-dining/migrations"
)

// New creates a uniquely named schema, applies all embedded migrations to it
// and returns a pool whose search_path points at it. The schema is dropped
// when the test ends. Skips the test when TEST_DATABASE_URL is not set.
func New(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	ctx := context.Background()

	b := make([]byte, 6)
	_, _ = rand.Read(b)
	schema := "it_" + hex.EncodeToString(b)

	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("testdb: connect: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("testdb: create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		_ = admin.Close(context.Background())
	})

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("testdb: parse url: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema

	all, err := migrate.Load(migrations.FS)
	if err != nil {
		t.Fatalf("testdb: load migrations: %v", err)
	}
	conn, err := pgx.ConnectConfig(ctx, cfg.ConnConfig)
	if err != nil {
		t.Fatalf("testdb: connect for migrate: %v", err)
	}
	if _, err := migrate.Run(ctx, conn, all, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("testdb: migrate: %v", err)
	}
	_ = conn.Close(ctx)

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("testdb: pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}
