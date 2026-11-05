package order

import (
	"strings"
	"testing"
	"time"
)

func validOrder() CompletedOrder {
	return CompletedOrder{
		ExternalRestaurantID: "r-1",
		ExternalOrderID:      "o-1",
		ExternalCustomerID:   "c-1",
		EligibleAmountMinor:  12345,
		Currency:             "TRY",
		CompletedAt:          time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC),
	}
}

func TestCompletedOrderValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*CompletedOrder)
		wantErr []string // nil = valid
	}{
		{name: "valid", mutate: func(*CompletedOrder) {}},
		{name: "guest order", mutate: func(o *CompletedOrder) { o.ExternalCustomerID = "" }},
		{name: "zero amount", mutate: func(o *CompletedOrder) { o.EligibleAmountMinor = 0 }},
		{name: "id at max length", mutate: func(o *CompletedOrder) { o.ExternalOrderID = strings.Repeat("a", MaxExternalIDLength) }},
		{name: "non-ASCII id", mutate: func(o *CompletedOrder) { o.ExternalOrderID = "sipariş-1" }},

		{name: "missing restaurant", mutate: func(o *CompletedOrder) { o.ExternalRestaurantID = "" }, wantErr: []string{"external_restaurant_id: required"}},
		{name: "missing order id", mutate: func(o *CompletedOrder) { o.ExternalOrderID = "" }, wantErr: []string{"external_order_id: required"}},
		{name: "id too long", mutate: func(o *CompletedOrder) { o.ExternalOrderID = strings.Repeat("a", MaxExternalIDLength+1) }, wantErr: []string{"external_order_id: longer than"}},
		{name: "id with whitespace", mutate: func(o *CompletedOrder) { o.ExternalOrderID = " o-1" }, wantErr: []string{"whitespace"}},
		{name: "id with control char", mutate: func(o *CompletedOrder) { o.ExternalOrderID = "o\n1" }, wantErr: []string{"control characters"}},
		{name: "customer id whitespace only", mutate: func(o *CompletedOrder) { o.ExternalCustomerID = "   " }, wantErr: []string{"external_customer_id"}},
		{name: "negative amount", mutate: func(o *CompletedOrder) { o.EligibleAmountMinor = -1 }, wantErr: []string{"eligible_amount_minor"}},
		{name: "lower-case currency", mutate: func(o *CompletedOrder) { o.Currency = "try" }, wantErr: []string{"currency"}},
		{name: "short currency", mutate: func(o *CompletedOrder) { o.Currency = "TL" }, wantErr: []string{"currency"}},
		{name: "missing currency", mutate: func(o *CompletedOrder) { o.Currency = "" }, wantErr: []string{"currency"}},
		{name: "missing completed_at", mutate: func(o *CompletedOrder) { o.CompletedAt = time.Time{} }, wantErr: []string{"completed_at: required"}},
		{
			name: "all errors reported together",
			mutate: func(o *CompletedOrder) {
				*o = CompletedOrder{EligibleAmountMinor: -5}
			},
			wantErr: []string{"external_restaurant_id", "external_order_id", "eligible_amount_minor", "currency", "completed_at"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			o := validOrder()
			tt.mutate(&o)
			err := o.Validate()

			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected error")
			}
			for _, s := range tt.wantErr {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("error %q does not contain %q", err, s)
				}
			}
		})
	}
}

func TestValidateErrorsDoNotLeakValues(t *testing.T) {
	t.Parallel()

	const secret = "customer-secret-identifier"
	o := validOrder()
	o.ExternalCustomerID = secret + strings.Repeat("x", MaxExternalIDLength)

	err := o.Validate()
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaks the value: %v", err)
	}
}

func TestHasCustomer(t *testing.T) {
	t.Parallel()
	if !validOrder().HasCustomer() {
		t.Error("order with customer id should have customer")
	}
	if (CompletedOrder{}).HasCustomer() {
		t.Error("guest order should not have customer")
	}
}
