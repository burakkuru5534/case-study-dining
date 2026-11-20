package ingestion_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/burakkuru5534/case-study-dining/internal/ingestion"
	"github.com/burakkuru5534/case-study-dining/internal/ingestion/reference"
	"github.com/burakkuru5534/case-study-dining/internal/order"
	"github.com/burakkuru5534/case-study-dining/internal/platform/testdb"
)

func inboxEvent(tenant, eventID string) ingestion.Event {
	return ingestion.Event{
		Provider:        "reference",
		TenantRef:       tenant,
		ExternalEventID: eventID,
		EventType:       ingestion.EventTypeOrderCompleted,
		Order: order.CompletedOrder{
			ExternalRestaurantID: "r-1", ExternalOrderID: "o-1",
			EligibleAmountMinor: 1000, Currency: "TRY", CompletedAt: time.Now(),
		},
	}
}

func TestInbox_InsertAndDeduplicate(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	inbox := ingestion.NewInbox(db)
	body := []byte(`{"event_id":"evt-1","n":9007199254740993}`)

	inserted, err := inbox.Insert(ctx, inboxEvent("t1", "evt-1"), body)
	if err != nil || !inserted {
		t.Fatalf("first insert: inserted=%v err=%v", inserted, err)
	}

	inserted, err = inbox.Insert(ctx, inboxEvent("t1", "evt-1"), body)
	if err != nil || inserted {
		t.Fatalf("duplicate insert: inserted=%v err=%v, want false/nil", inserted, err)
	}

	inserted, err = inbox.Insert(ctx, inboxEvent("t2", "evt-1"), body)
	if err != nil || !inserted {
		t.Fatalf("same event id, other tenant: inserted=%v err=%v", inserted, err)
	}

	var (
		n                 int
		status, eventType string
		attempts          int
		payload           []byte
	)
	if err := db.QueryRow(ctx, `SELECT count(*) FROM webhook_events`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("rows = %d err=%v, want 2", n, err)
	}
	if err := db.QueryRow(ctx, `SELECT status, event_type, attempts, payload::text FROM webhook_events
		WHERE tenant_ref = 't1'`).Scan(&status, &eventType, &attempts, &payload); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || eventType != "order.completed" || attempts != 0 {
		t.Fatalf("status=%s type=%s attempts=%d", status, eventType, attempts)
	}

	// The worker re-parses the stored payload: large integers must survive jsonb exactly.
	var stored map[string]any
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	if err := dec.Decode(&stored); err != nil {
		t.Fatal(err)
	}
	if stored["n"] != json.Number("9007199254740993") {
		t.Fatalf("integer changed in jsonb: %s", stored["n"])
	}
}

// The documented sample must round-trip through jsonb and still parse to the
// same event, since replays re-parse what is stored.
func TestInbox_StoredPayloadReparsesIdentically(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	body, err := os.ReadFile("../../docs/reference-webhook/order_completed.json")
	if err != nil {
		t.Fatal(err)
	}

	want, err := reference.Adapter{}.Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ingestion.NewInbox(db).Insert(ctx, want, body); err != nil {
		t.Fatal(err)
	}

	var stored []byte
	if err := db.QueryRow(ctx, `SELECT payload::text FROM webhook_events`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	got, err := reference.Adapter{}.Parse(stored)
	if err != nil {
		t.Fatalf("stored payload no longer parses: %v", err)
	}
	if got != want {
		t.Fatalf("re-parsed event differs:\n got %+v\nwant %+v", got, want)
	}
}

// Go's JSON decoder accepts \u0000 inside strings, jsonb does not. That must
// surface as an invalid payload (422), not a retryable store error (503).
func TestInbox_NulCharacterIsInvalidPayload(t *testing.T) {
	db := testdb.New(t)

	_, err := ingestion.NewInbox(db).Insert(context.Background(), inboxEvent("t1", "evt-nul"),
		[]byte(`{"note":"a\u0000b"}`))
	if !errors.Is(err, ingestion.ErrInvalidPayload) {
		t.Fatalf("error = %v, want ErrInvalidPayload", err)
	}
}

func TestInbox_DatabaseErrorIsNotInvalidPayload(t *testing.T) {
	db := testdb.New(t)
	db.Close() // simulate an unavailable database

	_, err := ingestion.NewInbox(db).Insert(context.Background(), inboxEvent("t1", "evt-x"), []byte(`{}`))
	if err == nil || errors.Is(err, ingestion.ErrInvalidPayload) {
		t.Fatalf("error = %v, want a non-payload error", err)
	}
}
