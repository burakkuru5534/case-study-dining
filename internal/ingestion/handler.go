package ingestion

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const (
	// MaxBodyBytes bounds a webhook body. Orders are small; anything larger is
	// rejected before it is buffered.
	MaxBodyBytes = 1 << 20 // 1 MiB

	storeTimeout = 5 * time.Second
)

// Verifier authenticates a raw webhook body. header returns a request header
// value by name. Implementations must not parse the body.
type Verifier interface {
	Verify(header func(string) string, body []byte) error
}

// Provider bundles what intake needs for one POS provider.
type Provider struct {
	Verifier Verifier
	Adapter  Adapter
}

// EventStore persists verified events. *Inbox implements it; tests use a fake.
type EventStore interface {
	Insert(ctx context.Context, ev Event, rawBody []byte) (inserted bool, err error)
}

// Handler receives webhooks at POST /webhooks/{provider}.
//
// Order of checks: provider → body size → signature → parse → store.
// The signature is verified on the raw bytes before any parsing.
type Handler struct {
	providers map[string]Provider
	store     EventStore
	logger    *slog.Logger
}

func NewHandler(providers map[string]Provider, store EventStore, logger *slog.Logger) *Handler {
	return &Handler{providers: providers, store: store, logger: logger}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("provider")
	p, ok := h.providers[name]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown_provider"})
		return
	}
	log := h.logger.With("provider", name)

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			log.Warn("webhook rejected: body too large", "limit_bytes", MaxBodyBytes)
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "body_too_large"})
			return
		}
		log.Warn("webhook rejected: reading body failed", "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unreadable_body"})
		return
	}

	if err := p.Verifier.Verify(r.Header.Get, body); err != nil {
		// The reason is useful for operators; the caller gets nothing to probe with.
		log.Warn("webhook rejected: invalid signature", "reason", err)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid_signature"})
		return
	}

	ev, err := p.Adapter.Parse(body)
	if err != nil {
		var unsupported *UnsupportedEventTypeError
		switch {
		case errors.As(err, &unsupported):
			log.Info("webhook ignored: unsupported event type", "event_type", unsupported.EventType)
			writeJSON(w, http.StatusOK, map[string]any{"status": "ignored"})
		case errors.Is(err, ErrInvalidPayload):
			h.rejectInvalid(w, log, err)
		default:
			log.Error("webhook parse failed unexpectedly", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal_error"})
		}
		return
	}

	log = log.With("tenant_ref", ev.TenantRef, "event_id", ev.ExternalEventID)

	ctx, cancel := context.WithTimeout(r.Context(), storeTimeout)
	defer cancel()
	inserted, err := h.store.Insert(ctx, ev, body)
	switch {
	case errors.Is(err, ErrInvalidPayload):
		h.rejectInvalid(w, log, err)
		return
	case err != nil:
		// Retryable for the provider: nothing was stored.
		log.Error("webhook store failed", "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "temporarily_unavailable"})
		return
	}

	log.Info("webhook accepted", "duplicate", !inserted)
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "accepted", "duplicate": !inserted})
}

// rejectInvalid answers 422 with field-level details. Validation messages name
// fields only and never contain payload values, so they are safe to return.
func (h *Handler) rejectInvalid(w http.ResponseWriter, log *slog.Logger, err error) {
	log.Warn("webhook rejected: invalid payload", "error", err)
	msg := strings.TrimPrefix(err.Error(), ErrInvalidPayload.Error()+": ")
	writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
		"error":   "invalid_payload",
		"details": strings.Split(msg, "\n"),
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
