package ingestion

import (
	"math/rand/v2"
	"time"
)

// Backoff computes the delay before retrying a transiently failed event:
// Base * 2^(attempt-1), capped at Max, then randomized by ±Jitter so events
// that failed together do not all retry at the same instant.
type Backoff struct {
	Base   time.Duration
	Max    time.Duration
	Jitter float64        // 0.2 = ±20%
	Rand   func() float64 // [0,1); nil means math/rand/v2
}

// DefaultBackoff: 5s, 10s, 20s ... capped at 1h, ±20%.
var DefaultBackoff = Backoff{Base: 5 * time.Second, Max: time.Hour, Jitter: 0.2}

// Delay returns the wait after the attempt-th failure (attempt >= 1).
func (b Backoff) Delay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := b.Base
	// Doubling stops at Max, so large attempt numbers cannot overflow.
	for i := 1; i < attempt && d < b.Max; i++ {
		d *= 2
	}
	if d > b.Max {
		d = b.Max
	}
	if b.Jitter > 0 {
		r := rand.Float64
		if b.Rand != nil {
			r = b.Rand
		}
		// Float math only for timing, never for money.
		d = time.Duration(float64(d) * (1 + b.Jitter*(2*r()-1)))
	}
	return d
}
