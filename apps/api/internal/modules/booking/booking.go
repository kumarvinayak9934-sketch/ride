// Package booking implements §5.4 booking creation with the mandated order:
//
//  1. validate body
//  2. Redis SET NX seat holds (30 s TTL)
//  3. open tx; SELECT ride_instance FOR UPDATE
//  4. reject over-capacity (409) and vehicle-not-private
//  5. insert booking (PENDING_PAYMENT) + outbox booking.created — same tx
//  6. commit; create payment intent (outbox → provider)
//
// Idempotency-Key is required (middleware) AND enforced by the DB unique
// constraint (passenger_id, idempotency_key).
package booking

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kumarvinayak9934-sketch/ride/apps/api/internal/cache"
	"github.com/kumarvinayak9934-sketch/ride/apps/api/internal/middleware"
	"github.com/kumarvinayak9934-sketch/ride/apps/api/internal/modules/pricing"
	"github.com/kumarvinayak9934-sketch/ride/apps/api/internal/outbox"
)

// Handler serves booking endpoints.
type Handler struct {
	pool   *pgxpool.Pool
	cache  *cache.Cache
	outbox *outbox.Store
	engine *pricing.Engine
	log    *slog.Logger
	// webhookSecret signs the outbound payment.captured simulation.
	webhookSecret []byte
	upiVPA        string
}

func NewHandler(pool *pgxpool.Pool, c *cache.Cache, ob *outbox.Store,
	engine *pricing.Engine, log *slog.Logger, webhookSecret []byte, upiVPA string) *Handler {
	return &Handler{pool: pool, cache: c, outbox: ob, engine: engine,
		log: log, webhookSecret: webhookSecret, upiVPA: upiVPA}
}

// Routes mounts booking endpoints. Idempotency-Key middleware is applied by
// the caller (scope = booking:<user>).
func (h *Handler) Routes(r chi.Router) {
	r.Post("/", h.Create)
	r.Get("/", h.ListMine)
	r.Get("/{id}", h.Get)
	r.Post("/{id}/cancel", h.Cancel)
}

// CreateRequest is POST /v1/bookings.
type CreateRequest struct {
	RideInstanceID string `json:"ride_instance_id"`
	Seats          int    `json:"seats"`
}

// Booking is the resource returned to clients.
type Booking struct {
	ID             string    `json:"id"`
	RideInstanceID string    `json:"ride_instance_id"`
	PassengerID    string    `json:"passenger_id"`
	Seats          int       `json:"seats"`
	Status         string    `json:"status"`
	Fare           float64   `json:"fare"`
	Total          float64   `json:"total"`
	IdempotencyKey string    `json:"idempotency_key,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	Payment        *Payment  `json:"payment,omitempty"`
}

// Payment is the intent returned right after booking creation.
type Payment struct {
	ID        string  `json:"id"`
	Amount    float64 `json:"amount"`
	Status    string  `json:"status"`
	Provider  string  `json:"provider"`
	UPIIntent string  `json:"upi_intent"`
}

// Create handles POST /v1/bookings — the §5.4 six-step flow.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserID(r.Context())
	uid, err := uuid.Parse(userID)
	if err != nil {
		middleware.WriteProblem(w, http.StatusUnauthorized, "Unauthorized", "login required")
		return
	}
	var req CreateRequest
	if !middleware.DecodeJSON(w, r, &req) {
		return
	}
	if req.Seats < 1 || req.Seats > 11 {
		middleware.WriteProblem(w, http.StatusBadRequest, "Validation Failed", "seats must be 1..11")
		return
	}
	instanceID, err := uuid.Parse(req.RideInstanceID)
	if err != nil {
		middleware.WriteProblem(w, http.StatusBadRequest, "Validation Failed", "ride_instance_id must be a UUID")
		return
	}
	idemKey := r.Header.Get("Idempotency-Key") // enforced by middleware; belt & braces

	ctx := r.Context()

	// Fast pre-read of booked seats so concurrent passengers lock DISJOINT
	// absolute seat indices (spec §5.4 step 2). The authoritative capacity
	// check still happens under SELECT ... FOR UPDATE inside the tx.
	var preBooked int
	if err := h.pool.QueryRow(ctx,
		`SELECT seats_booked FROM ride_instance WHERE id=$1`, instanceID).
		Scan(&preBooked); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			middleware.WriteProblem(w, http.StatusNotFound, "Ride Not Found", "no such ride instance")
			return
		}
		middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "ride lookup failed")
		return
	}
	seatIdx := make([]int, req.Seats)
	for i := range seatIdx {
		seatIdx[i] = preBooked + i // absolute indices: [booked, booked+n)
	}
	if err := h.cache.AcquireSeats(ctx, instanceID.String(), seatIdx); err != nil {
		if errors.Is(err, cache.ErrLocked) {
			middleware.WriteProblem(w, http.StatusConflict, "Seat Contended",
				"another booking holds this seat, retry shortly")
			return
		}
		h.log.Warn("seat lock unavailable", slog.String("err", err.Error()))
		middleware.WriteProblem(w, http.StatusServiceUnavailable, "Service Unavailable", "retry shortly")
		return
	}
	// locks released after the tx ends (success or failure)
	defer h.cache.ReleaseSeats(ctx, instanceID.String(), seatIdx)

	// 3. tx + row lock
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "tx start failed")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var seats, booked int
	var routeID uuid.UUID
	var driverID uuid.UUID
	var vehicleSeats int
	var isPrivate bool
	err = tx.QueryRow(ctx, `
		SELECT ri.seats_booked, ri.route_id, ri.driver_id, r.seats,
		       v.seats_available, v.is_private
		FROM ride_instance ri
		JOIN route r ON r.id = ri.route_id
		JOIN vehicle v ON v.id = r.vehicle_id
		WHERE ri.id = $1
		FOR UPDATE OF ri`, instanceID).
		Scan(&booked, &routeID, &driverID, &seats, &vehicleSeats, &isPrivate)
	if errors.Is(err, pgx.ErrNoRows) {
		middleware.WriteProblem(w, http.StatusNotFound, "Ride Not Found", "no such ride instance")
		return
	}
	if err != nil {
		h.log.Error("lock ride failed", slog.String("err", err.Error()))
		middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "lock failed")
		return
	}

	// 4a. private vehicle only (§1.15)
	if !isPrivate {
		middleware.WriteProblem(w, http.StatusUnprocessableEntity, "Vehicle Not Pooled",
			"only private vehicles can be pooled")
		return
	}
	// 4b. over-capacity guard
	if booked+req.Seats > seats {
		middleware.WriteProblem(w, http.StatusConflict, "Not Enough Seats",
			fmt.Sprintf("%d of %d seats already booked", booked, seats))
		return
	}
	// passenger can't double-book the same instance (any key)
	var dup int
	_ = tx.QueryRow(ctx,
		`SELECT count(*) FROM booking WHERE ride_instance_id=$1 AND passenger_id=$2
		 AND status IN ('PENDING_PAYMENT','CONFIRMED')`,
		instanceID, uid).Scan(&dup)
	if dup > 0 {
		middleware.WriteProblem(w, http.StatusConflict, "Already Booked",
			"you already hold a booking on this ride")
		return
	}

	// price via §5.3 engine (seat count + current surge from free seats)
	var distKm, durationMin, priceEst float64
	if err := tx.QueryRow(ctx,
		`SELECT distance_km, duration_min, price_estimate FROM route WHERE id=$1`, routeID).
		Scan(&distKm, &durationMin, &priceEst); err != nil {
		middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "route lookup failed")
		return
	}
	free := SeatsFree(seats, booked)
	bd := h.engine.Compute(pricing.Input{
		DistanceKm:  distKm,
		DurationMin: durationMin,
		Demand:      1,
		Supply:      free,
		Seats:       seats,
		Location:    "booking",
	})

	// seat count for multi-seat bookings: per-seat totals × seats
	total := pricing.Money(bd.Total * float64(req.Seats))

	// 5. booking row + outbox event in the SAME transaction
	var bookingID uuid.UUID
	var createdAt time.Time
	err = tx.QueryRow(ctx, `
		INSERT INTO booking (ride_instance_id, passenger_id, seats, status, fare, idempotency_key)
		VALUES ($1,$2,$3,'PENDING_PAYMENT',$4,$5)
		RETURNING id, created_at`,
		instanceID, uid, req.Seats, bd.Fare, idemKey).
		Scan(&bookingID, &createdAt)
	if err != nil {
		if isUniqueViolation(err) {
			// replay against the DB constraint: return the existing booking
			if existing, ok := h.findByKey(ctx, uid, idemKey); ok {
				writeJSON(w, http.StatusOK, existing)
				return
			}
			middleware.WriteProblem(w, http.StatusConflict, "Idempotency Conflict",
				"an existing booking uses this key")
			return
		}
		h.log.Error("insert booking failed", slog.String("err", err.Error()))
		middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "insert failed")
		return
	}

	// seats_booked increment (guarded by the FOR UPDATE above)
	if _, err := tx.Exec(ctx,
		`UPDATE ride_instance SET seats_booked = seats_booked + $2 WHERE id=$1`,
		instanceID, req.Seats); err != nil {
		middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "seat update failed")
		return
	}

	if err := h.outbox.Enqueue(ctx, tx, "booking.created", map[string]any{
		"booking_id": bookingID.String(),
		"ride_id":    instanceID.String(),
		"passenger":  uid.String(),
		"seats":      req.Seats,
		"fare":       bd.Fare, // per seat
		"total":      total,   // grand total incl. fees + GST
		"created_at": createdAt.UTC().Format(time.RFC3339),
	}); err != nil {
		h.log.Error("outbox enqueue failed", slog.String("err", err.Error()))
		middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "outbox failed")
		return
	}

	// payment intent row (same tx so we never commit a booking without one)
	payID := uuid.NewString()
	upiIntent := fmt.Sprintf("upi://pay?pa=%s&pn=Safar%%20Share&am=%.2f&cu=INR&tr=%s",
		h.upiVPA, total, payID)
	if _, err := tx.Exec(ctx, `
		INSERT INTO payment (id, booking_id, amount, status, provider, upi_intent)
		VALUES ($1,$2,$3,'CREATED','upi',$4)`,
		payID, bookingID, total, upiIntent); err != nil {
		h.log.Error("payment insert failed", slog.String("err", err.Error()))
		middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "payment failed")
		return
	}

	if err := tx.Commit(ctx); err != nil {
		if isUniqueViolation(err) {
			if existing, ok := h.findByKey(ctx, uid, idemKey); ok {
				writeJSON(w, http.StatusOK, existing)
				return
			}
		}
		middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "commit failed")
		return
	}

	// 6. respond with booking + intent
	writeJSON(w, http.StatusCreated, Booking{
		ID: bookingID.String(), RideInstanceID: instanceID.String(),
		PassengerID: uid.String(), Seats: req.Seats, Status: BookingPending,
		Fare: bd.Fare, Total: total, IdempotencyKey: idemKey, CreatedAt: createdAt,
		Payment: &Payment{
			ID: payID, Amount: total, Status: "CREATED",
			Provider: "upi", UPIIntent: upiIntent,
		},
	})
}

// Get fetches one booking (owner or driver only).
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	b, ok := h.load(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, b)
}

// ListMine lists the caller's bookings, newest first.
func (h *Handler) ListMine(w http.ResponseWriter, r *http.Request) {
	uid, err := uuid.Parse(middleware.UserID(r.Context()))
	if err != nil {
		middleware.WriteProblem(w, http.StatusUnauthorized, "Unauthorized", "login required")
		return
	}
	rows, err := h.pool.Query(r.Context(), `
		SELECT b.id::text, b.ride_instance_id::text, b.passenger_id::text, b.seats, b.status,
		       b.fare, b.created_at, p.id::text, p.amount, p.status, p.upi_intent
		FROM booking b
		LEFT JOIN payment p ON p.booking_id = b.id
		WHERE b.passenger_id=$1
		ORDER BY b.created_at DESC LIMIT 50`, uid)
	if err != nil {
		middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "query failed")
		return
	}
	defer rows.Close()
	out := []Booking{}
	for rows.Next() {
		var b Booking
		var pay Payment
		var payID, payStatus, upi *string
		var payAmt *float64
		if err := rows.Scan(&b.ID, &b.RideInstanceID, &b.PassengerID, &b.Seats, &b.Status,
			&b.Fare, &b.CreatedAt, &payID, &payAmt, &payStatus, &upi); err != nil {
			middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "scan failed")
			return
		}
		if payID != nil {
			pay = Payment{ID: *payID, Amount: *payAmt, Status: *payStatus, Provider: "upi", UPIIntent: deref(upi)}
			b.Payment = &pay
		}
		out = append(out, b)
	}
	writeJSON(w, http.StatusOK, map[string]any{"bookings": out})
}

// Cancel transitions CONFIRMED -> CANCELLED (guarded), releasing seats.
func (h *Handler) Cancel(w http.ResponseWriter, r *http.Request) {
	b, ok := h.load(w, r)
	if !ok {
		return
	}
	uid := middleware.UserID(r.Context())
	ctx := r.Context()

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "tx failed")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status string
	if err := tx.QueryRow(ctx,
		`SELECT status FROM booking WHERE id=$1 FOR UPDATE`, uuidMust(b.ID)).
		Scan(&status); err != nil {
		middleware.WriteProblem(w, http.StatusNotFound, "Not Found", "booking missing")
		return
	}
	if b.PassengerID != uid { // driver cancel is a future endpoint
		middleware.WriteProblem(w, http.StatusForbidden, "Forbidden", "only the passenger can cancel")
		return
	}
	next, err := Transition("booking", status, BookingCancelled)
	if err != nil {
		middleware.WriteProblem(w, http.StatusConflict, "Illegal State Transition", err.Error())
		return
	}
	if _, err := tx.Exec(ctx,
		`UPDATE booking SET status=$2, updated_at=now() WHERE id=$1`,
		uuidMust(b.ID), next); err != nil {
		middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "update failed")
		return
	}
	// release seats
	if _, err := tx.Exec(ctx,
		`UPDATE ride_instance SET seats_booked = GREATEST(seats_booked - $2, 0) WHERE id=$1`,
		uuidMust(b.RideInstanceID), b.Seats); err != nil {
		middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "seat release failed")
		return
	}
	// refund path marker for the ledger (payment.refund outbox event)
	if err := h.outbox.Enqueue(ctx, tx, "payment.refund", map[string]any{
		"booking_id": b.ID, "seats": b.Seats,
	}); err != nil {
		middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "outbox failed")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "commit failed")
		return
	}
	b.Status = BookingCancelled
	writeJSON(w, http.StatusOK, b)
}

// ---------------------------------------------------------- helpers --------

func (h *Handler) load(w http.ResponseWriter, r *http.Request) (Booking, bool) {
	idStr := chi.URLParam(r, "id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		middleware.WriteProblem(w, http.StatusBadRequest, "Validation Failed", "bad booking id")
		return Booking{}, false
	}
	uid := middleware.UserID(r.Context())
	var b Booking
	var payID *string
	var payAmt *float64
	var payStatus, upi *string
	err = h.pool.QueryRow(r.Context(), `
		SELECT b.id::text, b.ride_instance_id::text, b.passenger_id::text, b.seats, b.status,
		       b.fare, b.created_at, p.id::text, p.amount, p.status, p.upi_intent
		FROM booking b
		LEFT JOIN payment p ON p.booking_id = b.id
		WHERE b.id=$1`, id).
		Scan(&b.ID, &b.RideInstanceID, &b.PassengerID, &b.Seats, &b.Status,
			&b.Fare, &b.CreatedAt, &payID, &payAmt, &payStatus, &upi)
	if errors.Is(err, pgx.ErrNoRows) {
		middleware.WriteProblem(w, http.StatusNotFound, "Not Found", "booking does not exist")
		return Booking{}, false
	}
	if err != nil {
		middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "lookup failed")
		return Booking{}, false
	}
	// visibility: passenger or driver of the ride
	if b.PassengerID != uid {
		var driverID string
		_ = h.pool.QueryRow(r.Context(),
			`SELECT ri.driver_id::text FROM ride_instance ri WHERE ri.id=$1`,
			uuidMust(b.RideInstanceID)).Scan(&driverID)
		if driverID != uid {
			middleware.WriteProblem(w, http.StatusForbidden, "Forbidden", "not your booking")
			return Booking{}, false
		}
	}
	if payID != nil {
		b.Payment = &Payment{ID: *payID, Amount: derefF(payAmt), Status: deref(payStatus),
			Provider: "upi", UPIIntent: deref(upi)}
	}
	return b, true
}

func (h *Handler) findByKey(ctx context.Context, uid uuid.UUID, key string) (Booking, bool) {
	var b Booking
	err := h.pool.QueryRow(ctx, `
		SELECT id::text, ride_instance_id::text, passenger_id::text, seats, status,
		       fare, created_at
		FROM booking WHERE passenger_id=$1 AND idempotency_key=$2`,
		uid, key).Scan(&b.ID, &b.RideInstanceID, &b.PassengerID, &b.Seats, &b.Status,
		&b.Fare, &b.CreatedAt)
	if err != nil {
		return Booking{}, false
	}
	return b, true
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "duplicate key") ||
		strings.Contains(s, "unique constraint") ||
		strings.Contains(s, "SQLSTATE 23505")
}

func uuidMust(s string) uuid.UUID {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil
	}
	return id
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefF(f *float64) float64 {
	if f == nil {
		return 0
	}
	return *f
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
