// Package pgstore persists loyalty data in PostgreSQL: program lookup via the
// restaurant, customer identities, balances and the append-only ledger.
// Functions take the caller's transaction so order and loyalty writes commit
// atomically.
package pgstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/burakkuru5534/case-study-dining/internal/loyalty"
)

var (
	// ErrRestaurantNotFound means the restaurant is not provisioned. It does
	// not resolve by retrying; the event can be replayed after provisioning.
	ErrRestaurantNotFound = errors.New("restaurant not provisioned")

	// ErrIdentityRace means another transaction created the same customer
	// identity concurrently. The caller must roll back (discarding the
	// customer row it created) and retry.
	ErrIdentityRace = errors.New("customer identity created concurrently")
)

// RestaurantProgram is a restaurant with its loyalty program's earn rule.
type RestaurantProgram struct {
	RestaurantID int64
	ProgramID    int64
	Rule         loyalty.Rule
}

// FindRestaurantForShare looks up a restaurant and its program, locking the
// restaurant row FOR SHARE until the transaction ends. A concurrent program
// change waits for this transaction, so the program used here stays the
// restaurant's program until commit (the 0003 trigger never has to fire).
func FindRestaurantForShare(ctx context.Context, tx pgx.Tx, provider, tenantRef, externalID string) (RestaurantProgram, error) {
	var rp RestaurantProgram
	err := tx.QueryRow(ctx, `
		SELECT r.id, p.id, p.earn_currency, p.earn_unit_minor, p.points_per_unit
		FROM restaurants r
		JOIN loyalty_programs p ON p.id = r.loyalty_program_id
		WHERE r.provider = $1 AND r.tenant_ref = $2 AND r.external_restaurant_id = $3
		FOR SHARE OF r`,
		provider, tenantRef, externalID,
	).Scan(&rp.RestaurantID, &rp.ProgramID, &rp.Rule.Currency, &rp.Rule.EarnUnitMinor, &rp.Rule.PointsPerUnit)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return RestaurantProgram{}, ErrRestaurantNotFound
	case err != nil:
		return RestaurantProgram{}, fmt.Errorf("find restaurant: %w", err)
	}
	return rp, nil
}

// ResolveCustomer returns the internal customer for a provider identity,
// creating both when the identity is new.
func ResolveCustomer(ctx context.Context, tx pgx.Tx, provider, tenantRef, externalCustomerID string) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx, `
		SELECT customer_id FROM customer_identities
		WHERE provider = $1 AND tenant_ref = $2 AND external_customer_id = $3`,
		provider, tenantRef, externalCustomerID,
	).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("find customer identity: %w", err)
	}

	// New identity. If another transaction inserts the same identity first,
	// this statement waits on the unique index, then DO NOTHING returns no row.
	err = tx.QueryRow(ctx, `
		WITH c AS (INSERT INTO customers DEFAULT VALUES RETURNING id)
		INSERT INTO customer_identities (provider, tenant_ref, external_customer_id, customer_id)
		SELECT $1, $2, $3, id FROM c
		ON CONFLICT (provider, tenant_ref, external_customer_id) DO NOTHING
		RETURNING customer_id`,
		provider, tenantRef, externalCustomerID,
	).Scan(&id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, ErrIdentityRace
	case err != nil:
		return 0, fmt.Errorf("create customer: %w", err)
	}
	return id, nil
}

// Earn is one earn transaction for an order.
type Earn struct {
	ProgramID  int64
	CustomerID int64
	OrderID    int64
	Points     int64
	Rule       loyalty.Rule // snapshot stored on the ledger row
}

// AwardEarn adds points to the balance and appends the ledger row. Both run in
// the caller's transaction. The account upsert takes a row lock, so concurrent
// awards to the same account serialize without lost updates. The ledger's
// unique (order_id) WHERE type='earn' index makes a second award impossible.
func AwardEarn(ctx context.Context, tx pgx.Tx, e Earn) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO loyalty_accounts (loyalty_program_id, customer_id, balance)
		VALUES ($1, $2, $3)
		ON CONFLICT (loyalty_program_id, customer_id)
		DO UPDATE SET balance = loyalty_accounts.balance + EXCLUDED.balance, updated_at = now()`,
		e.ProgramID, e.CustomerID, e.Points); err != nil {
		return fmt.Errorf("update balance: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO loyalty_transactions
			(loyalty_program_id, customer_id, type, points, order_id, earn_unit_minor, points_per_unit)
		VALUES ($1, $2, 'earn', $3, $4, $5, $6)`,
		e.ProgramID, e.CustomerID, e.Points, e.OrderID, e.Rule.EarnUnitMinor, e.Rule.PointsPerUnit); err != nil {
		return fmt.Errorf("append ledger: %w", err)
	}
	return nil
}

// Querier is satisfied by *pgxpool.Pool, *pgx.Conn and pgx.Tx.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ErrCustomerNotFound means no customer has this provider identity.
var ErrCustomerNotFound = errors.New("customer not found")

// FindCustomer resolves a provider identity to the internal customer, read-only.
func FindCustomer(ctx context.Context, q Querier, provider, tenantRef, externalCustomerID string) (int64, error) {
	var id int64
	err := q.QueryRow(ctx, `
		SELECT customer_id FROM customer_identities
		WHERE provider = $1 AND tenant_ref = $2 AND external_customer_id = $3`,
		provider, tenantRef, externalCustomerID).Scan(&id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, ErrCustomerNotFound
	case err != nil:
		return 0, fmt.Errorf("find customer: %w", err)
	}
	return id, nil
}

// Balance is a customer's balance in one loyalty program.
type Balance struct {
	ProgramID   int64
	ProgramName string
	Balance     int64
	UpdatedAt   time.Time
}

// Balances returns the customer's balance in every program they have earned in.
func Balances(ctx context.Context, q Querier, customerID int64) ([]Balance, error) {
	rows, err := q.Query(ctx, `
		SELECT p.id, p.name, a.balance, a.updated_at
		FROM loyalty_accounts a
		JOIN loyalty_programs p ON p.id = a.loyalty_program_id
		WHERE a.customer_id = $1
		ORDER BY p.id`, customerID)
	if err != nil {
		return nil, fmt.Errorf("list balances: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Balance, error) {
		var b Balance
		err := row.Scan(&b.ProgramID, &b.ProgramName, &b.Balance, &b.UpdatedAt)
		b.UpdatedAt = b.UpdatedAt.UTC()
		return b, err
	})
	if err != nil {
		return nil, fmt.Errorf("list balances: %w", err)
	}
	return out, nil
}
