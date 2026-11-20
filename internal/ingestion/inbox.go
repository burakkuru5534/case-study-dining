package ingestion

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// untranslatableCharacter is raised when jsonb rejects \u0000 inside a string.
// Go's JSON decoder accepts it, so such a payload passes Parse but can never be
// stored: it must be a 422, not a retryable 503.
const untranslatableCharacter = "22P05"

// Inbox persists verified events as 'pending' for asynchronous processing.
type Inbox struct {
	db *pgxpool.Pool
}

func NewInbox(db *pgxpool.Pool) *Inbox {
	return &Inbox{db: db}
}

// Insert stores the event with its raw body. It returns inserted=false when
// the event already exists (duplicate delivery), which is not an error.
func (i *Inbox) Insert(ctx context.Context, ev Event, rawBody []byte) (inserted bool, err error) {
	tag, err := i.db.Exec(ctx, `
		INSERT INTO webhook_events (provider, tenant_ref, external_event_id, event_type, payload)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (provider, tenant_ref, external_event_id) DO NOTHING`,
		ev.Provider, ev.TenantRef, ev.ExternalEventID, ev.EventType, rawBody)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == untranslatableCharacter {
			return false, fmt.Errorf("%w: payload contains characters that cannot be stored (e.g. \\u0000)", ErrInvalidPayload)
		}
		return false, fmt.Errorf("insert webhook event: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
