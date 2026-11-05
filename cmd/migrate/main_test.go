package main

import (
	"testing"

	"github.com/burakkuru5534/case-study-dining/internal/platform/migrate"
	"github.com/burakkuru5534/case-study-dining/migrations"
)

// Catches bad file names, duplicates and gaps in the real migration set at
// test time instead of at deploy time.
func TestEmbeddedMigrationsAreValid(t *testing.T) {
	t.Parallel()

	all, err := migrate.Load(migrations.FS)
	if err != nil {
		t.Fatalf("embedded migrations invalid: %v", err)
	}
	if len(all) == 0 {
		t.Fatal("no embedded migrations found")
	}
}
