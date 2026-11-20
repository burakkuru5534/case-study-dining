package ingestion_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/burakkuru5534/case-study-dining/internal/ingestion"
	"github.com/burakkuru5534/case-study-dining/internal/ingestion/reference"
	"github.com/burakkuru5534/case-study-dining/internal/platform/testdb"
)

// ---- harness ----

type catalog struct {
	program               int64
	istanbul, def, ankara int64 // restaurants "store-0042" in three tenants
}

func newCatalog(t *testing.T, db *pgxpool.Pool) catalog {
	t.Helper()
	var c catalog
	q := func(sql string, args ...any) int64 {
		var id int64
		if err := db.QueryRow(context.Background(), sql, args...).Scan(&id); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return id
	}
	c.program = q(`INSERT INTO loyalty_programs (name, earn_currency, earn_unit_minor, points_per_unit)
		VALUES ('default', 'TRY', 1000, 1) RETURNING id`)
	ins := `INSERT INTO restaurants (provider, tenant_ref, external_restaurant_id, loyalty_program_id)
		VALUES ('reference', $1, 'store-0042', $2) RETURNING id`
	c.istanbul = q(ins, "chain-istanbul", c.program)
	c.def = q(ins, "default", c.program)
	c.ankara = q(ins, "chain-ankara", c.program)
	return c
}

// payload returns the documented sample with mutations applied.
func payload(t *testing.T, mutate func(p, o map[string]any)) []byte {
	t.Helper()
	raw, err := os.ReadFile("../../docs/reference-webhook/order_completed.json")
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var p map[string]any
	if err := dec.Decode(&p); err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(p, p["order"].(map[string]any))
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// enqueue stores an event exactly as intake would and returns its row id.
func enqueue(t *testing.T, db *pgxpool.Pool, body []byte) int64 {
	t.Helper()
	ev, err := reference.Adapter{}.Parse(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := ingestion.NewInbox(db).Insert(context.Background(), ev, body); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var id int64
	if err := db.QueryRow(context.Background(), `SELECT id FROM webhook_events
		WHERE provider = $1 AND tenant_ref = $2 AND external_event_id = $3`,
		ev.Provider, ev.TenantRef, ev.ExternalEventID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func newProcessor(db *pgxpool.Pool, cfg ingestion.ProcessorConfig) *ingestion.Processor {
	return ingestion.NewProcessor(db, map[string]ingestion.Adapter{reference.ProviderName: reference.Adapter{}},
		cfg, slog.New(slog.DiscardHandler))
}

func processOne(t *testing.T, p *ingestion.Processor) {
	t.Helper()
	claimed, err := p.ProcessNext(context.Background())
	if err != nil || !claimed {
		t.Fatalf("ProcessNext: claimed=%v err=%v", claimed, err)
	}
}

type eventRow struct {
	status, lastErrorKind, lastError string
	attempts                         int
	processed                        bool
	retryInFuture                    bool
}

func event(t *testing.T, db *pgxpool.Pool, id int64) eventRow {
	t.Helper()
	var r eventRow
	if err := db.QueryRow(context.Background(), `
		SELECT status, coalesce(last_error_kind, ''), coalesce(last_error, ''), attempts,
		       processed_at IS NOT NULL, next_attempt_at > now()
		FROM webhook_events WHERE id = $1`, id).
		Scan(&r.status, &r.lastErrorKind, &r.lastError, &r.attempts, &r.processed, &r.retryInFuture); err != nil {
		t.Fatal(err)
	}
	return r
}

func scalar[T any](t *testing.T, db *pgxpool.Pool, sql string, args ...any) T {
	t.Helper()
	var v T
	if err := db.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return v
}

// ---- happy path ----

func TestProcessor_AwardsPointsInOneTransaction(t *testing.T) {
	db := testdb.New(t)
	c := newCatalog(t, db)
	id := enqueue(t, db, payload(t, nil))

	processOne(t, newProcessor(db, ingestion.ProcessorConfig{}))

	if r := event(t, db, id); r.status != "processed" || !r.processed || r.attempts != 0 {
		t.Fatalf("event = %+v", r)
	}

	var (
		restaurant, program, sourceEvent, eligible int64
		currency, status                           string
		completedAt                                time.Time
	)
	if err := db.QueryRow(context.Background(), `
		SELECT restaurant_id, loyalty_program_id, source_event_id, eligible_amount_minor, currency,
		       loyalty_status, completed_at
		FROM orders WHERE external_order_id = 'ORD-2026-000123'`).
		Scan(&restaurant, &program, &sourceEvent, &eligible, &currency, &status, &completedAt); err != nil {
		t.Fatal(err)
	}
	if restaurant != c.istanbul || program != c.program || sourceEvent != id || eligible != 22500 ||
		currency != "TRY" || status != "awarded" || !completedAt.Equal(time.Date(2026, 11, 5, 9, 30, 0, 0, time.UTC)) {
		t.Fatalf("order = r%d p%d e%d %d %s %s %s", restaurant, program, sourceEvent, eligible, currency, status, completedAt)
	}

	// Customer identity created and linked.
	customer := scalar[int64](t, db, `SELECT customer_id FROM customer_identities
		WHERE provider = 'reference' AND tenant_ref = 'chain-istanbul' AND external_customer_id = 'cust-7f3a9c'`)
	if got := scalar[int64](t, db, `SELECT customer_id FROM orders`); got != customer {
		t.Fatalf("order customer %d, identity customer %d", got, customer)
	}

	// Ledger row with rule snapshot, and the balance.
	var points, unit, ppu int64
	if err := db.QueryRow(context.Background(), `SELECT points, earn_unit_minor, points_per_unit
		FROM loyalty_transactions WHERE customer_id = $1 AND type = 'earn'`, customer).Scan(&points, &unit, &ppu); err != nil {
		t.Fatal(err)
	}
	if points != 22 || unit != 1000 || ppu != 1 {
		t.Fatalf("ledger = %d points, rule %d/%d", points, unit, ppu)
	}
	if b := scalar[int64](t, db, `SELECT balance FROM loyalty_accounts WHERE customer_id = $1 AND loyalty_program_id = $2`,
		customer, c.program); b != 22 {
		t.Fatalf("balance = %d", b)
	}
}

func TestProcessor_LoyaltyOutcomes(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(p, o map[string]any)
		wantStatus string
		wantPoints int64 // 0 means no ledger row and no account
	}{
		{"exactly 10 TRY earns 1", func(_, o map[string]any) {
			o["subtotal_minor"], o["discount_minor"] = json.Number("1000"), json.Number("0")
		}, "awarded", 1},
		{"below minimum", func(_, o map[string]any) {
			o["subtotal_minor"], o["discount_minor"] = json.Number("999"), json.Number("0")
		}, "below_minimum", 0},
		{"guest", func(_, o map[string]any) { delete(o, "customer_id") }, "no_customer", 0},
		{"unsupported currency is stored without points", func(_, o map[string]any) { o["currency"] = "EUR" }, "unsupported_currency", 0},
		{"guest beats unsupported currency", func(_, o map[string]any) {
			delete(o, "customer_id")
			o["currency"] = "EUR"
		}, "no_customer", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := testdb.New(t)
			newCatalog(t, db)
			id := enqueue(t, db, payload(t, tt.mutate))

			processOne(t, newProcessor(db, ingestion.ProcessorConfig{}))

			if r := event(t, db, id); r.status != "processed" {
				t.Fatalf("event = %+v", r)
			}
			if s := scalar[string](t, db, `SELECT loyalty_status FROM orders`); s != tt.wantStatus {
				t.Fatalf("loyalty_status = %s, want %s", s, tt.wantStatus)
			}
			sum := scalar[int64](t, db, `SELECT coalesce(sum(points), 0) FROM loyalty_transactions`)
			accounts := scalar[int64](t, db, `SELECT count(*) FROM loyalty_accounts`)
			if sum != tt.wantPoints || (tt.wantPoints == 0) != (accounts == 0) {
				t.Fatalf("ledger sum = %d, accounts = %d, want points %d", sum, accounts, tt.wantPoints)
			}
		})
	}
}

func TestProcessor_GuestOrderCreatesNoCustomer(t *testing.T) {
	db := testdb.New(t)
	newCatalog(t, db)
	enqueue(t, db, payload(t, func(_, o map[string]any) { o["customer_id"] = nil }))

	processOne(t, newProcessor(db, ingestion.ProcessorConfig{}))

	if n := scalar[int64](t, db, `SELECT count(*) FROM customers`); n != 0 {
		t.Fatalf("customers = %d", n)
	}
}

// ---- duplicates and identity ----

func TestProcessor_SameOrderUnderNewEventIsNotAwardedTwice(t *testing.T) {
	db := testdb.New(t)
	newCatalog(t, db)
	first := enqueue(t, db, payload(t, nil))
	second := enqueue(t, db, payload(t, func(p, _ map[string]any) { p["event_id"] = "evt-redelivered" }))

	p := newProcessor(db, ingestion.ProcessorConfig{})
	processOne(t, p)
	processOne(t, p)

	for _, id := range []int64{first, second} {
		if r := event(t, db, id); r.status != "processed" {
			t.Fatalf("event %d = %+v", id, r)
		}
	}
	if n := scalar[int64](t, db, `SELECT count(*) FROM orders`); n != 1 {
		t.Fatalf("orders = %d", n)
	}
	if src := scalar[int64](t, db, `SELECT source_event_id FROM orders`); src != first {
		t.Fatalf("source_event_id = %d, want first event %d", src, first)
	}
	if b := scalar[int64](t, db, `SELECT balance FROM loyalty_accounts`); b != 22 {
		t.Fatalf("balance = %d, want 22", b)
	}
}

func TestProcessor_ReturningCustomerAccumulates(t *testing.T) {
	db := testdb.New(t)
	newCatalog(t, db)
	enqueue(t, db, payload(t, nil))
	enqueue(t, db, payload(t, func(p, o map[string]any) { p["event_id"] = "evt-2"; o["order_id"] = "ORD-2" }))

	p := newProcessor(db, ingestion.ProcessorConfig{})
	processOne(t, p)
	processOne(t, p)

	if n := scalar[int64](t, db, `SELECT count(*) FROM customers`); n != 1 {
		t.Fatalf("customers = %d, identity must be reused", n)
	}
	if b := scalar[int64](t, db, `SELECT balance FROM loyalty_accounts`); b != 44 {
		t.Fatalf("balance = %d, want 44", b)
	}
	if n := scalar[int64](t, db, `SELECT count(*) FROM loyalty_transactions`); n != 2 {
		t.Fatalf("ledger rows = %d", n)
	}
}

func TestProcessor_SameExternalCustomerInOtherTenantIsAnotherCustomer(t *testing.T) {
	db := testdb.New(t)
	newCatalog(t, db)
	enqueue(t, db, payload(t, nil))
	enqueue(t, db, payload(t, func(p, _ map[string]any) { p["tenant_ref"] = "chain-ankara" }))

	p := newProcessor(db, ingestion.ProcessorConfig{})
	processOne(t, p)
	processOne(t, p)

	if n := scalar[int64](t, db, `SELECT count(DISTINCT customer_id) FROM customer_identities
		WHERE external_customer_id = 'cust-7f3a9c'`); n != 2 {
		t.Fatalf("distinct customers = %d, want 2", n)
	}
	// Same order number, different restaurant (tenant): both stored.
	if n := scalar[int64](t, db, `SELECT count(*) FROM orders`); n != 2 {
		t.Fatalf("orders = %d", n)
	}
}

// ---- failures ----

func TestProcessor_PermanentFailures(t *testing.T) {
	tests := []struct {
		name      string
		insert    func(t *testing.T, db *pgxpool.Pool) int64
		wantError string
	}{
		{
			name: "unknown restaurant",
			insert: func(t *testing.T, db *pgxpool.Pool) int64 {
				return enqueue(t, db, payload(t, func(_, o map[string]any) { o["restaurant_id"] = "store-9999" }))
			},
			wantError: "unknown restaurant",
		},
		{
			name: "stored payload no longer valid for the adapter",
			insert: func(t *testing.T, db *pgxpool.Pool) int64 {
				return scalar[int64](t, db, `INSERT INTO webhook_events (provider, tenant_ref, external_event_id, event_type, payload)
					VALUES ('reference', 'default', 'evt-bad', 'order.completed', '{"event_id":"evt-bad","event_type":"order.completed"}')
					RETURNING id`)
			},
			wantError: "invalid payload",
		},
		{
			name: "unknown provider",
			insert: func(t *testing.T, db *pgxpool.Pool) int64 {
				return scalar[int64](t, db, `INSERT INTO webhook_events (provider, tenant_ref, external_event_id, event_type, payload)
					VALUES ('acme-pos', 'default', 'evt-x', 'order.completed', '{}') RETURNING id`)
			},
			wantError: `unknown provider "acme-pos"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := testdb.New(t)
			newCatalog(t, db)
			id := tt.insert(t, db)

			processOne(t, newProcessor(db, ingestion.ProcessorConfig{}))

			r := event(t, db, id)
			if r.status != "failed" || r.lastErrorKind != "permanent" || r.attempts != 1 || !strings.Contains(r.lastError, tt.wantError) {
				t.Fatalf("event = %+v", r)
			}
			if n := scalar[int64](t, db, `SELECT count(*) FROM orders`); n != 0 {
				t.Fatalf("orders = %d, want 0", n)
			}
			// A failed event is never claimed again.
			if claimed, err := newProcessor(db, ingestion.ProcessorConfig{}).ProcessNext(context.Background()); claimed || err != nil {
				t.Fatalf("claimed=%v err=%v", claimed, err)
			}
		})
	}
}

// A real transient failure: another transaction holds the restaurant row, the
// processor's FOR SHARE hits lock_timeout. The event is retried later with
// backoff, and succeeds once the lock is gone.
func TestProcessor_TransientFailureIsRetriedWithBackoff(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	c := newCatalog(t, db)
	id := enqueue(t, db, payload(t, nil))

	blocker, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(ctx) }()
	if _, err := blocker.Exec(ctx, `SELECT 1 FROM restaurants WHERE id = $1 FOR UPDATE`, c.istanbul); err != nil {
		t.Fatal(err)
	}

	p := newProcessor(db, ingestion.ProcessorConfig{LockTimeout: 100 * time.Millisecond})
	processOne(t, p)

	r := event(t, db, id)
	if r.status != "pending" || r.lastErrorKind != "transient" || r.attempts != 1 || !r.retryInFuture ||
		!strings.Contains(r.lastError, "lock timeout") {
		t.Fatalf("after lock timeout: %+v", r)
	}
	if n := scalar[int64](t, db, `SELECT count(*) FROM orders`); n != 0 {
		t.Fatalf("partial writes survived: orders = %d", n)
	}
	// Not due yet: nothing to claim.
	if claimed, err := p.ProcessNext(ctx); claimed || err != nil {
		t.Fatalf("claimed a not-yet-due event: claimed=%v err=%v", claimed, err)
	}

	_ = blocker.Rollback(ctx)
	if _, err := db.Exec(ctx, `UPDATE webhook_events SET next_attempt_at = now() WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	processOne(t, p)

	if r := event(t, db, id); r.status != "processed" || r.attempts != 1 {
		t.Fatalf("after retry: %+v", r)
	}
	if b := scalar[int64](t, db, `SELECT balance FROM loyalty_accounts`); b != 22 {
		t.Fatalf("balance = %d", b)
	}
}

func TestProcessor_TransientRetriesExhausted(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	c := newCatalog(t, db)
	id := enqueue(t, db, payload(t, nil))
	if _, err := db.Exec(ctx, `UPDATE webhook_events SET attempts = 2 WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}

	blocker, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(ctx) }()
	if _, err := blocker.Exec(ctx, `SELECT 1 FROM restaurants WHERE id = $1 FOR UPDATE`, c.istanbul); err != nil {
		t.Fatal(err)
	}

	processOne(t, newProcessor(db, ingestion.ProcessorConfig{MaxAttempts: 3, LockTimeout: 100 * time.Millisecond}))

	if r := event(t, db, id); r.status != "failed" || r.lastErrorKind != "transient" || r.attempts != 3 {
		t.Fatalf("event = %+v", r)
	}
}

func TestProcessor_EmptyQueue(t *testing.T) {
	db := testdb.New(t)
	claimed, err := newProcessor(db, ingestion.ProcessorConfig{}).ProcessNext(context.Background())
	if claimed || err != nil {
		t.Fatalf("claimed=%v err=%v", claimed, err)
	}
}

// Cancellation (shutdown) mid-processing must leave the event exactly as it
// was: no attempt counted, no error recorded, nothing written.
func TestProcessor_CancellationLeavesEventUntouched(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	c := newCatalog(t, db)
	id := enqueue(t, db, payload(t, nil))

	blocker, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(ctx) }()
	if _, err := blocker.Exec(ctx, `SELECT 1 FROM restaurants WHERE id = $1 FOR UPDATE`, c.istanbul); err != nil {
		t.Fatal(err)
	}

	cctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	// Long lock timeout: the wait is ended by the context, not by lock_timeout.
	_, err = newProcessor(db, ingestion.ProcessorConfig{LockTimeout: time.Minute}).ProcessNext(cctx)
	// Exactly the context error, unwrapped, so the worker can tell shutdown
	// apart from a real failure.
	if err != context.DeadlineExceeded {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}

	if r := event(t, db, id); r.status != "pending" || r.attempts != 0 || r.lastError != "" || r.retryInFuture {
		t.Fatalf("event changed by a cancelled attempt: %+v", r)
	}
	_ = blocker.Rollback(ctx)
	if n := scalar[int64](t, db, `SELECT count(*) FROM orders`); n != 0 {
		t.Fatalf("orders = %d", n)
	}
}

// ---- concurrency ----

// Many processors drain a queue that contains duplicate orders and customers
// created concurrently. Every event must be processed exactly once, every
// order stored once, and each balance must equal its ledger sum and the
// independently computed expectation.
func TestProcessor_ConcurrentProcessorsProcessEachEventOnce(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	newCatalog(t, db)

	const customers, ordersPerCustomer = 5, 10
	expected := map[string]int64{}
	events := 0
	for c := range customers {
		cust := fmt.Sprintf("cust-%d", c)
		for o := range ordersPerCustomer {
			amount := int64(500 + 1000*o + 37*c) // includes below-minimum orders
			orderID := fmt.Sprintf("ORD-%d-%d", c, o)
			mk := func(eventID string) []byte {
				return payload(t, func(p, ord map[string]any) {
					p["event_id"] = eventID
					ord["order_id"] = orderID
					ord["customer_id"] = cust
					ord["subtotal_minor"] = json.Number(fmt.Sprint(amount))
					ord["discount_minor"] = json.Number("0")
				})
			}
			enqueue(t, db, mk(fmt.Sprintf("evt-%d-%d", c, o)))
			events++
			if o%5 == 0 { // the same order redelivered under a new event id
				enqueue(t, db, mk(fmt.Sprintf("evt-%d-%d-redelivery", c, o)))
				events++
			}
			expected[cust] += amount / 1000
		}
	}

	// Tiny backoff so identity-race retries become due again quickly.
	p := newProcessor(db, ingestion.ProcessorConfig{Backoff: ingestion.Backoff{Base: time.Millisecond, Max: 5 * time.Millisecond}})

	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for wctx.Err() == nil {
				claimed, err := p.ProcessNext(wctx)
				if err != nil {
					errs <- err
					return
				}
				if !claimed {
					var pending int
					if err := db.QueryRow(wctx, `SELECT count(*) FROM webhook_events WHERE status = 'pending'`).Scan(&pending); err != nil || pending == 0 {
						return
					}
					time.Sleep(2 * time.Millisecond) // pending but not yet due
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("processor error: %v", err)
	}

	if n := scalar[int64](t, db, `SELECT count(*) FROM webhook_events WHERE status = 'processed'`); n != int64(events) {
		t.Fatalf("processed = %d, want %d", n, events)
	}
	if n := scalar[int64](t, db, `SELECT count(*) FROM orders`); n != customers*ordersPerCustomer {
		t.Fatalf("orders = %d", n)
	}
	if n := scalar[int64](t, db, `SELECT count(*) FROM customers`); n != customers {
		t.Fatalf("customers = %d, want %d (no orphans from identity races)", n, customers)
	}
	rows, err := db.Query(ctx, `
		SELECT ci.external_customer_id, a.balance,
		       (SELECT coalesce(sum(points), 0) FROM loyalty_transactions lt
		        WHERE lt.customer_id = a.customer_id AND lt.loyalty_program_id = a.loyalty_program_id)
		FROM loyalty_accounts a JOIN customer_identities ci ON ci.customer_id = a.customer_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var cust string
		var balance, ledger int64
		if err := rows.Scan(&cust, &balance, &ledger); err != nil {
			t.Fatal(err)
		}
		seen++
		if balance != ledger || balance != expected[cust] {
			t.Errorf("%s: balance %d, ledger %d, expected %d", cust, balance, ledger, expected[cust])
		}
	}
	if seen != customers {
		t.Fatalf("accounts = %d", seen)
	}
}
