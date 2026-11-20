package reference

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Signature headers of the reference format.
//
//	X-Signature-Timestamp: <unix seconds>
//	X-Signature:           v1=<hex(HMAC-SHA256(secret, "<timestamp>.<raw body>"))>
//
// X-Signature may carry several comma-separated v1 entries, so a sender can
// sign with old and new secrets during rotation.
const (
	HeaderTimestamp  = "X-Signature-Timestamp"
	HeaderSignature  = "X-Signature"
	signatureVersion = "v1"

	// DefaultTolerance bounds clock skew and the replay window. Replays inside
	// the window are absorbed by event_id deduplication in the inbox.
	DefaultTolerance = 5 * time.Minute

	maxSignatureEntries = 8
)

// ErrInvalidSignature is wrapped by every verification failure. The wrapped
// reason is safe to log; it never contains secrets or payload data.
var ErrInvalidSignature = errors.New("invalid webhook signature")

// Sign returns the X-Signature header value for body signed at timestamp.
func Sign(secret []byte, timestamp int64, body []byte) string {
	return signatureVersion + "=" + hex.EncodeToString(mac(secret, timestamp, body))
}

func mac(secret []byte, timestamp int64, body []byte) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(strconv.FormatInt(timestamp, 10)))
	m.Write([]byte("."))
	m.Write(body)
	return m.Sum(nil)
}

// Verifier checks reference-format signatures. It is pure: the clock is injected.
type Verifier struct {
	// Secrets are all currently accepted secrets (rotation).
	Secrets [][]byte
	// Tolerance is the maximum allowed |now - timestamp|. Zero means DefaultTolerance.
	Tolerance time.Duration
	// Now returns the current time. Nil means time.Now.
	Now func() time.Time
}

// Verify checks the signature of the raw body. header returns a request header
// value by name (http.Header.Get fits). It must run before the body is parsed.
func (v Verifier) Verify(header func(string) string, body []byte) error {
	if len(v.Secrets) == 0 {
		// Misconfiguration must fail closed, never accept unsigned requests.
		return fmt.Errorf("%w: no secrets configured", ErrInvalidSignature)
	}

	tsRaw := header(HeaderTimestamp)
	if tsRaw == "" {
		return fmt.Errorf("%w: missing %s header", ErrInvalidSignature, HeaderTimestamp)
	}
	ts, err := strconv.ParseInt(tsRaw, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: malformed %s header", ErrInvalidSignature, HeaderTimestamp)
	}

	now := time.Now
	if v.Now != nil {
		now = v.Now
	}
	tolerance := v.Tolerance
	if tolerance == 0 {
		tolerance = DefaultTolerance
	}
	// Compare in seconds to avoid overflow from huge timestamps in time.Unix math.
	skew := now().Unix() - ts
	if skew < 0 {
		skew = -skew
	}
	if skew < 0 || skew > int64(tolerance/time.Second) {
		return fmt.Errorf("%w: timestamp outside tolerance", ErrInvalidSignature)
	}

	sigRaw := header(HeaderSignature)
	if sigRaw == "" {
		return fmt.Errorf("%w: missing %s header", ErrInvalidSignature, HeaderSignature)
	}

	var candidates [][]byte
	entries := strings.Split(sigRaw, ",")
	if len(entries) > maxSignatureEntries {
		return fmt.Errorf("%w: too many signature entries", ErrInvalidSignature)
	}
	for _, e := range entries {
		version, value, ok := strings.Cut(strings.TrimSpace(e), "=")
		if !ok || version != signatureVersion {
			continue // unknown versions are ignored, not fatal
		}
		decoded, err := hex.DecodeString(value)
		if err != nil || len(decoded) != sha256.Size {
			continue
		}
		candidates = append(candidates, decoded)
	}
	if len(candidates) == 0 {
		return fmt.Errorf("%w: no valid %s entry", ErrInvalidSignature, signatureVersion)
	}

	for _, secret := range v.Secrets {
		expected := mac(secret, ts, body)
		for _, c := range candidates {
			if hmac.Equal(expected, c) { // constant time
				return nil
			}
		}
	}
	return fmt.Errorf("%w: signature mismatch", ErrInvalidSignature)
}
