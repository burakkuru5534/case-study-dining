package customerapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/burakkuru5534/case-study-dining/internal/customerapi"
	"github.com/burakkuru5534/case-study-dining/internal/platform/testdb"
)

// ---- fixture ----

type fixture struct {
	t                 *testing.T
	db                *pgxpool.Pool
	program, program2 int64
	restaurant        int64 // reference/t1/store-1 → program
	restaurant2       int64 // reference/t2/store-2 → program2
	seq               int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, db: testdb.New(t)}
	f.program = f.id(`INSERT INTO loyalty_programs (name, earn_currency, earn_unit_minor, points_per_unit)
		VALUES ('Program A', 'TRY', 1000, 1) RETURNING id`)
	f.program2 = f.id(`INSERT INTO loyalty_programs (name, earn_currency, earn_unit_minor, points_per_unit)
		VALUES ('Program B', 'TRY', 1000, 1) RETURNING id`)
	f.restaurant = f.id(`INSERT INTO restaurants (provider, tenant_ref, external_restaurant_id, loyalty_program_id)
		VALUES ('reference', 't1', 'store-1', $1) RETURNING id`, f.program)
	f.restaurant2 = f.id(`INSERT INTO restaurants (provider, tenant_ref, external_restaurant_id, loyalty_program_id)
		VALUES ('reference', 't2', 'store-2', $1) RETURNING id`, f.program2)
	return f
}

func (f *fixture) id(sql string, args ...any) int64 {
	f.t.Helper()
	var id int64
	if err := f.db.QueryRow(context.Background(), sql, args...).Scan(&id); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
	return id
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

// customer creates a customer with one identity and returns its id.
func (f *fixture) customer(tenant, ext string) int64 {
	c := f.id(`INSERT INTO customers DEFAULT VALUES RETURNING id`)
	f.identity(c, tenant, ext)
	return c
}

func (f *fixture) identity(customer int64, tenant, ext string) {
	f.exec(`INSERT INTO customer_identities (provider, tenant_ref, external_customer_id, customer_id)
		VALUES ('reference', $1, $2, $3)`, tenant, ext, customer)
}

// order inserts an order (and, for points > 0, the ledger row and balance).
func (f *fixture) order(customer *int64, restaurant, program int64, extID string, amount int64, at time.Time, status string, points int64) {
	f.t.Helper()
	f.seq++
	ev := f.id(`INSERT INTO webhook_events (provider, tenant_ref, external_event_id, event_type, payload)
		VALUES ('reference', 't1', $1, 'order.completed', '{}') RETURNING id`, fmt.Sprintf("ev-%d", f.seq))
	orderID := f.id(`INSERT INTO orders (restaurant_id, loyalty_program_id, external_order_id, customer_id,
			eligible_amount_minor, currency, completed_at, loyalty_status, source_event_id)
		VALUES ($1, $2, $3, $4, $5, 'TRY', $6, $7, $8) RETURNING id`,
		restaurant, program, extID, customer, amount, at, status, ev)
	if points > 0 {
		f.exec(`INSERT INTO loyalty_accounts (loyalty_program_id, customer_id, balance) VALUES ($1, $2, $3)
			ON CONFLICT (loyalty_program_id, customer_id) DO UPDATE SET balance = loyalty_accounts.balance + EXCLUDED.balance`,
			program, *customer, points)
		f.exec(`INSERT INTO loyalty_transactions (loyalty_program_id, customer_id, type, points, order_id, earn_unit_minor, points_per_unit)
			VALUES ($1, $2, 'earn', $3, $4, 1000, 1)`, program, *customer, points, orderID)
	}
}

// ---- HTTP helpers ----

func (f *fixture) mux() *http.ServeMux {
	h := customerapi.NewHandler(f.db, slog.New(slog.DiscardHandler))
	m := http.NewServeMux()
	m.HandleFunc(customerapi.OrdersPattern, h.Orders)
	m.HandleFunc(customerapi.LoyaltyPattern, h.Loyalty)
	return m
}

func get(t *testing.T, m http.Handler, path string, out any) int {
	t.Helper()
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if out != nil && rec.Code == http.StatusOK {
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Fatalf("Content-Type = %q", ct)
		}
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("decode %s: %v", rec.Body, err)
		}
	}
	return rec.Code
}

type ordersPage struct {
	Orders []struct {
		RestaurantID        string    `json:"restaurant_id"`
		OrderID             string    `json:"order_id"`
		EligibleAmountMinor int64     `json:"eligible_amount_minor"`
		Currency            string    `json:"currency"`
		CompletedAt         time.Time `json:"completed_at"`
		LoyaltyStatus       string    `json:"loyalty_status"`
		PointsAwarded       int64     `json:"points_awarded"`
	} `json:"orders"`
	NextCursor *string `json:"next_cursor"`
}

const ordersPath = "/v1/providers/reference/tenants/t1/customers/cust-1/orders"

// pageAll follows next_cursor and returns the order ids in the order received.
func pageAll(t *testing.T, m http.Handler, path string, limit int) (ids []string, pages []int) {
	t.Helper()
	cursor := ""
	for range 100 {
		url := fmt.Sprintf("%s?limit=%d", path, limit)
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		var p ordersPage
		if code := get(t, m, url, &p); code != http.StatusOK {
			t.Fatalf("GET %s = %d", url, code)
		}
		pages = append(pages, len(p.Orders))
		for _, o := range p.Orders {
			ids = append(ids, o.OrderID)
		}
		if p.NextCursor == nil {
			return ids, pages
		}
		cursor = *p.NextCursor
	}
	t.Fatal("pagination did not terminate")
	return nil, nil
}

// ---- tests ----

func TestOrders_PaginationIsCompleteOrderedAndStable(t *testing.T) {
	f := newFixture(t)
	c := f.customer("t1", "cust-1")
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

	// 45 orders; groups of 5 share the same completed_at, so the id tiebreak matters.
	for i := range 45 {
		at := base.Add(time.Duration(i/5) * time.Hour)
		f.order(&c, f.restaurant, f.program, fmt.Sprintf("o-%02d", i), 1000, at, "awarded", 1)
	}
	// Expected: completed_at DESC, then insertion (id) DESC.
	var want []string
	for g := 8; g >= 0; g-- {
		for k := 4; k >= 0; k-- {
			want = append(want, fmt.Sprintf("o-%02d", g*5+k))
		}
	}

	ids, pages := pageAll(t, f.mux(), ordersPath, 20)

	if !slices.Equal(pages, []int{20, 20, 5}) {
		t.Fatalf("page sizes = %v, want [20 20 5]", pages)
	}
	if !slices.Equal(ids, want) {
		t.Fatalf("order mismatch:\n got %v\nwant %v", ids, want)
	}
}

func TestOrders_ExactMultipleOfLimitHasNoEmptyLastPage(t *testing.T) {
	f := newFixture(t)
	c := f.customer("t1", "cust-1")
	for i := range 10 {
		f.order(&c, f.restaurant, f.program, fmt.Sprintf("o-%d", i), 1000, time.Now().Add(-time.Duration(i)*time.Minute), "awarded", 1)
	}
	_, pages := pageAll(t, f.mux(), ordersPath, 5)
	if !slices.Equal(pages, []int{5, 5}) {
		t.Fatalf("pages = %v, want [5 5] (no trailing empty page)", pages)
	}
}

func TestOrders_NewOrdersBetweenPagesDoNotShiftLaterPages(t *testing.T) {
	f := newFixture(t)
	c := f.customer("t1", "cust-1")
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for i := range 20 {
		f.order(&c, f.restaurant, f.program, fmt.Sprintf("o-%02d", i), 1000, base.Add(time.Duration(i)*time.Minute), "awarded", 1)
	}
	m := f.mux()

	var p1 ordersPage
	get(t, m, ordersPath+"?limit=10", &p1)

	// A newer order arrives while the client is paging (offset paging would
	// now repeat o-10 on page 2).
	f.order(&c, f.restaurant, f.program, "o-new", 1000, base.Add(time.Hour), "awarded", 1)

	var p2 ordersPage
	get(t, m, ordersPath+"?limit=10&cursor="+*p1.NextCursor, &p2)

	var got []string
	for _, o := range append(p1.Orders, p2.Orders...) {
		got = append(got, o.OrderID)
	}
	var want []string
	for i := 19; i >= 0; i-- {
		want = append(want, fmt.Sprintf("o-%02d", i))
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}

func TestOrders_Content(t *testing.T) {
	f := newFixture(t)
	c := f.customer("t1", "cust-1")
	at := time.Date(2026, 11, 5, 9, 30, 0, 123456000, time.UTC)
	f.order(&c, f.restaurant, f.program, "awarded", 22500, at, "awarded", 22)
	f.order(&c, f.restaurant, f.program, "small", 999, at.Add(-time.Hour), "below_minimum", 0)
	f.order(&c, f.restaurant, f.program, "eur", 5000, at.Add(-2*time.Hour), "unsupported_currency", 0)
	f.order(nil, f.restaurant, f.program, "guest", 5000, at.Add(time.Hour), "no_customer", 0)

	var p ordersPage
	if code := get(t, f.mux(), ordersPath, &p); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if len(p.Orders) != 3 || p.NextCursor != nil {
		t.Fatalf("got %d orders (guest order must not appear), next=%v", len(p.Orders), p.NextCursor)
	}
	first := p.Orders[0]
	if first.OrderID != "awarded" || first.RestaurantID != "store-1" || first.EligibleAmountMinor != 22500 ||
		first.Currency != "TRY" || first.LoyaltyStatus != "awarded" || first.PointsAwarded != 22 || !first.CompletedAt.Equal(at) {
		t.Fatalf("first = %+v", first)
	}
	for _, o := range p.Orders[1:] {
		if o.PointsAwarded != 0 {
			t.Fatalf("%s: points_awarded = %d, want 0", o.OrderID, o.PointsAwarded)
		}
	}
}

func TestOrders_KnownCustomerWithoutOrders(t *testing.T) {
	f := newFixture(t)
	f.customer("t1", "cust-1")

	rec := httptest.NewRecorder()
	f.mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, ordersPath, nil))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"orders":[],"next_cursor":null}` {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
}

func TestIdentities(t *testing.T) {
	f := newFixture(t)
	c := f.customer("t1", "cust-1")
	f.identity(c, "t2", "member-9")     // same person, second identity
	other := f.customer("t2", "cust-1") // same external id in another tenant: another person
	now := time.Now()
	f.order(&c, f.restaurant, f.program, "via-t1", 1000, now, "awarded", 1)
	f.order(&c, f.restaurant2, f.program2, "via-t2", 1000, now.Add(-time.Minute), "awarded", 1)
	f.order(&other, f.restaurant2, f.program2, "other-person", 1000, now, "awarded", 1)
	m := f.mux()

	ids := func(path string) []string {
		got, _ := pageAll(t, m, path, 20)
		return got
	}
	want := []string{"via-t1", "via-t2"}
	if got := ids(ordersPath); !slices.Equal(got, want) {
		t.Fatalf("via first identity: %v", got)
	}
	if got := ids("/v1/providers/reference/tenants/t2/customers/member-9/orders"); !slices.Equal(got, want) {
		t.Fatalf("via second identity: %v", got)
	}
	if got := ids("/v1/providers/reference/tenants/t2/customers/cust-1/orders"); !slices.Equal(got, []string{"other-person"}) {
		t.Fatalf("other tenant: %v", got)
	}
}

func TestLoyalty(t *testing.T) {
	f := newFixture(t)
	c := f.customer("t1", "cust-1")
	f.order(&c, f.restaurant, f.program, "a1", 22500, time.Now(), "awarded", 22)
	f.order(&c, f.restaurant, f.program, "a2", 22500, time.Now(), "awarded", 22)
	f.order(&c, f.restaurant2, f.program2, "b1", 5000, time.Now(), "awarded", 5)
	f.customer("t1", "no-points")
	m := f.mux()

	var resp struct {
		Balances []struct {
			ProgramID   int64     `json:"program_id"`
			ProgramName string    `json:"program_name"`
			Balance     int64     `json:"balance"`
			UpdatedAt   time.Time `json:"updated_at"`
		} `json:"balances"`
	}
	if code := get(t, m, "/v1/providers/reference/tenants/t1/customers/cust-1/loyalty", &resp); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if len(resp.Balances) != 2 ||
		resp.Balances[0].ProgramID != f.program || resp.Balances[0].ProgramName != "Program A" || resp.Balances[0].Balance != 44 ||
		resp.Balances[1].ProgramID != f.program2 || resp.Balances[1].Balance != 5 || resp.Balances[0].UpdatedAt.IsZero() {
		t.Fatalf("balances = %+v", resp.Balances)
	}

	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/providers/reference/tenants/t1/customers/no-points/loyalty", nil))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"balances":[]}` {
		t.Fatalf("customer without points: %d %s", rec.Code, rec.Body)
	}
}

func TestErrors(t *testing.T) {
	f := newFixture(t)
	f.customer("t1", "cust-1")
	m := f.mux()
	long := strings.Repeat("x", 129)

	tests := []struct {
		name, path string
		want       int
		wantError  string
	}{
		{"unknown customer orders", "/v1/providers/reference/tenants/t1/customers/nobody/orders", 404, "customer_not_found"},
		{"unknown customer loyalty", "/v1/providers/reference/tenants/t1/customers/nobody/loyalty", 404, "customer_not_found"},
		{"unknown tenant", "/v1/providers/reference/tenants/t9/customers/cust-1/orders", 404, "customer_not_found"},
		{"customer id too long", "/v1/providers/reference/tenants/t1/customers/" + long + "/orders", 400, "invalid_customer"},
		{"limit zero", ordersPath + "?limit=0", 400, "invalid_limit"},
		{"limit too large", ordersPath + "?limit=101", 400, "invalid_limit"},
		{"limit not a number", ordersPath + "?limit=abc", 400, "invalid_limit"},
		{"garbage cursor", ordersPath + "?cursor=!!!", 400, "invalid_cursor"},
		{"cursor without id", ordersPath + "?cursor=eyJ0IjoiMjAyNi0xMS0wNVQwOTozMDowMFoifQ", 400, "invalid_cursor"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			m.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.path, nil))
			if rec.Code != tt.want || !strings.Contains(rec.Body.String(), `"error":"`+tt.wantError+`"`) {
				t.Fatalf("status = %d body = %s, want %d %s", rec.Code, rec.Body, tt.want, tt.wantError)
			}
		})
	}

	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, ordersPath, nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d", rec.Code)
	}
}

func TestDatabaseErrorIs503WithoutDetails(t *testing.T) {
	f := newFixture(t)
	f.db.Close()

	rec := httptest.NewRecorder()
	f.mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, ordersPath, nil))
	if rec.Code != http.StatusServiceUnavailable || strings.TrimSpace(rec.Body.String()) != `{"error":"temporarily_unavailable"}` {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
}
