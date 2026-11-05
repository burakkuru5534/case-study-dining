package migrations_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/burakkuru5534/case-study-dining/internal/platform/migrate"
	"github.com/burakkuru5534/case-study-dining/migrations"
)

// SQLSTATE codes asserted by these tests.
const (
	uniqueViolation     = "23505"
	checkViolation      = "23514"
	foreignKeyViolation = "23503"
	restrictViolation   = "23001"
)

// ---- harness ----

// newSchemaDB applies the real embedded migrations to a fresh, uniquely named
// schema and returns a pool scoped to it via search_path.
func newSchemaDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	ctx := context.Background()

	b := make([]byte, 6)
	_, _ = rand.Read(b)
	schema := "schema_test_" + hex.EncodeToString(b)

	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		_ = admin.Close(context.Background())
	})

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema

	all, err := migrate.Load(migrations.FS)
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	mconn, err := pgx.ConnectConfig(ctx, cfg.ConnConfig)
	if err != nil {
		t.Fatalf("connect for migrate: %v", err)
	}
	if _, err := migrate.Run(ctx, mconn, all, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	_ = mconn.Close(ctx)

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func expectCode(t *testing.T, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected SQLSTATE %s, got %v", code, err)
	}
	if pgErr.Code != code {
		t.Fatalf("expected SQLSTATE %s, got %s: %s", code, pgErr.Code, pgErr.Message)
	}
}

func mustExec(t *testing.T, db *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := db.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func mustID(t *testing.T, db *pgxpool.Pool, sql string, args ...any) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(context.Background(), sql, args...).Scan(&id); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return id
}

func count(t *testing.T, db *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

// ---- fixtures ----

type fixture struct {
	program, restaurant, customer int64
}

func newProgram(t *testing.T, db *pgxpool.Pool) int64 {
	return mustID(t, db, `INSERT INTO loyalty_programs (name, earn_currency, earn_unit_minor, points_per_unit)
		VALUES ('default', 'TRY', 1000, 1) RETURNING id`)
}

func newRestaurant(t *testing.T, db *pgxpool.Pool, program int64, extID string) int64 {
	return mustID(t, db, `INSERT INTO restaurants (provider, tenant_ref, external_restaurant_id, loyalty_program_id)
		VALUES ('acme', 't1', $1, $2) RETURNING id`, extID, program)
}

func newEvent(t *testing.T, db *pgxpool.Pool, extID string) int64 {
	return mustID(t, db, `INSERT INTO webhook_events (provider, tenant_ref, external_event_id, event_type, payload)
		VALUES ('acme', 't1', $1, 'order.completed', '{}') RETURNING id`, extID)
}

func uniq() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func setup(t *testing.T, db *pgxpool.Pool) fixture {
	t.Helper()
	p := newProgram(t, db)
	return fixture{
		program:    p,
		restaurant: newRestaurant(t, db, p, "r-"+uniq()),
		customer:   mustID(t, db, `INSERT INTO customers DEFAULT VALUES RETURNING id`),
	}
}

// insertOrder inserts an awarded order for f.customer and returns its id.
func insertOrder(t *testing.T, db *pgxpool.Pool, f fixture, extOrderID string) int64 {
	t.Helper()
	ev := newEvent(t, db, "ev-"+extOrderID)
	return mustID(t, db, `INSERT INTO orders (restaurant_id, loyalty_program_id, external_order_id, customer_id,
			eligible_amount_minor, currency, completed_at, loyalty_status, source_event_id)
		VALUES ($1, $2, $3, $4, 12345, 'TRY', now(), 'awarded', $5) RETURNING id`,
		f.restaurant, f.program, extOrderID, f.customer, ev)
}

func ensureAccount(t *testing.T, db *pgxpool.Pool, f fixture) {
	t.Helper()
	mustExec(t, db, `INSERT INTO loyalty_accounts (loyalty_program_id, customer_id) VALUES ($1, $2)
		ON CONFLICT DO NOTHING`, f.program, f.customer)
}

const insertEarn = `INSERT INTO loyalty_transactions
	(loyalty_program_id, customer_id, type, points, order_id, earn_unit_minor, points_per_unit)
	VALUES ($1, $2, 'earn', $3, $4, 1000, 1)`

// ---- identity and tenant scoping ----

func TestSchema_WebhookEventDedupIsTenantScoped(t *testing.T) {
	db := newSchemaDB(t)
	ctx := context.Background()
	ins := `INSERT INTO webhook_events (provider, tenant_ref, external_event_id, event_type, payload)
		VALUES ($1, $2, $3, 'order.completed', '{}')`

	mustExec(t, db, ins, "acme", "t1", "ev-1")

	_, err := db.Exec(ctx, ins, "acme", "t1", "ev-1")
	expectCode(t, err, uniqueViolation)

	// Same event ID under another tenant or provider is a different event.
	mustExec(t, db, ins, "acme", "t2", "ev-1")
	mustExec(t, db, ins, "other", "t1", "ev-1")

	// The intake path: a duplicate is a silent no-op.
	tag, err := db.Exec(ctx, ins+" ON CONFLICT (provider, tenant_ref, external_event_id) DO NOTHING", "acme", "t1", "ev-1")
	if err != nil || tag.RowsAffected() != 0 {
		t.Fatalf("ON CONFLICT DO NOTHING: rows=%d err=%v", tag.RowsAffected(), err)
	}
}

func TestSchema_RestaurantIdentityIsTenantScoped(t *testing.T) {
	db := newSchemaDB(t)
	p := newProgram(t, db)
	ins := `INSERT INTO restaurants (provider, tenant_ref, external_restaurant_id, loyalty_program_id) VALUES ($1, $2, $3, $4)`

	mustExec(t, db, ins, "acme", "t1", "r-1", p)
	_, err := db.Exec(context.Background(), ins, "acme", "t1", "r-1", p)
	expectCode(t, err, uniqueViolation)
	mustExec(t, db, ins, "acme", "t2", "r-1", p)
}

func TestSchema_CustomerIdentityIsTenantScoped(t *testing.T) {
	db := newSchemaDB(t)
	c1 := mustID(t, db, `INSERT INTO customers DEFAULT VALUES RETURNING id`)
	c2 := mustID(t, db, `INSERT INTO customers DEFAULT VALUES RETURNING id`)
	ins := `INSERT INTO customer_identities (provider, tenant_ref, external_customer_id, customer_id) VALUES ($1, $2, $3, $4)`

	mustExec(t, db, ins, "acme", "t1", "cust-42", c1)

	// Same external ID in the same tenant cannot map to another customer.
	_, err := db.Exec(context.Background(), ins, "acme", "t1", "cust-42", c2)
	expectCode(t, err, uniqueViolation)

	// Same external ID in another tenant is a different person.
	mustExec(t, db, ins, "acme", "t2", "cust-42", c2)

	// Two identities can link to one internal customer (cross-restaurant loyalty).
	mustExec(t, db, ins, "other", "t9", "xyz", c1)
	if n := count(t, db, `SELECT count(*) FROM customer_identities WHERE customer_id = $1`, c1); n != 2 {
		t.Fatalf("identities for c1 = %d, want 2", n)
	}
}

// ---- webhook event state machine ----

func TestSchema_WebhookEventStatusConstraints(t *testing.T) {
	db := newSchemaDB(t)

	tests := []struct {
		name    string
		set     string
		wantErr string
	}{
		{"transient retry stays pending", `attempts = 1, next_attempt_at = now() + interval '1 minute', last_error = 'timeout', last_error_kind = 'transient'`, ""},
		{"processed", `status = 'processed', processed_at = now()`, ""},
		{"permanent failure", `status = 'failed', last_error = 'bad payload', last_error_kind = 'permanent'`, ""},
		{"retries exhausted", `status = 'failed', last_error = 'timeout', last_error_kind = 'transient'`, ""},
		{"processed without processed_at", `status = 'processed'`, checkViolation},
		{"processed_at while pending", `processed_at = now()`, checkViolation},
		{"failed without error", `status = 'failed'`, checkViolation},
		{"error without kind", `last_error = 'x'`, checkViolation},
		{"kind without error", `last_error_kind = 'transient'`, checkViolation},
		{"permanent error left pending", `last_error = 'x', last_error_kind = 'permanent'`, checkViolation},
		{"unknown status", `status = 'processing'`, checkViolation},
		{"unknown error kind", `last_error = 'x', last_error_kind = 'fatal'`, checkViolation},
		{"negative attempts", `attempts = -1`, checkViolation},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id := newEvent(t, db, "ev-status-"+string(rune('a'+i)))
			_, err := db.Exec(context.Background(), "UPDATE webhook_events SET "+tt.set+" WHERE id = $1", id)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			expectCode(t, err, tt.wantErr)
		})
	}
}

// ---- orders ----

func TestSchema_OrderConstraints(t *testing.T) {
	db := newSchemaDB(t)
	ctx := context.Background()
	f := setup(t, db)
	otherRestaurant := newRestaurant(t, db, f.program, "r-2")
	otherProgram := newProgram(t, db)
	ev := newEvent(t, db, "ev-orders")

	ins := `INSERT INTO orders (restaurant_id, loyalty_program_id, external_order_id, customer_id,
			eligible_amount_minor, currency, completed_at, loyalty_status, source_event_id)
		VALUES ($1, $2, $3, $4, $5, $6, now(), $7, $8)`

	mustExec(t, db, ins, f.restaurant, f.program, "o-1", f.customer, 12345, "TRY", "awarded", ev)

	t.Run("same order in same restaurant is rejected", func(t *testing.T) {
		_, err := db.Exec(ctx, ins, f.restaurant, f.program, "o-1", f.customer, 12345, "TRY", "awarded", ev)
		expectCode(t, err, uniqueViolation)
	})
	t.Run("same order number in another restaurant is allowed", func(t *testing.T) {
		mustExec(t, db, ins, otherRestaurant, f.program, "o-1", f.customer, 500, "TRY", "below_minimum", ev)
	})
	t.Run("program must match the restaurant's program", func(t *testing.T) {
		_, err := db.Exec(ctx, ins, f.restaurant, otherProgram, "o-2", f.customer, 12345, "TRY", "awarded", ev)
		expectCode(t, err, checkViolation)
	})
	t.Run("guest order must be no_customer", func(t *testing.T) {
		_, err := db.Exec(ctx, ins, f.restaurant, f.program, "o-3", nil, 12345, "TRY", "awarded", ev)
		expectCode(t, err, checkViolation)
		mustExec(t, db, ins, f.restaurant, f.program, "o-3", nil, 12345, "TRY", "no_customer", ev)
	})
	t.Run("no_customer requires a guest order", func(t *testing.T) {
		_, err := db.Exec(ctx, ins, f.restaurant, f.program, "o-4", f.customer, 12345, "TRY", "no_customer", ev)
		expectCode(t, err, checkViolation)
	})
	t.Run("unsupported currency is stored", func(t *testing.T) {
		mustExec(t, db, ins, f.restaurant, f.program, "o-5", f.customer, 12345, "EUR", "unsupported_currency", ev)
	})
	t.Run("invalid currency format", func(t *testing.T) {
		_, err := db.Exec(ctx, ins, f.restaurant, f.program, "o-6", f.customer, 12345, "try", "awarded", ev)
		expectCode(t, err, checkViolation)
	})
	t.Run("negative amount", func(t *testing.T) {
		_, err := db.Exec(ctx, ins, f.restaurant, f.program, "o-7", f.customer, -1, "TRY", "awarded", ev)
		expectCode(t, err, checkViolation)
	})
	t.Run("unknown loyalty status", func(t *testing.T) {
		_, err := db.Exec(ctx, ins, f.restaurant, f.program, "o-8", f.customer, 12345, "TRY", "pending", ev)
		expectCode(t, err, checkViolation)
	})
}

func TestSchema_RestaurantProgramChangeKeepsOrderHistory(t *testing.T) {
	db := newSchemaDB(t)
	ctx := context.Background()
	f := setup(t, db)
	order := insertOrder(t, db, f, "o-1")

	newProg := newProgram(t, db)
	mustExec(t, db, `UPDATE restaurants SET loyalty_program_id = $1 WHERE id = $2`, newProg, f.restaurant)

	var snapshot int64
	if err := db.QueryRow(ctx, `SELECT loyalty_program_id FROM orders WHERE id = $1`, order).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot != f.program {
		t.Fatalf("existing order program changed to %d, want %d", snapshot, f.program)
	}

	// New orders must now use the new program.
	ev := newEvent(t, db, "ev-after-change")
	_, err := db.Exec(ctx, `INSERT INTO orders (restaurant_id, loyalty_program_id, external_order_id, customer_id,
			eligible_amount_minor, currency, completed_at, loyalty_status, source_event_id)
		VALUES ($1, $2, 'o-2', $3, 12345, 'TRY', now(), 'awarded', $4)`, f.restaurant, f.program, f.customer, ev)
	expectCode(t, err, checkViolation)
}

// ---- ledger and balance ----

func TestSchema_LedgerConstraints(t *testing.T) {
	db := newSchemaDB(t)
	ctx := context.Background()
	f := setup(t, db)
	ensureAccount(t, db, f)
	order := insertOrder(t, db, f, "o-1")

	mustExec(t, db, insertEarn, f.program, f.customer, 12, order)

	t.Run("second earn for the same order is rejected", func(t *testing.T) {
		_, err := db.Exec(ctx, insertEarn, f.program, f.customer, 12, order)
		expectCode(t, err, uniqueViolation)
	})
	t.Run("earn requires an order", func(t *testing.T) {
		_, err := db.Exec(ctx, insertEarn, f.program, f.customer, 12, nil)
		expectCode(t, err, checkViolation)
	})
	t.Run("earn requires the rule snapshot", func(t *testing.T) {
		o := insertOrder(t, db, f, "o-rule")
		_, err := db.Exec(ctx, `INSERT INTO loyalty_transactions (loyalty_program_id, customer_id, type, points, order_id)
			VALUES ($1, $2, 'earn', 12, $3)`, f.program, f.customer, o)
		expectCode(t, err, checkViolation)
	})
	t.Run("points must be positive", func(t *testing.T) {
		o := insertOrder(t, db, f, "o-zero")
		_, err := db.Exec(ctx, insertEarn, f.program, f.customer, 0, o)
		expectCode(t, err, checkViolation)
	})
	t.Run("ledger program must match the order's program", func(t *testing.T) {
		other := newProgram(t, db)
		mustExec(t, db, `INSERT INTO loyalty_accounts (loyalty_program_id, customer_id) VALUES ($1, $2)`, other, f.customer)
		o := insertOrder(t, db, f, "o-prog")
		_, err := db.Exec(ctx, insertEarn, other, f.customer, 12, o)
		expectCode(t, err, foreignKeyViolation)
	})
	t.Run("ledger customer must match the order's customer", func(t *testing.T) {
		stranger := mustID(t, db, `INSERT INTO customers DEFAULT VALUES RETURNING id`)
		mustExec(t, db, `INSERT INTO loyalty_accounts (loyalty_program_id, customer_id) VALUES ($1, $2)`, f.program, stranger)
		o := insertOrder(t, db, f, "o-cust")
		_, err := db.Exec(ctx, insertEarn, f.program, stranger, 12, o)
		expectCode(t, err, foreignKeyViolation)
	})
	t.Run("ledger requires an account", func(t *testing.T) {
		g := setup(t, db) // new program, restaurant and customer without an account
		o := insertOrder(t, db, g, "o-noacct")
		_, err := db.Exec(ctx, insertEarn, g.program, g.customer, 12, o)
		expectCode(t, err, foreignKeyViolation)
	})
	t.Run("balance cannot go negative", func(t *testing.T) {
		_, err := db.Exec(ctx, `UPDATE loyalty_accounts SET balance = -1 WHERE loyalty_program_id = $1 AND customer_id = $2`,
			f.program, f.customer)
		expectCode(t, err, checkViolation)
	})
}

func TestSchema_LedgerIsAppendOnly(t *testing.T) {
	db := newSchemaDB(t)
	ctx := context.Background()
	f := setup(t, db)
	ensureAccount(t, db, f)
	order := insertOrder(t, db, f, "o-1")
	mustExec(t, db, insertEarn, f.program, f.customer, 12, order)

	_, err := db.Exec(ctx, `UPDATE loyalty_transactions SET points = 999`)
	expectCode(t, err, restrictViolation)

	_, err = db.Exec(ctx, `DELETE FROM loyalty_transactions`)
	expectCode(t, err, restrictViolation)

	_, err = db.Exec(ctx, `TRUNCATE loyalty_transactions`)
	expectCode(t, err, restrictViolation)

	if n := count(t, db, `SELECT count(*) FROM loyalty_transactions`); n != 1 {
		t.Fatalf("ledger rows = %d, want 1", n)
	}
}

// ---- concurrency ----

// Two transactions race to insert an earn for the same order: exactly one wins.
func TestSchema_ConcurrentEarnForSameOrder(t *testing.T) {
	db := newSchemaDB(t)
	f := setup(t, db)
	ensureAccount(t, db, f)
	order := insertOrder(t, db, f, "o-1")

	const racers = 8
	var (
		wg    sync.WaitGroup
		start = make(chan struct{})
		mu    sync.Mutex
		ok    int
		dup   int
		other []error
	)
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := db.Exec(context.Background(), insertEarn, f.program, f.customer, 12, order)
			mu.Lock()
			defer mu.Unlock()
			var pgErr *pgconn.PgError
			switch {
			case err == nil:
				ok++
			case errors.As(err, &pgErr) && pgErr.Code == uniqueViolation:
				dup++
			default:
				other = append(other, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(other) > 0 || ok != 1 || dup != racers-1 {
		t.Fatalf("ok=%d dup=%d other=%v; want ok=1 dup=%d", ok, dup, other, racers-1)
	}
}

// Workers claim events with FOR UPDATE SKIP LOCKED and process each in one
// transaction. Two different events carry the same order; with many workers
// running concurrently, the order must be stored and awarded exactly once.
//
// processOne is a minimal stand-in for the real worker (Task 8), written only
// to prove the schema's guarantees under concurrency.
func TestSchema_ConcurrentWorkersAwardOrderOnce(t *testing.T) {
	db := newSchemaDB(t)
	ctx := context.Background()
	f := setup(t, db)

	const payload = `{"external_order_id": "o-dup", "eligible_amount_minor": 12345}`
	for _, ev := range []string{"ev-a", "ev-b", "ev-c"} {
		mustExec(t, db, `INSERT INTO webhook_events (provider, tenant_ref, external_event_id, event_type, payload)
			VALUES ('acme', 't1', $1, 'order.completed', $2)`, ev, payload)
	}

	const workers = 6
	var (
		wg     sync.WaitGroup
		start  = make(chan struct{})
		errsMu sync.Mutex
		errs   []error
	)
	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for {
				done, err := processOne(wctx, db, f)
				if err != nil {
					errsMu.Lock()
					errs = append(errs, err)
					errsMu.Unlock()
					return
				}
				if done {
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("worker errors: %v", errs)
	}
	if n := count(t, db, `SELECT count(*) FROM webhook_events WHERE status = 'processed'`); n != 3 {
		t.Fatalf("processed events = %d, want 3", n)
	}
	if n := count(t, db, `SELECT count(*) FROM orders`); n != 1 {
		t.Fatalf("orders = %d, want 1", n)
	}
	if n := count(t, db, `SELECT count(*) FROM loyalty_transactions`); n != 1 {
		t.Fatalf("ledger rows = %d, want 1", n)
	}
	var balance, ledgerSum int64
	if err := db.QueryRow(ctx, `SELECT a.balance, (SELECT sum(points) FROM loyalty_transactions)
		FROM loyalty_accounts a WHERE a.loyalty_program_id = $1 AND a.customer_id = $2`,
		f.program, f.customer).Scan(&balance, &ledgerSum); err != nil {
		t.Fatal(err)
	}
	if balance != 12 || ledgerSum != 12 {
		t.Fatalf("balance=%d ledgerSum=%d, want 12/12", balance, ledgerSum)
	}
}

// processOne claims one due event and processes it in a single transaction.
// It returns done=true when no event is available.
func processOne(ctx context.Context, db *pgxpool.Pool, f fixture) (done bool, err error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var (
		eventID    int64
		extOrderID string
		amount     int64
	)
	err = tx.QueryRow(ctx, `
		SELECT id, payload->>'external_order_id', (payload->>'eligible_amount_minor')::bigint
		FROM webhook_events
		WHERE status = 'pending' AND next_attempt_at <= now()
		ORDER BY next_attempt_at, id
		LIMIT 1
		FOR UPDATE SKIP LOCKED`).Scan(&eventID, &extOrderID, &amount)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}

	// A concurrent insert of the same order blocks on the unique index until the
	// other transaction commits, then DO NOTHING returns no row.
	var orderID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO orders (restaurant_id, loyalty_program_id, external_order_id, customer_id,
			eligible_amount_minor, currency, completed_at, loyalty_status, source_event_id)
		VALUES ($1, $2, $3, $4, $5, 'TRY', now(), 'awarded', $6)
		ON CONFLICT (restaurant_id, external_order_id) DO NOTHING
		RETURNING id`, f.restaurant, f.program, extOrderID, f.customer, amount, eventID).Scan(&orderID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Duplicate order: acknowledge the event, award nothing.
	case err != nil:
		return false, err
	default:
		points := amount / 1000 * 1
		if _, err := tx.Exec(ctx, `
			INSERT INTO loyalty_accounts (loyalty_program_id, customer_id, balance) VALUES ($1, $2, $3)
			ON CONFLICT (loyalty_program_id, customer_id)
			DO UPDATE SET balance = loyalty_accounts.balance + EXCLUDED.balance, updated_at = now()`,
			f.program, f.customer, points); err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx, insertEarn, f.program, f.customer, points, orderID); err != nil {
			return false, err
		}
	}

	if _, err := tx.Exec(ctx, `UPDATE webhook_events SET status = 'processed', processed_at = now() WHERE id = $1`,
		eventID); err != nil {
		return false, err
	}
	return false, tx.Commit(ctx)
}
