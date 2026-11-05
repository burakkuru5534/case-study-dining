package reference

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/burakkuru5534/case-study-dining/internal/ingestion"
	"github.com/burakkuru5534/case-study-dining/internal/order"
)

// base returns a valid payload as a mutable map. json.Number is used where a
// test needs exact control over the raw JSON number text.
func base() map[string]any {
	return map[string]any{
		"event_id":   "evt-1",
		"event_type": "order.completed",
		"tenant_ref": "chain-1",
		"order": map[string]any{
			"restaurant_id":  "store-1",
			"order_id":       "ord-1",
			"customer_id":    "cust-1",
			"subtotal_minor": json.Number("25000"),
			"discount_minor": json.Number("2500"),
			"tax_minor":      json.Number("2250"),
			"tip_minor":      json.Number("1500"),
			"currency":       "TRY",
			"completed_at":   "2026-11-05T12:30:00+03:00",
		},
	}
}

func orderOf(p map[string]any) map[string]any { return p["order"].(map[string]any) }

func marshal(t *testing.T, p map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParse_Valid(t *testing.T) {
	t.Parallel()

	want := ingestion.Event{
		Provider:        "reference",
		TenantRef:       "chain-1",
		ExternalEventID: "evt-1",
		EventType:       "order.completed",
		Order: order.CompletedOrder{
			ExternalRestaurantID: "store-1",
			ExternalOrderID:      "ord-1",
			ExternalCustomerID:   "cust-1",
			EligibleAmountMinor:  22500, // 25000 - 2500; tax and tip excluded
			Currency:             "TRY",
			CompletedAt:          time.Date(2026, 11, 5, 9, 30, 0, 0, time.UTC),
		},
	}

	tests := []struct {
		name   string
		mutate func(p map[string]any)
		check  func(t *testing.T, got ingestion.Event)
	}{
		{name: "full payload", mutate: func(map[string]any) {}, check: func(t *testing.T, got ingestion.Event) {
			if got != want {
				t.Fatalf("got %+v\nwant %+v", got, want)
			}
		}},
		{name: "completed_at converted to UTC", mutate: func(map[string]any) {}, check: func(t *testing.T, got ingestion.Event) {
			if got.Order.CompletedAt.Location() != time.UTC {
				t.Fatalf("location = %v", got.Order.CompletedAt.Location())
			}
		}},
		{name: "tax and tip do not change eligible amount", mutate: func(p map[string]any) {
			orderOf(p)["tax_minor"] = json.Number("999999")
			orderOf(p)["tip_minor"] = json.Number("999999")
		}, check: wantEligible(22500)},
		{name: "tax and tip optional", mutate: func(p map[string]any) {
			delete(orderOf(p), "tax_minor")
			delete(orderOf(p), "tip_minor")
		}, check: wantEligible(22500)},
		{name: "discount optional", mutate: func(p map[string]any) { delete(orderOf(p), "discount_minor") }, check: wantEligible(25000)},
		{name: "discount null", mutate: func(p map[string]any) { orderOf(p)["discount_minor"] = nil }, check: wantEligible(25000)},
		{name: "discount equals subtotal", mutate: func(p map[string]any) {
			orderOf(p)["discount_minor"] = json.Number("25000")
		}, check: wantEligible(0)},
		{name: "zero subtotal", mutate: func(p map[string]any) {
			orderOf(p)["subtotal_minor"] = json.Number("0")
			orderOf(p)["discount_minor"] = json.Number("0")
		}, check: wantEligible(0)},
		{name: "max int64 subtotal", mutate: func(p map[string]any) {
			orderOf(p)["subtotal_minor"] = json.Number("9223372036854775807")
			orderOf(p)["discount_minor"] = json.Number("0")
		}, check: wantEligible(9223372036854775807)},
		{name: "customer missing is guest", mutate: func(p map[string]any) { delete(orderOf(p), "customer_id") }, check: wantGuest},
		{name: "customer null is guest", mutate: func(p map[string]any) { orderOf(p)["customer_id"] = nil }, check: wantGuest},
		{name: "tenant missing uses default", mutate: func(p map[string]any) { delete(p, "tenant_ref") }, check: wantTenant(DefaultTenantRef)},
		{name: "tenant null uses default", mutate: func(p map[string]any) { p["tenant_ref"] = nil }, check: wantTenant(DefaultTenantRef)},
		{name: "lower-case currency is normalized", mutate: func(p map[string]any) { orderOf(p)["currency"] = "try" }, check: func(t *testing.T, got ingestion.Event) {
			if got.Order.Currency != "TRY" {
				t.Fatalf("currency = %q", got.Order.Currency)
			}
		}},
		{name: "non-TRY currency is valid input", mutate: func(p map[string]any) { orderOf(p)["currency"] = "EUR" }, check: func(t *testing.T, got ingestion.Event) {
			if got.Order.Currency != "EUR" {
				t.Fatalf("currency = %q", got.Order.Currency)
			}
		}},
		{name: "unknown fields are ignored", mutate: func(p map[string]any) {
			p["schema_version"] = 3
			p["metadata"] = map[string]any{"x": []int{1, 2}}
			orderOf(p)["items"] = []any{map[string]any{"sku": "A", "qty": 2}}
			orderOf(p)["loyalty_hint"] = 1.5 // a float in an unknown field is fine
		}, check: func(t *testing.T, got ingestion.Event) {
			if got != want {
				t.Fatalf("got %+v", got)
			}
		}},
		{name: "fractional seconds", mutate: func(p map[string]any) {
			orderOf(p)["completed_at"] = "2026-11-05T09:30:00.123456Z"
		}, check: func(t *testing.T, got ingestion.Event) {
			if got.Order.CompletedAt.Nanosecond() != 123456000 {
				t.Fatalf("nanos = %d", got.Order.CompletedAt.Nanosecond())
			}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := base()
			tt.mutate(p)
			got, err := Adapter{}.Parse(marshal(t, p))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			tt.check(t, got)
		})
	}
}

func wantEligible(v int64) func(*testing.T, ingestion.Event) {
	return func(t *testing.T, got ingestion.Event) {
		t.Helper()
		if got.Order.EligibleAmountMinor != v {
			t.Fatalf("eligible = %d, want %d", got.Order.EligibleAmountMinor, v)
		}
	}
}

func wantTenant(v string) func(*testing.T, ingestion.Event) {
	return func(t *testing.T, got ingestion.Event) {
		t.Helper()
		if got.TenantRef != v {
			t.Fatalf("tenant = %q, want %q", got.TenantRef, v)
		}
	}
}

func wantGuest(t *testing.T, got ingestion.Event) {
	t.Helper()
	if got.Order.HasCustomer() {
		t.Fatalf("expected guest, got customer %q", got.Order.ExternalCustomerID)
	}
}

func TestParse_Invalid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(p map[string]any)
		wantErr string // substring
	}{
		// Envelope.
		{"missing event_id", func(p map[string]any) { delete(p, "event_id") }, "event_id: required"},
		{"empty event_id", func(p map[string]any) { p["event_id"] = "" }, "event_id: required"},
		{"numeric event_id", func(p map[string]any) { p["event_id"] = 123 }, "event_id: wrong JSON type"},
		{"missing event_type", func(p map[string]any) { delete(p, "event_type") }, "event_type: required"},
		{"empty tenant_ref", func(p map[string]any) { p["tenant_ref"] = "" }, "tenant_ref: required"},
		{"tenant_ref too long", func(p map[string]any) { p["tenant_ref"] = strings.Repeat("t", 129) }, "tenant_ref: longer than"},
		{"missing order", func(p map[string]any) { delete(p, "order") }, "order: required"},
		{"null order", func(p map[string]any) { p["order"] = nil }, "order: required"},
		{"order is an array", func(p map[string]any) { p["order"] = []any{} }, "body must be a JSON object"},

		// Order identifiers.
		{"missing restaurant_id", func(p map[string]any) { delete(orderOf(p), "restaurant_id") }, "external_restaurant_id: required"},
		{"missing order_id", func(p map[string]any) { delete(orderOf(p), "order_id") }, "external_order_id: required"},
		{"numeric order_id", func(p map[string]any) { orderOf(p)["order_id"] = 42 }, "order_id: wrong JSON type"},
		{"empty customer_id", func(p map[string]any) { orderOf(p)["customer_id"] = "" }, "customer_id: must be omitted or null"},
		{"customer_id with whitespace", func(p map[string]any) { orderOf(p)["customer_id"] = " c-1 " }, "external_customer_id: must not have leading or trailing whitespace"},

		// Amounts: integers in minor units only.
		{"missing subtotal", func(p map[string]any) { delete(orderOf(p), "subtotal_minor") }, "subtotal_minor: required"},
		{"null subtotal", func(p map[string]any) { orderOf(p)["subtotal_minor"] = nil }, "subtotal_minor: required"},
		{"fractional subtotal", func(p map[string]any) { orderOf(p)["subtotal_minor"] = json.Number("250.50") }, "subtotal_minor: must be an integer"},
		{"integral float subtotal", func(p map[string]any) { orderOf(p)["subtotal_minor"] = json.Number("25000.0") }, "subtotal_minor: must be an integer"},
		{"exponent subtotal", func(p map[string]any) { orderOf(p)["subtotal_minor"] = json.Number("2.5e4") }, "subtotal_minor: must be an integer"},
		{"string subtotal", func(p map[string]any) { orderOf(p)["subtotal_minor"] = "25000" }, "subtotal_minor: must be an integer"},
		{"boolean subtotal", func(p map[string]any) { orderOf(p)["subtotal_minor"] = true }, "subtotal_minor: must be an integer"},
		{"subtotal overflows int64", func(p map[string]any) { orderOf(p)["subtotal_minor"] = json.Number("9223372036854775808") }, "subtotal_minor: must be an integer number of minor units within int64 range"},
		{"negative subtotal", func(p map[string]any) { orderOf(p)["subtotal_minor"] = json.Number("-1") }, "subtotal_minor: must not be negative"},
		{"negative discount", func(p map[string]any) { orderOf(p)["discount_minor"] = json.Number("-100") }, "discount_minor: must not be negative"},
		{"fractional discount", func(p map[string]any) { orderOf(p)["discount_minor"] = json.Number("0.5") }, "discount_minor: must be an integer"},
		{"discount exceeds subtotal", func(p map[string]any) { orderOf(p)["discount_minor"] = json.Number("25001") }, "discount_minor: must not exceed subtotal_minor"},
		{"negative tax", func(p map[string]any) { orderOf(p)["tax_minor"] = json.Number("-1") }, "tax_minor: must not be negative"},
		{"fractional tip", func(p map[string]any) { orderOf(p)["tip_minor"] = json.Number("1.25") }, "tip_minor: must be an integer"},

		// Currency and time.
		{"missing currency", func(p map[string]any) { delete(orderOf(p), "currency") }, "currency"},
		{"invalid currency", func(p map[string]any) { orderOf(p)["currency"] = "TL" }, "currency"},
		{"currency with digits", func(p map[string]any) { orderOf(p)["currency"] = "TR1" }, "currency"},
		{"missing completed_at", func(p map[string]any) { delete(orderOf(p), "completed_at") }, "completed_at: required"},
		{"completed_at without offset", func(p map[string]any) { orderOf(p)["completed_at"] = "2026-11-05T12:30:00" }, "completed_at: must be RFC 3339"},
		{"completed_at date only", func(p map[string]any) { orderOf(p)["completed_at"] = "2026-11-05" }, "completed_at: must be RFC 3339"},
		{"completed_at unix number", func(p map[string]any) { orderOf(p)["completed_at"] = 1793872200 }, "completed_at: wrong JSON type"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := base()
			tt.mutate(p)
			_, err := Adapter{}.Parse(marshal(t, p))
			assertInvalid(t, err, tt.wantErr)
		})
	}
}

func TestParse_InvalidBody(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{"empty body", "", "body is empty"},
		{"whitespace body", "  \n ", "body is empty"},
		{"not JSON", "hello", "malformed JSON"},
		{"truncated JSON", `{"event_id": "evt-1", "order": {`, "malformed JSON"},
		{"JSON array", `[1, 2]`, "body must be a JSON object"},
		{"JSON string", `"order.completed"`, "body must be a JSON object"},
		{"null body", `null`, "event_type: required"},
		{"trailing data", `{"event_type": "order.completed"} {"x": 1}`, "unexpected data after JSON object"},
		{"leading zero number", `{"event_type": "order.completed", "order": {"subtotal_minor": 0100}}`, "malformed JSON"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Adapter{}.Parse([]byte(tt.body))
			assertInvalid(t, err, tt.wantErr)
		})
	}
}

func assertInvalid(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ingestion.ErrInvalidPayload) {
		t.Fatalf("error does not wrap ErrInvalidPayload: %v", err)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not contain %q", err, want)
	}
}

func TestParse_UnsupportedEventType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		body     string
		wantType string
	}{
		{
			name:     "other event type",
			body:     `{"event_id": "evt-1", "event_type": "order.created", "order": {"order_id": "o-1"}}`,
			wantType: "order.created",
		},
		{
			// Recognized before the order is decoded, so a different body shape is fine.
			name:     "body shape of another event is not validated",
			body:     `{"event_type": "menu.updated", "order": [1, 2, 3], "menu": {"items": 1.5}}`,
			wantType: "menu.updated",
		},
		{
			name:     "long event type is truncated",
			body:     `{"event_type": "` + strings.Repeat("x", 500) + `"}`,
			wantType: strings.Repeat("x", 64),
		},
		{
			name:     "truncation does not split a multi-byte rune",
			body:     `{"event_type": "` + strings.Repeat("x", 63) + `ş-tail"}`,
			wantType: strings.Repeat("x", 63),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Adapter{}.Parse([]byte(tt.body))
			var unsupported *ingestion.UnsupportedEventTypeError
			if !errors.As(err, &unsupported) {
				t.Fatalf("expected UnsupportedEventTypeError, got %v", err)
			}
			if errors.Is(err, ingestion.ErrInvalidPayload) {
				t.Fatal("unsupported event type must not be reported as invalid payload")
			}
			if unsupported.EventType != tt.wantType {
				t.Fatalf("event type = %q, want %q", unsupported.EventType, tt.wantType)
			}
		})
	}
}

// Errors are logged by intake and stored as last_error by the worker, so they
// must never echo payload values.
func TestParse_ErrorsDoNotLeakPayloadValues(t *testing.T) {
	t.Parallel()

	const secret = "SECRET-CUSTOMER-9f8e7d"
	cases := map[string]func(p map[string]any){
		"customer too long": func(p map[string]any) { orderOf(p)["customer_id"] = secret + strings.Repeat("z", 200) },
		"customer whitespace": func(p map[string]any) {
			orderOf(p)["customer_id"] = " " + secret
		},
		"secret in bad currency":  func(p map[string]any) { orderOf(p)["currency"] = secret },
		"secret in bad timestamp": func(p map[string]any) { orderOf(p)["completed_at"] = secret },
		"secret in bad amount":    func(p map[string]any) { orderOf(p)["subtotal_minor"] = secret },
		"secret as wrong type":    func(p map[string]any) { orderOf(p)["order_id"] = map[string]any{"v": secret} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := base()
			mutate(p)
			_, err := Adapter{}.Parse(marshal(t, p))
			if err == nil {
				t.Fatal("expected error")
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error leaks payload value: %v", err)
			}
		})
	}

	t.Run("malformed JSON", func(t *testing.T) {
		t.Parallel()
		_, err := Adapter{}.Parse([]byte(`{"order": {"customer_id": "` + secret + `" "x"}}`))
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Fatalf("expected sanitized error, got %v", err)
		}
	})
}

// The documented samples must stay valid: docs cannot drift from the adapter.
func TestDocumentedSamples(t *testing.T) {
	t.Parallel()

	tests := []struct {
		file         string
		wantEligible int64
		wantCustomer bool
		wantTenant   string
	}{
		{"order_completed.json", 22500, true, "chain-istanbul"},
		{"order_completed_guest.json", 8000, false, DefaultTenantRef},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			t.Parallel()
			body, err := os.ReadFile("../../../docs/reference-webhook/" + tt.file)
			if err != nil {
				t.Fatal(err)
			}
			ev, err := Adapter{}.Parse(body)
			if err != nil {
				t.Fatalf("documented sample is invalid: %v", err)
			}
			if ev.Order.EligibleAmountMinor != tt.wantEligible || ev.Order.HasCustomer() != tt.wantCustomer || ev.TenantRef != tt.wantTenant {
				t.Fatalf("got eligible=%d customer=%v tenant=%q", ev.Order.EligibleAmountMinor, ev.Order.HasCustomer(), ev.TenantRef)
			}
		})
	}
}
