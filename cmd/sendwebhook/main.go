// Command sendwebhook signs a reference-format JSON file and POSTs it to the
// API. It is a local development and testing tool, not part of the service.
//
//	go run ./cmd/sendwebhook -file docs/reference-webhook/order_completed.json
//	go run ./cmd/sendwebhook -file ... -event-id evt-2 -order-id ORD-2
//
// The secret defaults to the first entry of WEBHOOK_SECRET_REFERENCE.
// Exit code: 0 for 2xx, 2 for any other HTTP status, 1 for local errors.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/burakkuru5534/case-study-dining/internal/ingestion/reference"
)

func main() {
	code, err := run(os.Args[1:], os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sendwebhook:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func run(args []string, out io.Writer) (int, error) {
	fs := flag.NewFlagSet("sendwebhook", flag.ContinueOnError)
	url := fs.String("url", "http://localhost:8080/webhooks/reference", "webhook endpoint")
	file := fs.String("file", "", "JSON payload file (required)")
	secret := fs.String("secret", firstSecret(os.Getenv("WEBHOOK_SECRET_REFERENCE")), "HMAC secret (default: first entry of WEBHOOK_SECRET_REFERENCE)")
	eventID := fs.String("event-id", "", "override event_id")
	orderID := fs.String("order-id", "", "override order.order_id")
	skew := fs.Duration("timestamp-offset", 0, "shift the signature timestamp, e.g. -10m to test replay rejection")
	if err := fs.Parse(args); err != nil {
		return 0, err
	}
	if *file == "" {
		return 0, errors.New("-file is required")
	}
	if *secret == "" {
		return 0, errors.New("no secret: set -secret or WEBHOOK_SECRET_REFERENCE")
	}

	body, err := os.ReadFile(*file)
	if err != nil {
		return 0, err
	}
	if *eventID != "" || *orderID != "" {
		if body, err = override(body, *eventID, *orderID); err != nil {
			return 0, err
		}
	}

	ts := time.Now().Add(*skew).Unix()
	req, err := http.NewRequest(http.MethodPost, *url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(reference.HeaderTimestamp, strconv.FormatInt(ts, 10))
	req.Header.Set(reference.HeaderSignature, reference.Sign([]byte(*secret), ts, body))

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	fmt.Fprintf(out, "%s\n%s", resp.Status, respBody)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return 2, nil
	}
	return 0, nil
}

// override replaces event_id and/or order.order_id. Numbers are kept exact.
func override(body []byte, eventID, orderID string) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("override needs a JSON object: %w", err)
	}
	if eventID != "" {
		m["event_id"] = eventID
	}
	if orderID != "" {
		o, ok := m["order"].(map[string]any)
		if !ok {
			return nil, errors.New("-order-id: payload has no order object")
		}
		o["order_id"] = orderID
	}
	return json.Marshal(m)
}

func firstSecret(list string) string {
	first, _, _ := strings.Cut(list, ",")
	return strings.TrimSpace(first)
}
