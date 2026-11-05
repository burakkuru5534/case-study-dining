package loyalty

import (
	"errors"
	"math"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

var tenTRY = Rule{Currency: "TRY", EarnUnitMinor: 1000, PointsPerUnit: 1}

func TestEvaluate(t *testing.T) {
	t.Parallel()

	customer := func(amount int64) Purchase {
		return Purchase{HasCustomer: true, Currency: "TRY", EligibleAmountMinor: amount}
	}

	tests := []struct {
		name       string
		rule       Rule
		purchase   Purchase
		wantPoints int64
		wantStatus Status
	}{
		// Boundaries of the 1 point / 10 TRY rule (1000 kuruş).
		{"zero amount", tenTRY, customer(0), 0, StatusBelowMinimum},
		{"1 kuruş", tenTRY, customer(1), 0, StatusBelowMinimum},
		{"9.99 TRY", tenTRY, customer(999), 0, StatusBelowMinimum},
		{"exactly 10 TRY earns 1", tenTRY, customer(1000), 1, StatusAwarded},
		{"10.01 TRY", tenTRY, customer(1001), 1, StatusAwarded},
		{"19.99 TRY", tenTRY, customer(1999), 1, StatusAwarded},
		{"20 TRY", tenTRY, customer(2000), 2, StatusAwarded},
		{"123.45 TRY", tenTRY, customer(12345), 12, StatusAwarded},
		{"max int64 amount", tenTRY, customer(math.MaxInt64), math.MaxInt64 / 1000, StatusAwarded},

		// Other rules.
		{"2 points per unit", Rule{"TRY", 1000, 2}, customer(2500), 4, StatusAwarded},
		{"1 point per kuruş", Rule{"TRY", 1, 1}, customer(1), 1, StatusAwarded},

		// Status precedence: no_customer → unsupported_currency → below_minimum → awarded.
		{"guest", tenTRY, Purchase{HasCustomer: false, Currency: "TRY", EligibleAmountMinor: 5000}, 0, StatusNoCustomer},
		{"guest beats unsupported currency", tenTRY, Purchase{HasCustomer: false, Currency: "EUR", EligibleAmountMinor: 5000}, 0, StatusNoCustomer},
		{"guest beats below minimum", tenTRY, Purchase{HasCustomer: false, Currency: "TRY", EligibleAmountMinor: 1}, 0, StatusNoCustomer},
		{"unsupported currency", tenTRY, Purchase{HasCustomer: true, Currency: "EUR", EligibleAmountMinor: 5000}, 0, StatusUnsupportedCurrency},
		{"unsupported currency beats below minimum", tenTRY, Purchase{HasCustomer: true, Currency: "USD", EligibleAmountMinor: 1}, 0, StatusUnsupportedCurrency},
		{"currency match is exact", tenTRY, Purchase{HasCustomer: true, Currency: "try", EligibleAmountMinor: 5000}, 0, StatusUnsupportedCurrency},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			points, status, err := Evaluate(tt.rule, tt.purchase)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if points != tt.wantPoints || status != tt.wantStatus {
				t.Fatalf("got (%d, %s), want (%d, %s)", points, status, tt.wantPoints, tt.wantStatus)
			}
		})
	}
}

func TestEvaluateErrors(t *testing.T) {
	t.Parallel()

	ok := Purchase{HasCustomer: true, Currency: "TRY", EligibleAmountMinor: 5000}

	tests := []struct {
		name     string
		rule     Rule
		purchase Purchase
		wantErr  string
		wantIs   error
	}{
		{"zero earn unit", Rule{"TRY", 0, 1}, ok, "earn_unit_minor", nil},
		{"negative earn unit", Rule{"TRY", -1000, 1}, ok, "earn_unit_minor", nil},
		{"zero points per unit", Rule{"TRY", 1000, 0}, ok, "points_per_unit", nil},
		{"negative points per unit", Rule{"TRY", 1000, -1}, ok, "points_per_unit", nil},
		{"invalid rule currency", Rule{"tl", 1000, 1}, ok, "rule currency", nil},
		{"empty rule currency", Rule{"", 1000, 1}, ok, "rule currency", nil},
		{"negative amount", tenTRY, Purchase{HasCustomer: true, Currency: "TRY", EligibleAmountMinor: -1}, "negative", nil},
		{"rule invalid even for guest", Rule{"TRY", 0, 1}, Purchase{HasCustomer: false, Currency: "TRY"}, "earn_unit_minor", nil},
		{
			name:     "multiplication overflow",
			rule:     Rule{"TRY", 1, 2},
			purchase: Purchase{HasCustomer: true, Currency: "TRY", EligibleAmountMinor: math.MaxInt64/2 + 1},
			wantIs:   ErrPointsOverflow,
		},
		{
			name:     "large points per unit overflow",
			rule:     Rule{"TRY", 1000, math.MaxInt64},
			purchase: Purchase{HasCustomer: true, Currency: "TRY", EligibleAmountMinor: 2000},
			wantIs:   ErrPointsOverflow,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			points, status, err := Evaluate(tt.rule, tt.purchase)
			if err == nil {
				t.Fatalf("expected error, got (%d, %s)", points, status)
			}
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Fatalf("error %v is not %v", err, tt.wantIs)
			}
			if tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tt.wantErr)
			}
			if points != 0 || status != "" {
				t.Fatalf("on error expected zero result, got (%d, %q)", points, status)
			}
		})
	}
}

func TestOverflowBoundaryDoesNotError(t *testing.T) {
	t.Parallel()
	// Largest amount that still fits with 2 points per unit.
	points, status, err := Evaluate(Rule{"TRY", 1, 2}, Purchase{HasCustomer: true, Currency: "TRY", EligibleAmountMinor: math.MaxInt64 / 2})
	if err != nil || status != StatusAwarded || points != (math.MaxInt64/2)*2 {
		t.Fatalf("got (%d, %s, %v)", points, status, err)
	}
}

// Guards against drift between Go and the orders.loyalty_status CHECK
// constraint: the set of statuses must be identical in both places.
func TestStatusValuesMatchSchema(t *testing.T) {
	t.Parallel()

	sql, err := os.ReadFile("../../migrations/0002_core_schema.sql")
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	m := regexp.MustCompile(`loyalty_status\s+text[^(]*\(\s*loyalty_status\s+IN\s*\(([^)]*)\)`).FindSubmatch(sql)
	if m == nil {
		t.Fatal("loyalty_status CHECK constraint not found in schema")
	}
	var inSchema []string
	for _, v := range regexp.MustCompile(`'([a-z_]+)'`).FindAllSubmatch(m[1], -1) {
		inSchema = append(inSchema, string(v[1]))
	}

	inGo := []string{string(StatusAwarded), string(StatusNoCustomer), string(StatusUnsupportedCurrency), string(StatusBelowMinimum)}

	slices.Sort(inSchema)
	slices.Sort(inGo)
	if !slices.Equal(inSchema, inGo) {
		t.Fatalf("status drift: schema %v, Go %v", inSchema, inGo)
	}
}
