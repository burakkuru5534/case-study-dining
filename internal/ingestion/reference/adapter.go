// Package reference implements the adapter for the platform's own internal
// reference webhook format (docs/reference-webhook-format.md).
//
// This is NOT a real POS provider's specification. It is our contract for
// tests, demos and providers that integrate against us directly. Real
// providers get their own adapter package once their contract is known.
package reference

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/burakkuru5534/case-study-dining/internal/ingestion"
	"github.com/burakkuru5534/case-study-dining/internal/order"
)

const (
	// ProviderName is the provider key stored with events from this adapter.
	ProviderName = "reference"
	// DefaultTenantRef is used when a payload carries no tenant_ref.
	DefaultTenantRef = "default"
)

// Adapter parses the reference format. The zero value is ready to use.
type Adapter struct{}

var _ ingestion.Adapter = Adapter{}

func (Adapter) Provider() string { return ProviderName }

// envelope is decoded first, with the order kept raw, so an unsupported event
// type is recognized even if its body has a different shape.
type envelope struct {
	EventID   string          `json:"event_id"`
	EventType string          `json:"event_type"`
	TenantRef *string         `json:"tenant_ref"`
	Order     json.RawMessage `json:"order"`
}

type orderBody struct {
	RestaurantID  string          `json:"restaurant_id"`
	OrderID       string          `json:"order_id"`
	CustomerID    *string         `json:"customer_id"`    // missing or null = guest
	SubtotalMinor json.RawMessage `json:"subtotal_minor"` // required
	DiscountMinor json.RawMessage `json:"discount_minor"` // optional, default 0
	TaxMinor      json.RawMessage `json:"tax_minor"`      // optional, validated, excluded
	TipMinor      json.RawMessage `json:"tip_minor"`      // optional, validated, excluded
	Currency      string          `json:"currency"`
	CompletedAt   string          `json:"completed_at"`
}

// Parse decodes and validates a reference-format body. Errors name fields
// only and never include payload values.
func (Adapter) Parse(body []byte) (ingestion.Event, error) {
	var env envelope
	if err := decodeStrict(body, &env); err != nil {
		return ingestion.Event{}, invalid(err)
	}

	if env.EventType == "" {
		return ingestion.Event{}, invalid(errors.New("event_type: required"))
	}
	if env.EventType != ingestion.EventTypeOrderCompleted {
		return ingestion.Event{}, ingestion.NewUnsupportedEventTypeError(env.EventType)
	}

	tenant := DefaultTenantRef
	if env.TenantRef != nil {
		tenant = *env.TenantRef // explicit "" is rejected by validation, not defaulted
	}

	if len(env.Order) == 0 || string(env.Order) == "null" {
		return ingestion.Event{}, invalid(errors.New("order: required"))
	}
	var ob orderBody
	if err := decodeStrict(env.Order, &ob); err != nil {
		return ingestion.Event{}, invalid(fmt.Errorf("order: %w", err))
	}

	o, err := ob.toOrder()
	if err != nil {
		return ingestion.Event{}, invalid(err)
	}

	ev := ingestion.Event{
		Provider:        ProviderName,
		TenantRef:       tenant,
		ExternalEventID: env.EventID,
		EventType:       env.EventType,
		Order:           o,
	}
	if err := ev.Validate(); err != nil {
		return ingestion.Event{}, err // already wraps ErrInvalidPayload
	}
	return ev, nil
}

// toOrder applies the amount policy: eligible = subtotal - discount;
// tax and tip are excluded.
func (b orderBody) toOrder() (order.CompletedOrder, error) {
	var errs []error

	subtotal, ok, err := parseMinor("order.subtotal_minor", b.SubtotalMinor)
	if err != nil {
		errs = append(errs, err)
	} else if !ok {
		errs = append(errs, errors.New("order.subtotal_minor: required"))
	}
	discount, _, err := parseMinor("order.discount_minor", b.DiscountMinor)
	if err != nil {
		errs = append(errs, err)
	}
	// Tax and tip do not earn points, but malformed values still make the payload invalid.
	for _, f := range []struct {
		name string
		raw  json.RawMessage
	}{{"order.tax_minor", b.TaxMinor}, {"order.tip_minor", b.TipMinor}} {
		if _, _, err := parseMinor(f.name, f.raw); err != nil {
			errs = append(errs, err)
		}
	}
	if discount > subtotal {
		errs = append(errs, errors.New("order.discount_minor: must not exceed subtotal_minor"))
	}

	customer := ""
	if b.CustomerID != nil {
		if *b.CustomerID == "" {
			errs = append(errs, errors.New("order.customer_id: must be omitted or null for guests, not empty"))
		}
		customer = *b.CustomerID
	}

	var completedAt time.Time
	if b.CompletedAt == "" {
		errs = append(errs, errors.New("order.completed_at: required"))
	} else if t, err := time.Parse(time.RFC3339Nano, b.CompletedAt); err != nil {
		errs = append(errs, errors.New("order.completed_at: must be RFC 3339 with a time zone offset"))
	} else {
		completedAt = t.UTC()
	}

	if err := errors.Join(errs...); err != nil {
		return order.CompletedOrder{}, err
	}

	return order.CompletedOrder{
		ExternalRestaurantID: b.RestaurantID,
		ExternalOrderID:      b.OrderID,
		ExternalCustomerID:   customer,
		// No overflow: both operands are non-negative and discount <= subtotal.
		EligibleAmountMinor: subtotal - discount,
		Currency:            strings.ToUpper(b.Currency),
		CompletedAt:         completedAt,
	}, nil
}

// parseMinor accepts only non-negative JSON integers. Strings ("100"),
// fractions (1.5) and exponents (1e3) are rejected, so no float ever enters
// money handling. Missing or null reports ok=false.
func parseMinor(field string, raw json.RawMessage) (v int64, ok bool, err error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false, nil
	}
	v, err = strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("%s: must be an integer number of minor units within int64 range", field)
	}
	if v < 0 {
		return 0, false, fmt.Errorf("%s: must not be negative", field)
	}
	return v, true, nil
}

// decodeStrict decodes exactly one JSON object. Unknown fields are allowed
// (providers add fields over time); trailing data is not.
func decodeStrict(data []byte, v any) error {
	if len(bytes.TrimSpace(data)) == 0 {
		return errors.New("body is empty")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(v); err != nil {
		return sanitizeJSONError(err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("unexpected data after JSON object")
	}
	return nil
}

// sanitizeJSONError keeps the field name but drops anything that could echo
// payload content.
func sanitizeJSONError(err error) error {
	var typeErr *json.UnmarshalTypeError
	var syntaxErr *json.SyntaxError
	switch {
	case errors.As(err, &typeErr):
		if typeErr.Field != "" {
			return fmt.Errorf("%s: wrong JSON type", typeErr.Field)
		}
		return errors.New("body must be a JSON object")
	case errors.As(err, &syntaxErr):
		return fmt.Errorf("malformed JSON at offset %d", syntaxErr.Offset)
	case errors.Is(err, io.ErrUnexpectedEOF):
		return errors.New("malformed JSON: unexpected end of input")
	default:
		return errors.New("malformed JSON")
	}
}

func invalid(err error) error {
	return fmt.Errorf("%w: %w", ingestion.ErrInvalidPayload, err)
}
