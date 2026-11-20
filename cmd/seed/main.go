// Command seed provisions development data: the default loyalty program
// (1 point per 10 TRY) and the restaurants used by the documented sample
// payloads. It is idempotent and refuses to run with APP_ENV=production.
//
//	go run ./cmd/seed
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/burakkuru5534/case-study-dining/internal/ingestion/reference"
	"github.com/burakkuru5534/case-study-dining/internal/platform/config"
	"github.com/burakkuru5534/case-study-dining/internal/platform/postgres"
)

const (
	programName = "Default TRY (1 point per 10 TRY)"
	// Serializes concurrent seed runs: loyalty_programs.name is not unique.
	seedLockKey int64 = 0x7365656473 // "seeds"
)

// Restaurants referenced by docs/reference-webhook/*.json.
var restaurants = []struct{ tenantRef, externalID string }{
	{"chain-istanbul", "store-0042"},
	{reference.DefaultTenantRef, "store-0042"},
}

func main() {
	if err := mainErr(os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		os.Exit(1)
	}
}

func mainErr(out io.Writer) error {
	if os.Getenv("APP_ENV") == config.EnvProduction {
		return errors.New("refusing to seed development data with APP_ENV=production")
	}
	url, err := config.LoadDatabaseURL()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := postgres.NewPool(ctx, url)
	if err != nil {
		return err
	}
	defer pool.Close()
	return run(ctx, pool, out)
}

func run(ctx context.Context, pool *pgxpool.Pool, out io.Writer) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", seedLockKey); err != nil {
			return fmt.Errorf("lock: %w", err)
		}

		var programID int64
		err := tx.QueryRow(ctx, `SELECT id FROM loyalty_programs WHERE name = $1 ORDER BY id LIMIT 1`, programName).Scan(&programID)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			if err := tx.QueryRow(ctx, `
				INSERT INTO loyalty_programs (name, earn_currency, earn_unit_minor, points_per_unit)
				VALUES ($1, 'TRY', 1000, 1) RETURNING id`, programName).Scan(&programID); err != nil {
				return fmt.Errorf("create program: %w", err)
			}
			fmt.Fprintf(out, "created loyalty program %d: %s\n", programID, programName)
		case err != nil:
			return fmt.Errorf("find program: %w", err)
		default:
			fmt.Fprintf(out, "loyalty program %d already exists\n", programID)
		}

		for _, r := range restaurants {
			tag, err := tx.Exec(ctx, `
				INSERT INTO restaurants (provider, tenant_ref, external_restaurant_id, loyalty_program_id)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT (provider, tenant_ref, external_restaurant_id) DO NOTHING`,
				reference.ProviderName, r.tenantRef, r.externalID, programID)
			if err != nil {
				return fmt.Errorf("create restaurant %s/%s: %w", r.tenantRef, r.externalID, err)
			}
			state := "already exists"
			if tag.RowsAffected() == 1 {
				state = "created"
			}
			fmt.Fprintf(out, "restaurant %s/%s/%s: %s\n", reference.ProviderName, r.tenantRef, r.externalID, state)
		}
		return nil
	})
}
