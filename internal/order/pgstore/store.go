// Package pgstore persists orders in PostgreSQL. It owns writes to the orders
// table. Functions take the caller's transaction so order and loyalty writes
// commit atomically.
package pgstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// NewOrder is a processed order ready to be stored.
type NewOrder struct {
	RestaurantID        int64
	LoyaltyProgramID    int64
	ExternalOrderID     string
	CustomerID          *int64 // nil for guest orders
	EligibleAmountMinor int64
	Currency            string
	CompletedAt         time.Time
	LoyaltyStatus       string
	SourceEventID       int64
}

// Insert stores the order. inserted is false when the restaurant already has
// an order with the same external ID (the same order delivered under another
// event); in that case nothing is written and id is 0.
func Insert(ctx context.Context, tx pgx.Tx, o NewOrder) (id int64, inserted bool, err error) {
	err = tx.QueryRow(ctx, `
		INSERT INTO orders (restaurant_id, loyalty_program_id, external_order_id, customer_id,
			eligible_amount_minor, currency, completed_at, loyalty_status, source_event_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (restaurant_id, external_order_id) DO NOTHING
		RETURNING id`,
		o.RestaurantID, o.LoyaltyProgramID, o.ExternalOrderID, o.CustomerID,
		o.EligibleAmountMinor, o.Currency, o.CompletedAt, o.LoyaltyStatus, o.SourceEventID,
	).Scan(&id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("insert order: %w", err)
	}
	return id, true, nil
}
