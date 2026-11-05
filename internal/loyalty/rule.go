// Package loyalty contains the earn rules. It is pure: no HTTP, database or
// provider dependencies, and integer arithmetic only.
package loyalty

import (
	"errors"
	"fmt"
	"math"
	"regexp"
)

// Status is the loyalty outcome recorded on an order. The values match the
// orders.loyalty_status CHECK constraint in migrations/0002_core_schema.sql.
type Status string

const (
	StatusAwarded             Status = "awarded"
	StatusNoCustomer          Status = "no_customer"
	StatusUnsupportedCurrency Status = "unsupported_currency"
	StatusBelowMinimum        Status = "below_minimum"
)

// ErrPointsOverflow is returned when the points would not fit in an int64.
var ErrPointsOverflow = errors.New("loyalty: points calculation overflows int64")

var currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

// Rule is a program's earn rule: PointsPerUnit points for every full
// EarnUnitMinor of eligible spend in Currency.
// The initial rule is 1 point per 10 TRY: {Currency: "TRY", EarnUnitMinor: 1000, PointsPerUnit: 1}.
type Rule struct {
	Currency      string
	EarnUnitMinor int64
	PointsPerUnit int64
}

// Validate checks the rule itself, independent of any order.
func (r Rule) Validate() error {
	var errs []error
	if !currencyPattern.MatchString(r.Currency) {
		errs = append(errs, errors.New("rule currency: must be a 3-letter upper-case ISO 4217 code"))
	}
	if r.EarnUnitMinor <= 0 {
		errs = append(errs, errors.New("rule earn_unit_minor: must be positive"))
	}
	if r.PointsPerUnit <= 0 {
		errs = append(errs, errors.New("rule points_per_unit: must be positive"))
	}
	return errors.Join(errs...)
}

// Purchase is the information Evaluate needs about an order.
type Purchase struct {
	HasCustomer         bool
	Currency            string
	EligibleAmountMinor int64
}

// Evaluate returns the points to award and the resulting status.
//
// Status precedence: no_customer → unsupported_currency → below_minimum → awarded.
// Points are non-zero only for StatusAwarded.
func Evaluate(r Rule, p Purchase) (int64, Status, error) {
	if err := r.Validate(); err != nil {
		return 0, "", fmt.Errorf("invalid rule: %w", err)
	}
	if p.EligibleAmountMinor < 0 {
		return 0, "", errors.New("eligible amount must not be negative")
	}

	switch {
	case !p.HasCustomer:
		return 0, StatusNoCustomer, nil
	case p.Currency != r.Currency:
		return 0, StatusUnsupportedCurrency, nil
	}

	units := p.EligibleAmountMinor / r.EarnUnitMinor // integer division: only full units earn
	if units == 0 {
		return 0, StatusBelowMinimum, nil
	}
	if units > math.MaxInt64/r.PointsPerUnit {
		return 0, "", ErrPointsOverflow
	}
	return units * r.PointsPerUnit, StatusAwarded, nil
}
