package ingestion

import (
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/burakkuru5534/case-study-dining/internal/loyalty"
	loyaltystore "github.com/burakkuru5534/case-study-dining/internal/loyalty/pgstore"
)

func TestBackoffDelay(t *testing.T) {
	t.Parallel()

	noJitter := Backoff{Base: 5 * time.Second, Max: time.Hour}
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 5 * time.Second}, // clamped to 1
		{1, 5 * time.Second},
		{2, 10 * time.Second},
		{3, 20 * time.Second},
		{10, 2560 * time.Second},
		{11, time.Hour}, // 5120s capped
		{1000, time.Hour},
		{math.MaxInt, time.Hour}, // no overflow
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.attempt), func(t *testing.T) {
			t.Parallel()
			if got := noJitter.Delay(tt.attempt); got != tt.want {
				t.Fatalf("Delay(%d) = %s, want %s", tt.attempt, got, tt.want)
			}
		})
	}
}

func TestBackoffJitterBounds(t *testing.T) {
	t.Parallel()

	at := func(r float64) Backoff {
		return Backoff{Base: 10 * time.Second, Max: time.Hour, Jitter: 0.2, Rand: func() float64 { return r }}
	}
	if got := at(0).Delay(1); got != 8*time.Second {
		t.Errorf("min jitter = %s, want 8s", got)
	}
	if got := at(0.5).Delay(1); got != 10*time.Second {
		t.Errorf("mid jitter = %s, want 10s", got)
	}
	if got := at(0.999999).Delay(1); got < 11999*time.Millisecond || got >= 12*time.Second {
		t.Errorf("max jitter = %s, want just under 12s", got)
	}

	// Real randomness stays within ±20%.
	real := Backoff{Base: 10 * time.Second, Max: time.Hour, Jitter: 0.2}
	for range 1000 {
		if d := real.Delay(1); d < 8*time.Second || d > 12*time.Second {
			t.Fatalf("delay %s outside ±20%%", d)
		}
	}
}

func TestDefaultBackoffTotalRetryWindow(t *testing.T) {
	t.Parallel()
	b := DefaultBackoff
	b.Jitter = 0
	var total time.Duration
	for attempt := 1; attempt < 10; attempt++ { // 9 waits between 10 attempts
		total += b.Delay(attempt)
	}
	if total != 2555*time.Second { // 5+10+...+1280
		t.Fatalf("total = %s", total)
	}
}

func TestIsPermanent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		err  error
		want bool
	}{
		{fmt.Errorf("%w: order.currency", ErrInvalidPayload), true},
		{fmt.Errorf("%w \"x\"", ErrUnknownProvider), true},
		{fmt.Errorf("%w (tenant_ref %q)", ErrUnknownRestaurant, "t"), true},
		{fmt.Errorf("%w: earn_unit_minor", loyalty.ErrInvalidRule), true},
		{loyalty.ErrPointsOverflow, true},
		{fmt.Errorf("%w: negative", loyalty.ErrInvalidPurchase), true},
		{loyaltystore.ErrIdentityRace, false},
		{errors.New("ERROR: canceling statement due to lock timeout (SQLSTATE 55P03)"), false},
		{errors.New("connection reset by peer"), false},
	}
	for _, tt := range tests {
		if got := isPermanent(tt.err); got != tt.want {
			t.Errorf("isPermanent(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
}

func TestSanitizeErrorTruncates(t *testing.T) {
	t.Parallel()
	long := errors.New(string(make([]byte, 2000)))
	if got := sanitizeError(long); len(got) > maxStoredErrorLength {
		t.Fatalf("len = %d", len(got))
	}
	// Truncation must not leave a broken multi-byte rune.
	multi := errors.New(fmt.Sprintf("%0499d", 0) + "ş")
	if got := sanitizeError(multi); got != fmt.Sprintf("%0499d", 0) {
		t.Fatalf("got %q", got[len(got)-3:])
	}
}
