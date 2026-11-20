package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/burakkuru5534/case-study-dining/internal/ingestion/reference"
)

const secret = "sendwebhook-test-secret-0123456789ab"

// The tool must produce requests the real verifier accepts.
func TestRunSignsVerifiably(t *testing.T) {
	t.Parallel()

	var gotBody []byte
	var verifyErr error
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		verifyErr = reference.Verifier{Secrets: [][]byte{[]byte(secret)}}.Verify(r.Header.Get, gotBody)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	var out bytes.Buffer
	code, err := run([]string{"-url", srv.URL, "-file", "../../docs/reference-webhook/order_completed.json",
		"-secret", secret, "-event-id", "evt-override", "-order-id", "ORD-override"}, &out)
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v out=%s", code, err, out.String())
	}
	if verifyErr != nil {
		t.Fatalf("server could not verify signature: %v", verifyErr)
	}
	ev, err := reference.Adapter{}.Parse(gotBody)
	if err != nil {
		t.Fatalf("overridden body no longer parses: %v", err)
	}
	if ev.ExternalEventID != "evt-override" || ev.Order.ExternalOrderID != "ORD-override" || ev.Order.EligibleAmountMinor != 22500 {
		t.Fatalf("event = %+v", ev)
	}
}

func TestRunTimestampOffsetIsRejectedByVerifier(t *testing.T) {
	t.Parallel()

	var verifyErr error
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		verifyErr = reference.Verifier{Secrets: [][]byte{[]byte(secret)}}.Verify(r.Header.Get, b)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	code, err := run([]string{"-url", srv.URL, "-file", "../../docs/reference-webhook/order_completed.json",
		"-secret", secret, "-timestamp-offset", "-10m"}, io.Discard)
	if err != nil || code != 2 {
		t.Fatalf("code=%d err=%v, want 2/nil", code, err)
	}
	if verifyErr == nil || !strings.Contains(verifyErr.Error(), "timestamp outside tolerance") {
		t.Fatalf("verify error = %v", verifyErr)
	}
}

func TestRunErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	noOrder := filepath.Join(dir, "no_order.json")
	_ = os.WriteFile(noOrder, []byte(`{"event_id":"e"}`), 0o600)

	tests := []struct {
		name string
		args []string
		want string
	}{
		{"missing file flag", []string{"-secret", secret}, "-file is required"},
		{"missing secret", []string{"-file", noOrder, "-secret", ""}, "no secret"},
		{"order override without order", []string{"-file", noOrder, "-secret", secret, "-order-id", "x"}, "no order object"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := run(tt.args, io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestFirstSecret(t *testing.T) {
	t.Parallel()
	if got := firstSecret(" a , b"); got != "a" {
		t.Fatalf("got %q", got)
	}
}
