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

// Querier is satisfied by *pgxpool.Pool, *pgx.Conn and pgx.Tx, so read
// queries run on a pool and write paths keep using the caller's transaction.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// OrderView is one row of a customer's order history.
type OrderView struct {
	ID                   int64 // internal; used only for the pagination cursor
	ExternalRestaurantID string
	ExternalOrderID      string
	EligibleAmountMinor  int64
	Currency             string
	CompletedAt          time.Time
	LoyaltyStatus        string
	PointsAwarded        int64
}

// Position is a keyset cursor: the (completed_at, id) of the last row seen.
type Position struct {
	CompletedAt time.Time
	ID          int64
}

// ListByCustomer returns up to limit orders of a customer, newest first,
// strictly after the given position (nil for the first page). The order
// (completed_at DESC, id DESC) matches orders_customer_history_idx, and the
// id tiebreak makes pages stable when timestamps are equal.
func ListByCustomer(ctx context.Context, q Querier, customerID int64, after *Position, limit int) ([]OrderView, error) {
	const base = `
		SELECT o.id, r.external_restaurant_id, o.external_order_id, o.eligible_amount_minor,
		       o.currency, o.completed_at, o.loyalty_status, coalesce(lt.points, 0)
		FROM orders o
		JOIN restaurants r ON r.id = o.restaurant_id
		LEFT JOIN loyalty_transactions lt ON lt.order_id = o.id AND lt.type = 'earn'
		WHERE o.customer_id = $1`
	var (
		rows pgx.Rows
		err  error
	)
	if after == nil {
		rows, err = q.Query(ctx, base+`
		ORDER BY o.completed_at DESC, o.id DESC
		LIMIT $2`, customerID, limit)
	} else {
		rows, err = q.Query(ctx, base+`
		  AND (o.completed_at, o.id) < ($2, $3)
		ORDER BY o.completed_at DESC, o.id DESC
		LIMIT $4`, customerID, after.CompletedAt, after.ID, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("list orders: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (OrderView, error) {
		var v OrderView
		err := row.Scan(&v.ID, &v.ExternalRestaurantID, &v.ExternalOrderID, &v.EligibleAmountMinor,
			&v.Currency, &v.CompletedAt, &v.LoyaltyStatus, &v.PointsAwarded)
		v.CompletedAt = v.CompletedAt.UTC()
		return v, err
	})
	if err != nil {
		return nil, fmt.Errorf("list orders: %w", err)
	}
	return out, nil
}
