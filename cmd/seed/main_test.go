package main

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/burakkuru5534/case-study-dining/internal/platform/testdb"
)

func TestSeedIsIdempotent(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()

	var first, second bytes.Buffer
	if err := run(ctx, db, &first); err != nil {
		t.Fatal(err)
	}
	if err := run(ctx, db, &second); err != nil {
		t.Fatal(err)
	}
	if strings.Count(first.String(), "created") != 3 {
		t.Fatalf("first run:\n%s", first.String())
	}
	if strings.Contains(second.String(), "created") {
		t.Fatalf("second run created rows:\n%s", second.String())
	}

	var programs, restaurants int
	if err := db.QueryRow(ctx, `SELECT (SELECT count(*) FROM loyalty_programs), (SELECT count(*) FROM restaurants)`).
		Scan(&programs, &restaurants); err != nil {
		t.Fatal(err)
	}
	if programs != 1 || restaurants != 2 {
		t.Fatalf("programs=%d restaurants=%d, want 1/2", programs, restaurants)
	}

	var currency string
	var unit, ppu int64
	if err := db.QueryRow(ctx, `SELECT earn_currency, earn_unit_minor, points_per_unit FROM loyalty_programs`).
		Scan(&currency, &unit, &ppu); err != nil {
		t.Fatal(err)
	}
	if currency != "TRY" || unit != 1000 || ppu != 1 {
		t.Fatalf("rule = %s %d/%d, want TRY 1000/1", currency, unit, ppu)
	}
}

func TestSeedConcurrentRunsCreateOneProgram(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make(chan error, 5)
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- run(ctx, db, &bytes.Buffer{})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var programs int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM loyalty_programs`).Scan(&programs); err != nil {
		t.Fatal(err)
	}
	if programs != 1 {
		t.Fatalf("programs = %d, want 1", programs)
	}
}

func TestSeedRefusesProduction(t *testing.T) {
	t.Setenv("APP_ENV", "production")
	t.Setenv("DATABASE_URL", "postgres://unused")
	err := mainErr(&bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("err = %v", err)
	}
}
