// Package booking owns the booking state machine (§5.6) and seat accounting.
//
//	Ride:       DRAFT -> PUBLISHED -> COMPLETED | CANCELLED
//	Booking:    PENDING_PAYMENT -> CONFIRMED -> COMPLETED
//	                          \-> EXPIRED      \-> CANCELLED
//	Verification: NOT_STARTED -> CONSENT_GIVEN -> DIGILOCKER_AUTH ->
//	              DOCS_FETCHED -> AUTO_VERIFIED | MANUAL_REVIEW -> EXPIRED
package booking

import "fmt"

// Ride states.
const (
	RideDraft     = "DRAFT"
	RidePublished = "PUBLISHED"
	RideCompleted = "COMPLETED"
	RideCancelled = "CANCELLED"
)

// Booking states.
const (
	BookingPending   = "PENDING_PAYMENT"
	BookingConfirmed = "CONFIRMED"
	BookingCancelled = "CANCELLED"
	BookingCompleted = "COMPLETED"
	BookingExpired   = "EXPIRED"
)

// Verification states.
const (
	VerifNotStarted   = "NOT_STARTED"
	VerifConsentGiven = "CONSENT_GIVEN"
	VerifDigiAuth     = "DIGILOCKER_AUTH"
	VerifDocsFetched  = "DOCS_FETCHED"
	VerifAutoVerified = "AUTO_VERIFIED"
	VerifManualReview = "MANUAL_REVIEW"
	VerifExpired      = "EXPIRED"
)

// rideTransitions is the allowed ride graph.
var rideTransitions = map[string]map[string]bool{
	RideDraft:     {RidePublished: true, RideCancelled: true},
	RidePublished: {RideCompleted: true, RideCancelled: true},
	RideCompleted: {},
	RideCancelled: {},
}

// bookingTransitions is the allowed booking graph (§5.6).
var bookingTransitions = map[string]map[string]bool{
	BookingPending:   {BookingConfirmed: true, BookingExpired: true, BookingCancelled: true},
	BookingConfirmed: {BookingCancelled: true, BookingCompleted: true},
	BookingCancelled: {},
	BookingCompleted: {},
	BookingExpired:   {},
}

// verificationTransitions is the allowed verification graph (§10).
var verificationTransitions = map[string]map[string]bool{
	VerifNotStarted:   {VerifConsentGiven: true},
	VerifConsentGiven: {VerifDigiAuth: true},
	VerifDigiAuth:     {VerifDocsFetched: true},
	VerifDocsFetched:  {VerifAutoVerified: true, VerifManualReview: true},
	VerifAutoVerified: {VerifExpired: true},
	VerifManualReview: {VerifAutoVerified: true, VerifExpired: true},
	VerifExpired:      {VerifConsentGiven: true}, // re-verification
}

// CanTransition reports whether from -> to is a legal state change.
// Unknown states are always rejected (fail closed).
func CanTransition(kind, from, to string) bool {
	var table map[string]map[string]bool
	switch kind {
	case "ride":
		table = rideTransitions
	case "booking":
		table = bookingTransitions
	case "verification":
		table = verificationTransitions
	default:
		return false
	}
	next, ok := table[from]
	if !ok {
		return false
	}
	return next[to]
}

// Transition validates and returns the new state or a descriptive error
// suitable for a 409 problem+json response.
func Transition(kind, from, to string) (string, error) {
	if !CanTransition(kind, from, to) {
		return from, fmt.Errorf("illegal %s transition %s -> %s", kind, from, to)
	}
	return to, nil
}

// SeatsFree computes remaining seats with an underflow guard.
func SeatsFree(total, booked int) int {
	free := total - booked
	if free < 0 {
		return 0
	}
	return free
}
