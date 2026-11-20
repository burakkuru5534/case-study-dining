package ingestion

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/burakkuru5534/case-study-dining/internal/loyalty"
	loyaltystore "github.com/burakkuru5534/case-study-dining/internal/loyalty/pgstore"
	orderstore "github.com/burakkuru5534/case-study-dining/internal/order/pgstore"
)

// Permanent processing failures: retrying cannot help. The event stays
// 'failed' until an operator resolves the cause and sets it back to 'pending'.
var (
	ErrUnknownProvider   = errors.New("unknown provider")
	ErrUnknownRestaurant = errors.New("unknown restaurant")
)

const maxStoredErrorLength = 500

// ProcessorConfig tunes retries and lock waits. Zero values use defaults.
type ProcessorConfig struct {
	MaxAttempts int           // default 10
	LockTimeout time.Duration // default 5s; bounds waits on row locks
	Backoff     Backoff       // default DefaultBackoff
}

// Processor turns one pending inbox event into an order and, when earned,
// loyalty points, all in a single transaction.
type Processor struct {
	db       *pgxpool.Pool
	adapters map[string]Adapter
	cfg      ProcessorConfig
	logger   *slog.Logger
}

func NewProcessor(db *pgxpool.Pool, adapters map[string]Adapter, cfg ProcessorConfig, logger *slog.Logger) *Processor {
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 10
	}
	if cfg.LockTimeout <= 0 {
		cfg.LockTimeout = 5 * time.Second
	}
	if cfg.Backoff.Base <= 0 {
		cfg.Backoff = DefaultBackoff
	}
	return &Processor{db: db, adapters: adapters, cfg: cfg, logger: logger}
}

type claimedEvent struct {
	id       int64
	provider string
	payload  []byte
	attempts int
}

// outcome describes a successful processing, for logging.
type outcome struct {
	status         loyalty.Status
	points         int64
	duplicateOrder bool
}

// ProcessNext claims the oldest due event and processes it.
//
// It returns claimed=false when no event is due. A processing failure is
// recorded on the event (retry with backoff, or permanent failure) and is not
// returned as an error; err is only returned when the outcome could not be
// recorded, in which case the transaction rolls back and the event is left
// untouched for a later attempt.
func (p *Processor) ProcessNext(ctx context.Context) (claimed bool, err error) {
	tx, err := p.db.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }() // no-op after commit

	// Bounded lock waits: a long-held lock becomes a transient failure instead
	// of a stuck worker. SET LOCAL only lasts for this transaction.
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", p.cfg.LockTimeout.Milliseconds())); err != nil {
		return false, fmt.Errorf("set lock_timeout: %w", err)
	}

	// The row lock is held until commit. SKIP LOCKED lets other workers take
	// other events instead of waiting; a crash rolls back and frees the event.
	var ev claimedEvent
	err = tx.QueryRow(ctx, `
		SELECT id, provider, payload::text, attempts
		FROM webhook_events
		WHERE status = 'pending' AND next_attempt_at <= now()
		ORDER BY next_attempt_at, id
		LIMIT 1
		FOR UPDATE SKIP LOCKED`).Scan(&ev.id, &ev.provider, &ev.payload, &ev.attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim event: %w", err)
	}
	log := p.logger.With("webhook_event_id", ev.id, "provider", ev.provider)

	// Business writes happen under a savepoint, so a failure can be undone
	// while the event row stays locked for recording the failure.
	sp, err := tx.Begin(ctx)
	if err != nil {
		return true, fmt.Errorf("savepoint: %w", err)
	}
	out, procErr := p.process(ctx, sp, ev)
	if procErr == nil {
		if err := sp.Commit(ctx); err != nil { // RELEASE SAVEPOINT
			return true, fmt.Errorf("release savepoint: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE webhook_events SET status = 'processed', processed_at = now() WHERE id = $1`, ev.id); err != nil {
			return true, fmt.Errorf("mark processed: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return true, fmt.Errorf("commit: %w", err)
		}
		log.Info("event processed", "loyalty_status", out.status, "points", out.points, "duplicate_order", out.duplicateOrder)
		return true, nil
	}

	// Shutdown or caller cancellation is not the event's fault: leave it
	// untouched (the deferred rollback releases it) instead of counting an attempt.
	if ctx.Err() != nil {
		return true, ctx.Err()
	}

	if err := sp.Rollback(ctx); err != nil { // ROLLBACK TO SAVEPOINT
		return true, fmt.Errorf("rollback to savepoint: %w (processing error: %v)", err, procErr)
	}
	if err := p.recordFailure(ctx, tx, ev, procErr, log); err != nil {
		return true, err
	}
	if err := tx.Commit(ctx); err != nil {
		return true, fmt.Errorf("commit failure: %w", err)
	}
	return true, nil
}

// process performs the business writes for one event inside tx.
func (p *Processor) process(ctx context.Context, tx pgx.Tx, ev claimedEvent) (outcome, error) {
	adapter, ok := p.adapters[ev.provider]
	if !ok {
		return outcome{}, fmt.Errorf("%w %q", ErrUnknownProvider, ev.provider)
	}
	// Re-parse the stored raw payload: adapter fixes apply to replays.
	event, err := adapter.Parse(ev.payload)
	if err != nil {
		var unsupported *UnsupportedEventTypeError
		if errors.As(err, &unsupported) {
			return outcome{}, fmt.Errorf("%w: %w", ErrInvalidPayload, err)
		}
		return outcome{}, err
	}
	o := event.Order

	rp, err := loyaltystore.FindRestaurantForShare(ctx, tx, event.Provider, event.TenantRef, o.ExternalRestaurantID)
	if errors.Is(err, loyaltystore.ErrRestaurantNotFound) {
		return outcome{}, fmt.Errorf("%w (tenant_ref %q)", ErrUnknownRestaurant, event.TenantRef)
	}
	if err != nil {
		return outcome{}, err
	}

	var customerID *int64
	if o.HasCustomer() {
		id, err := loyaltystore.ResolveCustomer(ctx, tx, event.Provider, event.TenantRef, o.ExternalCustomerID)
		if err != nil {
			return outcome{}, err
		}
		customerID = &id
	}

	points, status, err := loyalty.Evaluate(rp.Rule, loyalty.Purchase{
		HasCustomer:         customerID != nil,
		Currency:            o.Currency,
		EligibleAmountMinor: o.EligibleAmountMinor,
	})
	if err != nil {
		return outcome{}, err
	}

	orderID, inserted, err := orderstore.Insert(ctx, tx, orderstore.NewOrder{
		RestaurantID:        rp.RestaurantID,
		LoyaltyProgramID:    rp.ProgramID,
		ExternalOrderID:     o.ExternalOrderID,
		CustomerID:          customerID,
		EligibleAmountMinor: o.EligibleAmountMinor,
		Currency:            o.Currency,
		CompletedAt:         o.CompletedAt,
		LoyaltyStatus:       string(status),
		SourceEventID:       ev.id,
	})
	if err != nil {
		return outcome{}, err
	}
	if !inserted {
		// Same order already delivered under another event: acknowledge, award nothing.
		return outcome{duplicateOrder: true}, nil
	}

	if status == loyalty.StatusAwarded {
		if err := loyaltystore.AwardEarn(ctx, tx, loyaltystore.Earn{
			ProgramID:  rp.ProgramID,
			CustomerID: *customerID,
			OrderID:    orderID,
			Points:     points,
			Rule:       rp.Rule,
		}); err != nil {
			return outcome{}, err
		}
	}
	return outcome{status: status, points: points}, nil
}

// recordFailure stores the failure on the still-locked event row.
func (p *Processor) recordFailure(ctx context.Context, tx pgx.Tx, ev claimedEvent, procErr error, log *slog.Logger) error {
	attempts := ev.attempts + 1
	msg := sanitizeError(procErr)

	if isPermanent(procErr) {
		log.Warn("event failed permanently", "attempts", attempts, "error", msg)
		_, err := tx.Exec(ctx, `
			UPDATE webhook_events
			SET status = 'failed', attempts = $2, last_error = $3, last_error_kind = 'permanent'
			WHERE id = $1`, ev.id, attempts, msg)
		if err != nil {
			return fmt.Errorf("record permanent failure: %w", err)
		}
		return nil
	}

	if attempts >= p.cfg.MaxAttempts {
		log.Error("event failed: retries exhausted", "attempts", attempts, "error", msg)
		_, err := tx.Exec(ctx, `
			UPDATE webhook_events
			SET status = 'failed', attempts = $2, last_error = $3, last_error_kind = 'transient'
			WHERE id = $1`, ev.id, attempts, msg)
		if err != nil {
			return fmt.Errorf("record exhausted retries: %w", err)
		}
		return nil
	}

	delay := p.cfg.Backoff.Delay(attempts)
	log.Warn("event processing failed, will retry", "attempts", attempts, "retry_in", delay.String(), "error", msg)
	_, err := tx.Exec(ctx, `
		UPDATE webhook_events
		SET attempts = $2, next_attempt_at = now() + $3 * interval '1 millisecond',
		    last_error = $4, last_error_kind = 'transient'
		WHERE id = $1`, ev.id, attempts, delay.Milliseconds(), msg)
	if err != nil {
		return fmt.Errorf("record transient failure: %w", err)
	}
	return nil
}

// isPermanent reports failures that retrying cannot fix. Everything else is
// treated as transient, which is bounded by MaxAttempts.
func isPermanent(err error) bool {
	for _, target := range []error{
		ErrInvalidPayload,
		ErrUnknownProvider,
		ErrUnknownRestaurant,
		loyalty.ErrInvalidRule,
		loyalty.ErrInvalidPurchase,
		loyalty.ErrPointsOverflow,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// sanitizeError bounds what is stored in last_error. Our own errors name
// fields, never values; database errors are truncated.
func sanitizeError(err error) string {
	msg := err.Error()
	if len(msg) > maxStoredErrorLength {
		msg = strings.ToValidUTF8(msg[:maxStoredErrorLength], "")
	}
	return msg
}
