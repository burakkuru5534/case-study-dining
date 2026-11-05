package ingestion

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/burakkuru5534/case-study-dining/internal/order"
)

func validEvent() Event {
	return Event{
		Provider:        "reference",
		TenantRef:       "default",
		ExternalEventID: "evt-1",
		EventType:       EventTypeOrderCompleted,
		Order: order.CompletedOrder{
			ExternalRestaurantID: "r-1",
			ExternalOrderID:      "o-1",
			EligibleAmountMinor:  1000,
			Currency:             "TRY",
			CompletedAt:          time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC),
		},
	}
}

func TestEventValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*Event)
		wantErr string
	}{
		{"valid", func(*Event) {}, ""},
		{"missing provider", func(e *Event) { e.Provider = "" }, "provider: required"},
		{"missing tenant", func(e *Event) { e.TenantRef = "" }, "tenant_ref: required"},
		{"missing event id", func(e *Event) { e.ExternalEventID = "" }, "event_id: required"},
		{"wrong event type", func(e *Event) { e.EventType = "order.created" }, "event_type"},
		{"invalid order", func(e *Event) { e.Order.Currency = "" }, "currency"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := validEvent()
			tt.mutate(&e)
			err := e.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrInvalidPayload) {
				t.Fatalf("error does not wrap ErrInvalidPayload: %v", err)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}
