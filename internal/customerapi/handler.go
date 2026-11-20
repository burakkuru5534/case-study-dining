// Package customerapi serves the read API for a customer's order history and
// loyalty balances. Customers are addressed by their provider identity
// (provider, tenant, external customer id): callers know those, and internal
// sequential ids are never exposed.
package customerapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	loyaltystore "github.com/burakkuru5534/case-study-dining/internal/loyalty/pgstore"
	"github.com/burakkuru5534/case-study-dining/internal/order"
	orderstore "github.com/burakkuru5534/case-study-dining/internal/order/pgstore"
)

const (
	DefaultLimit = 20
	MaxLimit     = 100
	queryTimeout = 5 * time.Second
)

// Querier is satisfied by *pgxpool.Pool.
type Querier interface {
	orderstore.Querier
	loyaltystore.Querier
}

type Handler struct {
	db     Querier
	logger *slog.Logger
}

func NewHandler(db Querier, logger *slog.Logger) *Handler {
	return &Handler{db: db, logger: logger}
}

// Paths, registered by the caller (behind authentication).
const (
	OrdersPattern  = "GET /v1/providers/{provider}/tenants/{tenant}/customers/{customer}/orders"
	LoyaltyPattern = "GET /v1/providers/{provider}/tenants/{tenant}/customers/{customer}/loyalty"
)

type orderJSON struct {
	RestaurantID        string    `json:"restaurant_id"`
	OrderID             string    `json:"order_id"`
	EligibleAmountMinor int64     `json:"eligible_amount_minor"`
	Currency            string    `json:"currency"`
	CompletedAt         time.Time `json:"completed_at"`
	LoyaltyStatus       string    `json:"loyalty_status"`
	PointsAwarded       int64     `json:"points_awarded"`
}

type ordersResponse struct {
	Orders     []orderJSON `json:"orders"`
	NextCursor *string     `json:"next_cursor"`
}

// Orders returns the customer's orders, newest first, with keyset pagination.
func (h *Handler) Orders(w http.ResponseWriter, r *http.Request) {
	limit, err := parseLimit(r.URL.Query().Get("limit"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_limit")
		return
	}
	var after *orderstore.Position
	if c := r.URL.Query().Get("cursor"); c != "" {
		pos, err := decodeCursor(c)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_cursor")
			return
		}
		after = &pos
	}

	ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
	defer cancel()
	customerID, ok := h.resolveCustomer(ctx, w, r)
	if !ok {
		return
	}

	// One extra row tells whether another page exists, without a COUNT.
	rows, err := orderstore.ListByCustomer(ctx, h.db, customerID, after, limit+1)
	if err != nil {
		h.unavailable(w, "list orders failed", err)
		return
	}

	resp := ordersResponse{Orders: make([]orderJSON, 0, min(len(rows), limit))}
	if len(rows) > limit {
		last := rows[limit-1]
		c := encodeCursor(orderstore.Position{CompletedAt: last.CompletedAt, ID: last.ID})
		resp.NextCursor = &c
		rows = rows[:limit]
	}
	for _, o := range rows {
		resp.Orders = append(resp.Orders, orderJSON{
			RestaurantID:        o.ExternalRestaurantID,
			OrderID:             o.ExternalOrderID,
			EligibleAmountMinor: o.EligibleAmountMinor,
			Currency:            o.Currency,
			CompletedAt:         o.CompletedAt,
			LoyaltyStatus:       o.LoyaltyStatus,
			PointsAwarded:       o.PointsAwarded,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

type balanceJSON struct {
	ProgramID   int64     `json:"program_id"`
	ProgramName string    `json:"program_name"`
	Balance     int64     `json:"balance"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type loyaltyResponse struct {
	Balances []balanceJSON `json:"balances"`
}

// Loyalty returns the customer's balance in every program they earned in.
func (h *Handler) Loyalty(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
	defer cancel()
	customerID, ok := h.resolveCustomer(ctx, w, r)
	if !ok {
		return
	}

	balances, err := loyaltystore.Balances(ctx, h.db, customerID)
	if err != nil {
		h.unavailable(w, "list balances failed", err)
		return
	}
	resp := loyaltyResponse{Balances: make([]balanceJSON, 0, len(balances))}
	for _, b := range balances {
		resp.Balances = append(resp.Balances, balanceJSON{
			ProgramID: b.ProgramID, ProgramName: b.ProgramName, Balance: b.Balance, UpdatedAt: b.UpdatedAt,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// resolveCustomer validates the path identity and maps it to the internal
// customer. It writes the error response itself and returns ok=false on failure.
func (h *Handler) resolveCustomer(ctx context.Context, w http.ResponseWriter, r *http.Request) (int64, bool) {
	provider, tenant, customer := r.PathValue("provider"), r.PathValue("tenant"), r.PathValue("customer")
	for _, f := range []struct{ name, v string }{{"provider", provider}, {"tenant", tenant}, {"customer", customer}} {
		if order.ValidateExternalID(f.name, f.v) != nil {
			writeError(w, http.StatusBadRequest, "invalid_"+f.name)
			return 0, false
		}
	}

	id, err := loyaltystore.FindCustomer(ctx, h.db, provider, tenant, customer)
	switch {
	case errors.Is(err, loyaltystore.ErrCustomerNotFound):
		writeError(w, http.StatusNotFound, "customer_not_found")
		return 0, false
	case err != nil:
		h.unavailable(w, "find customer failed", err)
		return 0, false
	}
	return id, true
}

func (h *Handler) unavailable(w http.ResponseWriter, msg string, err error) {
	// Database details stay in the log, never in the response.
	h.logger.Error(msg, "error", err)
	writeError(w, http.StatusServiceUnavailable, "temporarily_unavailable")
}

func parseLimit(raw string) (int, error) {
	if raw == "" {
		return DefaultLimit, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > MaxLimit {
		return 0, errors.New("limit out of range")
	}
	return n, nil
}

// The cursor is opaque to clients: base64url(JSON{t, id}).
type cursorJSON struct {
	T  time.Time `json:"t"`
	ID int64     `json:"id"`
}

func encodeCursor(p orderstore.Position) string {
	b, _ := json.Marshal(cursorJSON{T: p.CompletedAt.UTC(), ID: p.ID}) // cannot fail for this type
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string) (orderstore.Position, error) {
	if len(s) > 200 {
		return orderstore.Position{}, errors.New("cursor too long")
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return orderstore.Position{}, err
	}
	var c cursorJSON
	if err := json.Unmarshal(b, &c); err != nil {
		return orderstore.Position{}, err
	}
	if c.T.IsZero() || c.ID <= 0 {
		return orderstore.Position{}, errors.New("incomplete cursor")
	}
	return orderstore.Position{CompletedAt: c.T, ID: c.ID}, nil
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
