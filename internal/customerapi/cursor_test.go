package customerapi

import (
	"encoding/base64"
	"testing"
	"time"

	orderstore "github.com/burakkuru5534/case-study-dining/internal/order/pgstore"
)

func TestCursorRoundTripKeepsMicroseconds(t *testing.T) {
	t.Parallel()
	in := orderstore.Position{CompletedAt: time.Date(2026, 11, 5, 9, 30, 0, 123456000, time.UTC), ID: 42}
	out, err := decodeCursor(encodeCursor(in))
	if err != nil {
		t.Fatal(err)
	}
	if !out.CompletedAt.Equal(in.CompletedAt) || out.ID != in.ID {
		t.Fatalf("got %+v, want %+v", out, in)
	}
}

func TestDecodeCursorRejectsGarbage(t *testing.T) {
	t.Parallel()
	b64 := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for name, c := range map[string]string{
		"not base64":    "!!!",
		"padded base64": base64.URLEncoding.EncodeToString([]byte(`{"t":"2026-11-05T09:30:00Z","id":1}`)) + "==",
		"not json":      b64("hello"),
		"missing id":    b64(`{"t":"2026-11-05T09:30:00Z"}`),
		"zero id":       b64(`{"t":"2026-11-05T09:30:00Z","id":0}`),
		"negative id":   b64(`{"t":"2026-11-05T09:30:00Z","id":-5}`),
		"missing time":  b64(`{"id":5}`),
		"bad time":      b64(`{"t":"yesterday","id":5}`),
		"string id":     b64(`{"t":"2026-11-05T09:30:00Z","id":"5"}`),
		"too long":      b64(string(make([]byte, 300))),
	} {
		if _, err := decodeCursor(c); err == nil {
			t.Errorf("%s: accepted %q", name, c)
		}
	}
}

func TestParseLimit(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]int{"": DefaultLimit, "1": 1, "100": 100, "37": 37} {
		if got, err := parseLimit(raw); err != nil || got != want {
			t.Errorf("parseLimit(%q) = %d, %v", raw, got, err)
		}
	}
	for _, raw := range []string{"0", "-1", "101", "abc", "1.5", "1e2"} {
		if _, err := parseLimit(raw); err == nil {
			t.Errorf("parseLimit(%q) accepted", raw)
		}
	}
}
