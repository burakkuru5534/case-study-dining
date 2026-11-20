package reference

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"
)

var (
	secretA = []byte("test-secret-0123456789abcdef0123456789")
	secretB = []byte("other-secret-abcdef0123456789abcdef012")
	body    = []byte(`{"event_id":"evt-1"}`)
	signNow = time.Unix(1763631000, 0)
)

// Computed outside Go:
//
//	printf '%s' '1763631000.{"event_id":"evt-1"}' | openssl dgst -sha256 -hmac 'test-secret-0123456789abcdef0123456789'
//
// Sign and Verify share code, so a bug in both would cancel out; this vector does not.
const knownSignature = "v1=216cf9b7fe7f664b3c4b9703a7c33700364b6d2f311d249c3bcabe0a62158f82"

func TestSignMatchesIndependentVector(t *testing.T) {
	t.Parallel()
	if got := Sign(secretA, signNow.Unix(), body); got != knownSignature {
		t.Fatalf("Sign = %s, want %s", got, knownSignature)
	}
}

func headers(ts, sig string) func(string) string {
	return func(name string) string {
		switch name {
		case HeaderTimestamp:
			return ts
		case HeaderSignature:
			return sig
		}
		return ""
	}
}

func TestVerify(t *testing.T) {
	t.Parallel()

	ts := strconv.FormatInt(signNow.Unix(), 10)
	valid := Sign(secretA, signNow.Unix(), body)
	at := func(d time.Duration) func() time.Time { return func() time.Time { return signNow.Add(d) } }
	verifier := Verifier{Secrets: [][]byte{secretA}, Now: at(0)}

	tests := []struct {
		name    string
		v       Verifier
		header  func(string) string
		body    []byte
		wantErr string // "" = valid
	}{
		{name: "valid", v: verifier, header: headers(ts, valid), body: body},
		{name: "independent vector verifies", v: verifier, header: headers(ts, knownSignature), body: body},
		{name: "upper-case hex accepted", v: verifier, header: headers(ts, "v1="+strings.ToUpper(strings.TrimPrefix(valid, "v1="))), body: body},
		{name: "rotation: old and new secret both accepted (new)", v: Verifier{Secrets: [][]byte{secretB, secretA}, Now: at(0)}, header: headers(ts, valid), body: body},
		{name: "rotation: sender signs with two secrets", v: Verifier{Secrets: [][]byte{secretB}, Now: at(0)}, header: headers(ts, valid+", "+Sign(secretB, signNow.Unix(), body)), body: body},
		{name: "unknown version entries ignored", v: verifier, header: headers(ts, "v0=abc, "+valid), body: body},
		{name: "exactly at tolerance (past)", v: Verifier{Secrets: [][]byte{secretA}, Now: at(DefaultTolerance)}, header: headers(ts, valid), body: body},
		{name: "exactly at tolerance (future)", v: Verifier{Secrets: [][]byte{secretA}, Now: at(-DefaultTolerance)}, header: headers(ts, valid), body: body},
		{name: "custom tolerance", v: Verifier{Secrets: [][]byte{secretA}, Tolerance: time.Hour, Now: at(59 * time.Minute)}, header: headers(ts, valid), body: body},

		{name: "wrong secret", v: Verifier{Secrets: [][]byte{secretB}, Now: at(0)}, header: headers(ts, valid), body: body, wantErr: "signature mismatch"},
		{name: "body tampered by one byte", v: verifier, header: headers(ts, valid), body: []byte(`{"event_id":"evt-2"}`), wantErr: "signature mismatch"},
		{name: "timestamp changed", v: verifier, header: headers(strconv.FormatInt(signNow.Unix()+1, 10), valid), body: body, wantErr: "signature mismatch"},
		{name: "too old", v: Verifier{Secrets: [][]byte{secretA}, Now: at(DefaultTolerance + time.Second)}, header: headers(ts, valid), body: body, wantErr: "timestamp outside tolerance"},
		{name: "too far in future", v: Verifier{Secrets: [][]byte{secretA}, Now: at(-DefaultTolerance - time.Second)}, header: headers(ts, valid), body: body, wantErr: "timestamp outside tolerance"},
		{name: "min int64 timestamp", v: verifier, header: headers(strconv.FormatInt(math.MinInt64, 10), valid), body: body, wantErr: "timestamp outside tolerance"},
		{name: "max int64 timestamp", v: verifier, header: headers(strconv.FormatInt(math.MaxInt64, 10), valid), body: body, wantErr: "timestamp outside tolerance"},
		{name: "missing timestamp", v: verifier, header: headers("", valid), body: body, wantErr: "missing X-Signature-Timestamp"},
		{name: "malformed timestamp", v: verifier, header: headers("yesterday", valid), body: body, wantErr: "malformed X-Signature-Timestamp"},
		{name: "fractional timestamp", v: verifier, header: headers(ts+".5", valid), body: body, wantErr: "malformed X-Signature-Timestamp"},
		{name: "missing signature", v: verifier, header: headers(ts, ""), body: body, wantErr: "missing X-Signature"},
		{name: "no v1 entry", v: verifier, header: headers(ts, "v2=abcd"), body: body, wantErr: "no valid v1 entry"},
		{name: "v1 not hex", v: verifier, header: headers(ts, "v1=not-hex"), body: body, wantErr: "no valid v1 entry"},
		{name: "v1 wrong length", v: verifier, header: headers(ts, "v1=abcd"), body: body, wantErr: "no valid v1 entry"},
		{name: "raw hex without version", v: verifier, header: headers(ts, strings.TrimPrefix(valid, "v1=")), body: body, wantErr: "no valid v1 entry"},
		{name: "too many entries", v: verifier, header: headers(ts, strings.Repeat(valid+",", 9)+valid), body: body, wantErr: "too many signature entries"},
		{name: "no secrets configured fails closed", v: Verifier{Now: at(0)}, header: headers(ts, valid), body: body, wantErr: "no secrets configured"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.v.Verify(tt.header, tt.body)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrInvalidSignature) {
				t.Fatalf("error does not wrap ErrInvalidSignature: %v", err)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestVerifyErrorsDoNotLeakSecretsOrSignatures(t *testing.T) {
	t.Parallel()
	ts := strconv.FormatInt(signNow.Unix(), 10)
	sig := Sign(secretB, signNow.Unix(), body)
	err := Verifier{Secrets: [][]byte{secretA}, Now: func() time.Time { return signNow }}.Verify(headers(ts, sig), body)
	if err == nil {
		t.Fatal("expected error")
	}
	for _, s := range []string{string(secretA), string(secretB), sig, string(body)} {
		if strings.Contains(err.Error(), s) {
			t.Fatalf("error leaks %q: %v", s, err)
		}
	}
}
