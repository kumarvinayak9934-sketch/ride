package payment

import (
	"strings"
	"testing"
	"time"
)

var testSecret = []byte("test-webhook-secret")

// Round-trip: what a provider computes must satisfy ValidSignature.
func TestSignatureRoundTrip(t *testing.T) {
	body := `{"event_id":"evt_1","type":"payment.captured","booking_id":"b-1","amount":123.45,"ts":1700000000}`
	ts := "1700000000"
	sig := VerifySignature(testSecret, ts, body)
	if len(sig) != 64 {
		t.Fatalf("hex sha256 must be 64 chars, got %d", len(sig))
	}
	now := time.Unix(1700000000, 0)
	if err := ValidSignature(testSecret, ts, body, sig, now); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	// case-insensitive hex compare
	if err := ValidSignature(testSecret, ts, body, strings.ToUpper(sig), now); err != nil {
		t.Fatalf("uppercase hex rejected: %v", err)
	}
}

func TestSignatureRejects(t *testing.T) {
	body := `{"event_id":"evt_1"}`
	ts := "1700000000"
	sig := VerifySignature(testSecret, ts, body)
	now := time.Unix(1700000000, 0)

	cases := []struct {
		name   string
		secret []byte
		ts     string
		body   string
		sig    string
		now    time.Time
	}{
		{"wrong secret", []byte("other"), ts, body, sig, now},
		{"tampered body", testSecret, ts, `{"event_id":"evt_2"}`, sig, now},
		{"wrong ts", testSecret, "1700000001", body, sig, now},
		{"garbage sig", testSecret, ts, body, "deadbeef", now},
		{"empty sig", testSecret, ts, body, "", now},
		{"empty ts", testSecret, "", body, sig, now},
		{"non-numeric ts", testSecret, "abc", body, sig, now},
		// freshness: ±5 min window (spec §5.5) — 401 s ahead is outside
		{"too old", testSecret, "1699996900", body,
			VerifySignature(testSecret, "1699996900", body), now},
		{"too new", testSecret, "1700000401", body,
			VerifySignature(testSecret, "1700000401", body), now},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := ValidSignature(c.secret, c.ts, c.body, c.sig, c.now); err == nil {
				t.Fatal("expected rejection, got nil")
			}
		})
	}

	// boundary: exactly 300 s old is OK, 301 s is not
	okSig := VerifySignature(testSecret, "1699999700", body)
	if err := ValidSignature(testSecret, "1699999700", body, okSig, now); err != nil {
		t.Fatalf("300s-old timestamp must pass: %v", err)
	}
	late := time.Unix(1700000000+301, 0)
	if err := ValidSignature(testSecret, "1699999700", body, okSig, late); err == nil {
		t.Fatal("301s-old timestamp must fail")
	}
}

// Signature must actually depend on the secret (not a constant).
func TestSignatureDependsOnSecret(t *testing.T) {
	body := "{}"
	a := VerifySignature([]byte("s1"), "1", body)
	b := VerifySignature([]byte("s2"), "1", body)
	if a == b {
		t.Fatal("different secrets produced identical signatures")
	}
}

func TestStatusFor(t *testing.T) {
	if statusFor("payment.captured") != "CAPTURED" ||
		statusFor("payment.failed") != "FAILED" ||
		statusFor("payment.refunded") != "REFUNDED" ||
		statusFor("weird") != "CREATED" {
		t.Fatal("status mapping wrong")
	}
}

// The ledger pair must balance: sum(debit) == sum(credit) per transaction.
func TestLedgerMath(t *testing.T) {
	amount := 100.0
	driver := round2(amount * 0.85)
	fee := round2(amount - driver)
	if driver+fee != amount {
		t.Fatalf("split does not balance: %v + %v != %v", driver, fee, amount)
	}
	// tiny amounts still balance (paise-level rounding)
	for _, amt := range []float64{0.01, 1.0, 19.99, 40, 1234.56} {
		d := round2(amt * 0.85)
		f := round2(amt - d)
		if round2(d+f) != round2(amt) {
			t.Fatalf("imbalance at %v: %v+%v", amt, d, f)
		}
	}
}
