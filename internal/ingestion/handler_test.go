package ingestion_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/burakkuru5534/case-study-dining/internal/ingestion"
	"github.com/burakkuru5534/case-study-dining/internal/ingestion/reference"
)

var (
	testSecret = []byte("handler-test-secret-0123456789abcdef")
	fixedNow   = time.Unix(1763631000, 0)
)

// fakeStore records inserts and returns configured results.
type fakeStore struct {
	mu       sync.Mutex
	calls    int
	lastEv   ingestion.Event
	lastBody []byte
	inserted bool
	err      error
}

func (f *fakeStore) Insert(_ context.Context, ev ingestion.Event, body []byte) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastEv, f.lastBody = ev, append([]byte(nil), body...)
	return f.inserted, f.err
}

// countingAdapter proves whether Parse was reached.
type countingAdapter struct {
	ingestion.Adapter
	mu    sync.Mutex
	calls int
}

func (c *countingAdapter) Parse(b []byte) (ingestion.Event, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return c.Adapter.Parse(b)
}

type harness struct {
	mux     *http.ServeMux
	store   *fakeStore
	adapter *countingAdapter
	logs    *bytes.Buffer
}

func newHarness(t *testing.T, store *fakeStore) *harness {
	t.Helper()
	adapter := &countingAdapter{Adapter: reference.Adapter{}}
	logs := &bytes.Buffer{}
	h := ingestion.NewHandler(map[string]ingestion.Provider{
		reference.ProviderName: {
			Verifier: reference.Verifier{Secrets: [][]byte{testSecret}, Now: func() time.Time { return fixedNow }},
			Adapter:  adapter,
		},
	}, store, slog.New(slog.NewJSONHandler(logs, nil)))

	mux := http.NewServeMux()
	mux.Handle("POST /webhooks/{provider}", h)
	return &harness{mux: mux, store: store, adapter: adapter, logs: logs}
}

func (h *harness) do(t *testing.T, method, path string, body []byte, sign bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if sign {
		req.Header.Set(reference.HeaderTimestamp, strconv.FormatInt(fixedNow.Unix(), 10))
		req.Header.Set(reference.HeaderSignature, reference.Sign(testSecret, fixedNow.Unix(), body))
	}
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	return rec
}

func sample(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../../docs/reference-webhook/order_completed.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("response is not JSON: %q", rec.Body.String())
	}
	return m
}

const path = "/webhooks/reference"

func TestHandler_AcceptsValidEvent(t *testing.T) {
	t.Parallel()
	h := newHarness(t, &fakeStore{inserted: true})
	body := sample(t)

	rec := h.do(t, http.MethodPost, path, body, true)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	if m := decode(t, rec); m["status"] != "accepted" || m["duplicate"] != false {
		t.Fatalf("response = %v", m)
	}
	if h.store.calls != 1 {
		t.Fatalf("store calls = %d", h.store.calls)
	}
	if !bytes.Equal(h.store.lastBody, body) {
		t.Fatal("stored body differs from the raw request body")
	}
	if h.store.lastEv.Provider != "reference" || h.store.lastEv.TenantRef != "chain-istanbul" || h.store.lastEv.ExternalEventID != "evt_01JC4Z8Q2W" {
		t.Fatalf("stored event = %+v", h.store.lastEv)
	}
}

func TestHandler_DuplicateIsAcknowledged(t *testing.T) {
	t.Parallel()
	h := newHarness(t, &fakeStore{inserted: false})

	rec := h.do(t, http.MethodPost, path, sample(t), true)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d", rec.Code)
	}
	if m := decode(t, rec); m["duplicate"] != true {
		t.Fatalf("response = %v", m)
	}
}

func TestHandler_SignatureIsCheckedBeforeParsing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		body   []byte
		mutate func(*http.Request)
	}{
		{name: "unsigned", body: []byte(`{"event_id":`), mutate: func(*http.Request) {}},
		{name: "wrong secret", body: []byte(`not json at all`), mutate: func(r *http.Request) {
			r.Header.Set(reference.HeaderTimestamp, strconv.FormatInt(fixedNow.Unix(), 10))
			r.Header.Set(reference.HeaderSignature, reference.Sign([]byte("another-secret-0123456789abcdef0123"), fixedNow.Unix(), []byte(`not json at all`)))
		}},
		{name: "stale timestamp", body: []byte(`{}`), mutate: func(r *http.Request) {
			old := fixedNow.Add(-time.Hour).Unix()
			r.Header.Set(reference.HeaderTimestamp, strconv.FormatInt(old, 10))
			r.Header.Set(reference.HeaderSignature, reference.Sign(testSecret, old, []byte(`{}`)))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, &fakeStore{inserted: true})
			req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(tt.body))
			tt.mutate(req)
			rec := httptest.NewRecorder()
			h.mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (invalid body must not turn this into 422)", rec.Code)
			}
			if m := decode(t, rec); m["error"] != "invalid_signature" || len(m) != 1 {
				t.Fatalf("response must not reveal details: %v", m)
			}
			if h.adapter.calls != 0 || h.store.calls != 0 {
				t.Fatalf("parse calls = %d, store calls = %d; want 0/0", h.adapter.calls, h.store.calls)
			}
		})
	}
}

func TestHandler_SignedBodyTamperedAfterSigning(t *testing.T) {
	t.Parallel()
	h := newHarness(t, &fakeStore{inserted: true})
	body := sample(t)

	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(bytes.Replace(body, []byte("25000"), []byte("95000"), 1)))
	req.Header.Set(reference.HeaderTimestamp, strconv.FormatInt(fixedNow.Unix(), 10))
	req.Header.Set(reference.HeaderSignature, reference.Sign(testSecret, fixedNow.Unix(), body))
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized || h.store.calls != 0 {
		t.Fatalf("status = %d, store calls = %d", rec.Code, h.store.calls)
	}
}

func TestHandler_InvalidPayloadIs422WithDetails(t *testing.T) {
	t.Parallel()
	h := newHarness(t, &fakeStore{inserted: true})
	body := []byte(`{"event_id":"e-1","event_type":"order.completed","order":{"restaurant_id":"r","order_id":"o","subtotal_minor":100.5,"currency":"TRY"}}`)

	rec := h.do(t, http.MethodPost, path, body, true)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d", rec.Code)
	}
	m := decode(t, rec)
	details := fmt.Sprint(m["details"])
	if m["error"] != "invalid_payload" || !strings.Contains(details, "order.subtotal_minor") || !strings.Contains(details, "order.completed_at: required") {
		t.Fatalf("response = %v", m)
	}
	if h.store.calls != 0 {
		t.Fatal("invalid payload must not be stored")
	}
}

func TestHandler_UnsupportedEventTypeIs200AndNotStored(t *testing.T) {
	t.Parallel()
	h := newHarness(t, &fakeStore{inserted: true})

	rec := h.do(t, http.MethodPost, path, []byte(`{"event_id":"e-1","event_type":"menu.updated"}`), true)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if m := decode(t, rec); m["status"] != "ignored" {
		t.Fatalf("response = %v", m)
	}
	if h.store.calls != 0 {
		t.Fatal("unsupported event must not be stored")
	}
	if !strings.Contains(h.logs.String(), `"event_type":"menu.updated"`) {
		t.Fatalf("expected event type in log, got %s", h.logs)
	}
}

func TestHandler_BodyTooLarge(t *testing.T) {
	t.Parallel()
	h := newHarness(t, &fakeStore{inserted: true})

	rec := h.do(t, http.MethodPost, path, bytes.Repeat([]byte("a"), ingestion.MaxBodyBytes+1), true)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d", rec.Code)
	}
	if h.adapter.calls != 0 || h.store.calls != 0 {
		t.Fatal("oversized body must not reach parse or store")
	}
}

func TestHandler_BodyAtLimitIsRead(t *testing.T) {
	t.Parallel()
	h := newHarness(t, &fakeStore{inserted: true})
	// Exactly MaxBodyBytes of JSON: whitespace padding after a valid object is
	// still one object, so it must get past the size check and parse.
	body := sample(t)
	body = append(body, bytes.Repeat([]byte(" "), ingestion.MaxBodyBytes-len(body))...)

	rec := h.do(t, http.MethodPost, path, body, true)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
}

func TestHandler_StoreErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantError  string
	}{
		{"database unavailable is retryable", errors.New("connection refused"), http.StatusServiceUnavailable, "temporarily_unavailable"},
		{"unstorable payload is 422", fmt.Errorf("%w: payload contains characters that cannot be stored", ingestion.ErrInvalidPayload), http.StatusUnprocessableEntity, "invalid_payload"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, &fakeStore{err: tt.err})
			rec := h.do(t, http.MethodPost, path, sample(t), true)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if m := decode(t, rec); m["error"] != tt.wantError {
				t.Fatalf("response = %v", m)
			}
			if strings.Contains(rec.Body.String(), "connection refused") {
				t.Fatal("internal error leaked to the caller")
			}
		})
	}
}

func TestHandler_Routing(t *testing.T) {
	t.Parallel()
	h := newHarness(t, &fakeStore{inserted: true})

	if rec := h.do(t, http.MethodPost, "/webhooks/unknown", sample(t), true); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown provider: status = %d", rec.Code)
	}
	if rec := h.do(t, http.MethodGet, path, nil, false); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: status = %d", rec.Code)
	}
}

// Logs must never contain customer, payment, body or signing data.
func TestHandler_LogsContainNoSensitiveData(t *testing.T) {
	t.Parallel()
	h := newHarness(t, &fakeStore{inserted: true})
	body := sample(t)

	// Accepted, rejected-invalid, rejected-signature and ignored paths all log.
	h.do(t, http.MethodPost, path, body, true)
	invalid := bytes.Replace(body, []byte(`"currency": "TRY"`), []byte(`"currency": "cust-7f3a9c-XX"`), 1)
	h.do(t, http.MethodPost, path, invalid, true)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set(reference.HeaderTimestamp, strconv.FormatInt(fixedNow.Unix(), 10))
	req.Header.Set(reference.HeaderSignature, "v1="+strings.Repeat("ab", 32))
	h.mux.ServeHTTP(httptest.NewRecorder(), req)

	logs := h.logs.String()
	for _, sensitive := range []string{
		"cust-7f3a9c",           // customer id
		"25000", "2500", "1500", // amounts
		"ORD-2026-000123",  // order id
		string(testSecret), // secret
		reference.Sign(testSecret, fixedNow.Unix(), body), // signature
		`"order"`, // any part of the body
	} {
		if strings.Contains(logs, sensitive) {
			t.Errorf("logs contain %q:\n%s", sensitive, logs)
		}
	}
	// Sanity: the useful identifiers are there.
	for _, want := range []string{`"provider":"reference"`, `"event_id":"evt_01JC4Z8Q2W"`, `"tenant_ref":"chain-istanbul"`} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs missing %s", want)
		}
	}
}
