package booking

import "testing"

// Exhaustively walks every (kind, from, to) pair and pins the spec's legal
// edges plus the illegal ones — this is the §12 state-machine test.
func TestTransitionGraph(t *testing.T) {
	type edge struct{ kind, from, to string }
	legal := []edge{
		{"ride", RideDraft, RidePublished},
		{"ride", RideDraft, RideCancelled},
		{"ride", RidePublished, RideCompleted},
		{"ride", RidePublished, RideCancelled},
		{"booking", BookingPending, BookingConfirmed},
		{"booking", BookingPending, BookingExpired},
		{"booking", BookingPending, BookingCancelled},
		{"booking", BookingConfirmed, BookingCancelled},
		{"booking", BookingConfirmed, BookingCompleted},
		{"verification", VerifNotStarted, VerifConsentGiven},
		{"verification", VerifConsentGiven, VerifDigiAuth},
		{"verification", VerifDigiAuth, VerifDocsFetched},
		{"verification", VerifDocsFetched, VerifAutoVerified},
		{"verification", VerifDocsFetched, VerifManualReview},
		{"verification", VerifAutoVerified, VerifExpired},
		{"verification", VerifManualReview, VerifAutoVerified},
		{"verification", VerifManualReview, VerifExpired},
		{"verification", VerifExpired, VerifConsentGiven},
	}
	for _, e := range legal {
		if !CanTransition(e.kind, e.from, e.to) {
			t.Errorf("expected legal: %s %s -> %s", e.kind, e.from, e.to)
		}
	}

	illegal := []edge{
		{"ride", RideCompleted, RidePublished}, // no zombie rides
		{"ride", RideCancelled, RideCompleted},
		{"ride", RidePublished, RideDraft},
		{"booking", BookingCompleted, BookingConfirmed}, // no un-complete
		{"booking", BookingExpired, BookingConfirmed},   // expired is terminal
		{"booking", BookingCancelled, BookingConfirmed},
		{"booking", BookingConfirmed, BookingPending},
		{"verification", VerifNotStarted, VerifAutoVerified}, // skip the flow
		{"verification", VerifAutoVerified, VerifManualReview},
		{"verification", VerifExpired, VerifAutoVerified},
		{"booking", BookingPending, BookingCompleted}, // payment must confirm first
	}
	for _, e := range illegal {
		if CanTransition(e.kind, e.from, e.to) {
			t.Errorf("expected illegal: %s %s -> %s", e.kind, e.from, e.to)
		}
	}

	// unknown states fail closed
	for _, kind := range []string{"ride", "booking", "verification"} {
		if CanTransition(kind, "BOGUS", RidePublished) {
			t.Errorf("%s: unknown source state must be rejected", kind)
		}
	}
	if CanTransition("nonsense", RideDraft, RidePublished) {
		t.Error("unknown machine must be rejected")
	}
}

func TestTransitionReturnsError(t *testing.T) {
	if _, err := Transition("booking", BookingCompleted, BookingConfirmed); err == nil {
		t.Fatal("expected error on illegal transition")
	}
	s, err := Transition("booking", BookingPending, BookingConfirmed)
	if err != nil || s != BookingConfirmed {
		t.Fatalf("got %q, %v", s, err)
	}
}

func TestSeatsFree(t *testing.T) {
	cases := []struct{ total, booked, want int }{
		{4, 0, 4}, {4, 3, 1}, {4, 4, 0}, {4, 9, 0}, {0, 0, 0},
	}
	for _, c := range cases {
		if got := SeatsFree(c.total, c.booked); got != c.want {
			t.Errorf("SeatsFree(%d,%d) = %d, want %d", c.total, c.booked, got, c.want)
		}
	}
}
