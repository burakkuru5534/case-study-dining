// Package order defines the provider-independent representation of a
// completed restaurant order. It has no HTTP, database or provider dependencies.
package order

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// MaxExternalIDLength bounds identifiers supplied by providers.
const MaxExternalIDLength = 128

var currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

// CompletedOrder is what every provider adapter produces.
type CompletedOrder struct {
	ExternalRestaurantID string
	ExternalOrderID      string
	// ExternalCustomerID is empty for guest orders.
	ExternalCustomerID string
	// EligibleAmountMinor is the subtotal after discounts, excluding tax and
	// tips, in minor units (kuruş for TRY). The adapter owns this mapping.
	EligibleAmountMinor int64
	// Currency is an upper-case ISO 4217 code.
	Currency    string
	CompletedAt time.Time
}

// HasCustomer reports whether the order belongs to an identified customer.
func (o CompletedOrder) HasCustomer() bool {
	return o.ExternalCustomerID != ""
}

// Validate reports every invalid field. Messages name fields only and never
// include values, so they are safe to log.
func (o CompletedOrder) Validate() error {
	var errs []error

	if err := ValidateExternalID("external_restaurant_id", o.ExternalRestaurantID); err != nil {
		errs = append(errs, err)
	}
	if err := ValidateExternalID("external_order_id", o.ExternalOrderID); err != nil {
		errs = append(errs, err)
	}
	if o.HasCustomer() {
		if err := ValidateExternalID("external_customer_id", o.ExternalCustomerID); err != nil {
			errs = append(errs, err)
		}
	}
	if o.EligibleAmountMinor < 0 {
		errs = append(errs, errors.New("eligible_amount_minor: must not be negative"))
	}
	if !ValidCurrency(o.Currency) {
		errs = append(errs, errors.New("currency: must be a 3-letter upper-case ISO 4217 code"))
	}
	if o.CompletedAt.IsZero() {
		errs = append(errs, errors.New("completed_at: required"))
	}

	return errors.Join(errs...)
}

// ValidCurrency reports whether code looks like an upper-case ISO 4217 code.
func ValidCurrency(code string) bool {
	return currencyPattern.MatchString(code)
}

// ValidateExternalID checks a provider-supplied identifier: required, bounded
// length, no surrounding whitespace and no control characters.
func ValidateExternalID(field, v string) error {
	switch {
	case v == "":
		return fmt.Errorf("%s: required", field)
	case len(v) > MaxExternalIDLength:
		return fmt.Errorf("%s: longer than %d bytes", field, MaxExternalIDLength)
	case strings.TrimSpace(v) != v:
		return fmt.Errorf("%s: must not have leading or trailing whitespace", field)
	case strings.IndexFunc(v, unicode.IsControl) >= 0:
		return fmt.Errorf("%s: must not contain control characters", field)
	}
	return nil
}
