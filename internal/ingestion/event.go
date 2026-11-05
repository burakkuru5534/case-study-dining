// Package ingestion turns verified provider webhooks into normalized events.
package ingestion

import (
	"errors"
	"fmt"
	"strings"

	"github.com/burakkuru5534/case-study-dining/internal/order"
)

// EventTypeOrderCompleted is the only event type the platform processes.
const EventTypeOrderCompleted = "order.completed"

// maxEventTypeLength bounds the event type echoed in UnsupportedEventTypeError
// (it ends up in logs).
const maxEventTypeLength = 64

// ErrInvalidPayload marks a payload that can never be processed as sent.
// Intake answers 422; the worker treats it as a permanent failure.
var ErrInvalidPayload = errors.New("invalid payload")

// UnsupportedEventTypeError is returned for well-formed events of a type the
// platform does not handle. Intake acknowledges them without storing.
type UnsupportedEventTypeError struct {
	EventType string // truncated; safe to log
}

func (e *UnsupportedEventTypeError) Error() string {
	return fmt.Sprintf("unsupported event type %q", e.EventType)
}

// NewUnsupportedEventTypeError truncates the provider-supplied type so a
// hostile or broken sender cannot push arbitrary amounts of text into logs.
func NewUnsupportedEventTypeError(eventType string) *UnsupportedEventTypeError {
	if len(eventType) > maxEventTypeLength {
		// Byte truncation can split a multi-byte rune; drop the broken tail.
		eventType = strings.ToValidUTF8(eventType[:maxEventTypeLength], "")
	}
	return &UnsupportedEventTypeError{EventType: eventType}
}

// Event is a normalized, provider-independent order event.
type Event struct {
	Provider        string
	TenantRef       string
	ExternalEventID string
	EventType       string
	Order           order.CompletedOrder
}

// Validate checks the envelope and the order. The returned error wraps
// ErrInvalidPayload and names fields only.
func (e Event) Validate() error {
	var errs []error
	for _, f := range []struct{ name, value string }{
		{"provider", e.Provider},
		{"tenant_ref", e.TenantRef},
		{"event_id", e.ExternalEventID},
	} {
		if err := order.ValidateExternalID(f.name, f.value); err != nil {
			errs = append(errs, err)
		}
	}
	if e.EventType != EventTypeOrderCompleted {
		errs = append(errs, fmt.Errorf("event_type: must be %q", EventTypeOrderCompleted))
	}
	if err := e.Order.Validate(); err != nil {
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidPayload, err)
	}
	return nil
}

// Adapter normalizes one provider's webhook body. Adapters are pure: no I/O.
// Signature verification is a separate step that runs before Parse.
type Adapter interface {
	// Provider is the stable provider key stored with every event.
	Provider() string

	// Parse returns a validated Event, or an error that is (or wraps)
	// ErrInvalidPayload or *UnsupportedEventTypeError.
	Parse(body []byte) (Event, error)
}
