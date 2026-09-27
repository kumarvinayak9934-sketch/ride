// Package payment implements §5.5: UPI payment webhooks with HMAC-SHA256
// verification over the raw body, ±5 min timestamp freshness, event_id
// idempotency, and double-entry ledger writes.
package payment

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kumarvinayak9934-sketch/ride/apps/api/internal/middleware"
	"github.com/kumarvinayak9934-sketch/ride/apps/api/internal/modules/booking"
	"github.com/kumarvinayak9934-sketch/ride/apps/api/internal/outbox"
)

// Handler serves payment endpoints.
type Handler struct {
	pool   *pgxpool.Pool
	outbox *outbox.Store
	secret []byte
	log    *slog.Logger
	// now is swappable in tests.
	now func() time.Time
}

func NewHandler(pool *pgxpool.Pool, ob *outbox.Store, secret []byte, log *slog.Logger) *Handler {
	return &Handler{pool: pool, outbox: ob, secret: secret, log: log, now: time.Now}
}

// Routes mounts payment endpoints. The webhook is intentionally NOT
// JWT-authed: authenticity comes from the HMAC signature (§5.5).
func (h *Handler) Routes(r chi.Router) {
	r.Post("/webhook", h.Webhook)
}

// Event is the provider callback payload.
type Event struct {
	EventID     string  `json:"event_id"`
	Type        string  `json:"type"` // payment.captured | payment.failed | payment.refunded
	BookingID   string  `json:"booking_id"`
	Amount      float64 `json:"amount"`
	ProviderRef string  `json:"provider_ref"`
	Timestamp   int64   `json:"ts"` // unix seconds (also sent in header)
}

// VerifySignature computes hex(HMAC-SHA256(secret, timestamp + "." + body)).
func VerifySignature(secret []byte, timestamp, body string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write([]byte(body))
	return hex.EncodeToString(mac.Sum(nil))
}

// ValidSignature does a constant-time compare plus ±300 s freshness window.
func ValidSignature(secret []byte, tsHeader, body, sigHeader string, now time.Time) error {
	if tsHeader == "" || sigHeader == "" {
		return fmt.Errorf("missing X-Timestamp or X-Signature header")
	}
	sec, err := strconv.ParseInt(tsHeader, 10, 64)
	if err != nil {
		return fmt.Errorf("X-Timestamp must be unix seconds")
	}
	// ±5 minutes freshness (§5.5)
	if math.Abs(float64(now.Unix()-sec)) > 300 {
		return fmt.Errorf("timestamp outside ±5 min freshness window")
	}
	want := VerifySignature(secret, tsHeader, body)
	if !hmac.Equal([]byte(strings.ToLower(want)), []byte(strings.ToLower(sigHeader))) {
		return fmt.Errorf("signature mismatch")
	}
	return nil
}

// Webhook handles POST /v1/payments/webhook.
func (h *Handler) Webhook(w http.ResponseWriter, r *http.Request) {
	// raw body for HMAC — read before any decode
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var raw strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Body.Read(buf)
		raw.Write(buf[:n])
		if err != nil {
			break
		}
	}

	ts := r.Header.Get("X-Timestamp")
	sig := r.Header.Get("X-Signature")
	if err := ValidSignature(h.secret, ts, raw.String(), sig, h.now()); err != nil {
		h.log.Warn("webhook signature rejected", slog.String("err", err.Error()))
		middleware.WriteProblem(w, http.StatusUnauthorized, "Invalid Signature", err.Error())
		return
	}

	var ev Event
	if err := json.Unmarshal([]byte(raw.String()), &ev); err != nil {
		middleware.WriteProblem(w, http.StatusBadRequest, "Invalid Payload", err.Error())
		return
	}
	if ev.EventID == "" || ev.BookingID == "" {
		middleware.WriteProblem(w, http.StatusBadRequest, "Invalid Payload", "event_id and booking_id required")
		return
	}
	switch ev.Type {
	case "payment.captured", "payment.failed", "payment.refunded":
	default:
		middleware.WriteProblem(w, http.StatusBadRequest, "Unknown Event Type", ev.Type)
		return
	}

	ctx := r.Context()
	bid, err := uuid.Parse(ev.BookingID)
	if err != nil {
		middleware.WriteProblem(w, http.StatusBadRequest, "Invalid Payload", "booking_id must be a UUID")
		return
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "tx failed")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// event_id idempotency (§5.5): exact replay of the last applied event
	// short-circuits; stale events that would un-apply money state are ignored.
	var storedEvent, storedStatus *string
	if err := tx.QueryRow(ctx,
		`SELECT event_id, status FROM payment WHERE booking_id=$1 FOR UPDATE`, bid).
		Scan(&storedEvent, &storedStatus); err != nil && err != pgx.ErrNoRows {
		middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "dedupe check failed")
		return
	}
	if storedEvent != nil && *storedEvent == ev.EventID {
		writeJSON(w, http.StatusOK, map[string]string{"status": "duplicate", "event_id": ev.EventID})
		return
	}
	newStatus := statusFor(ev.Type)
	curStatus := ""
	if storedStatus != nil {
		curStatus = *storedStatus
	}
	if !paymentStatusAllowed(curStatus, newStatus) {
		// e.g. late payment.captured after a refund — never un-apply money
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "event_id": ev.EventID})
		return
	}

	// stamp the intent row (or create one if the booking has none yet)
	tag, err := tx.Exec(ctx, `
		UPDATE payment SET status=$2, provider_ref=COALESCE(NULLIF($3,''), provider_ref),
		 event_id=$4, amount=COALESCE(NULLIF($5,0), amount), updated_at=now()
		 WHERE booking_id=$1 AND event_id IS DISTINCT FROM $4`,
		bid, newStatus, ev.ProviderRef, ev.EventID, ev.Amount)
	if err != nil {
		h.log.Error("payment update failed", slog.String("err", err.Error()))
		middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "update failed")
		return
	}
	if tag.RowsAffected() == 0 && storedEvent == nil {
		// no intent row at all (webhook beat our own insert — never lose a
		// money event): insert explicitly, deduped by the UNIQUE(event_id).
		if _, err := tx.Exec(ctx, `
			INSERT INTO payment (booking_id, amount, status, provider, provider_ref, event_id)
			VALUES ($1,$2,$3,'upi',$4,$5)
			ON CONFLICT (event_id) DO NOTHING`,
			bid, ev.Amount, newStatus, ev.ProviderRef, ev.EventID); err != nil {
			if isUnique23505(err) {
				writeJSON(w, http.StatusOK, map[string]string{"status": "duplicate", "event_id": ev.EventID})
				return
			}
			middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "insert failed")
			return
		}
	} else if tag.RowsAffected() == 0 && storedEvent != nil {
		// same event landed concurrently between SELECT and UPDATE
		writeJSON(w, http.StatusOK, map[string]string{"status": "duplicate", "event_id": ev.EventID})
		return
	}

	// booking transition (guarded by the §5.6 state machine)
	var bStatus string
	var seats int
	err = tx.QueryRow(ctx,
		`SELECT status, seats FROM booking WHERE id=$1 FOR UPDATE`, bid).
		Scan(&bStatus, &seats)
	if err == pgx.ErrNoRows {
		middleware.WriteProblem(w, http.StatusNotFound, "Booking Not Found", "unknown booking_id")
		return
	}
	if err != nil {
		middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "lock failed")
		return
	}

	switch ev.Type {
	case "payment.captured":
		if bStatus == booking.BookingConfirmed {
			// different event id but booking already confirmed — no-op, and
			// critically: no second ledger posting.
			writeJSON(w, http.StatusOK, map[string]string{"status": "already_confirmed"})
			return
		}
		next, terr := booking.Transition("booking", bStatus, booking.BookingConfirmed)
		if terr != nil {
			middleware.WriteProblem(w, http.StatusConflict, "Illegal State Transition", terr.Error())
			return
		}
		if _, err := tx.Exec(ctx,
			`UPDATE booking SET status=$2, updated_at=now() WHERE id=$1`, bid, next); err != nil {
			middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "update failed")
			return
		}
		// double-entry: capture posts fare -> driver payout + platform fee (§5.7 example)
		if err := h.postLedger(ctx, tx, "capture:"+ev.EventID, ev.Amount); err != nil {
			middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "ledger failed")
			return
		}
		if err := h.outbox.Enqueue(ctx, tx, "booking.confirmed", map[string]any{
			"booking_id": bid.String(), "event_id": ev.EventID,
			"amount": ev.Amount, "provider_ref": ev.ProviderRef,
		}); err != nil {
			middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "outbox failed")
			return
		}

	case "payment.failed":
		if bStatus == booking.BookingPending {
			next, terr := booking.Transition("booking", bStatus, booking.BookingExpired)
			if terr != nil {
				middleware.WriteProblem(w, http.StatusConflict, "Illegal State Transition", terr.Error())
				return
			}
			if _, err := tx.Exec(ctx,
				`UPDATE booking SET status=$2, updated_at=now() WHERE id=$1`, bid, next); err != nil {
				middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "update failed")
				return
			}
			// release the seats back
			if _, err := tx.Exec(ctx,
				`UPDATE ride_instance SET seats_booked = GREATEST(seats_booked - $2,0)
				 WHERE id = (SELECT ride_instance_id FROM booking WHERE id=$1)`,
				bid, seats); err != nil {
				middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "seat release failed")
				return
			}
		}

	case "payment.refunded":
		if _, err := tx.Exec(ctx,
			`UPDATE payment SET status='REFUNDED', updated_at=now() WHERE booking_id=$1`, bid); err != nil {
			middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "refund update failed")
			return
		}
		if err := h.postLedger(ctx, tx, "refund:"+ev.EventID, -ev.Amount); err != nil {
			middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "ledger failed")
			return
		}
	}

	if err := tx.Commit(ctx); err != nil {
		h.log.Error("webhook commit failed", slog.String("err", err.Error()))
		middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "commit failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "applied", "event_id": ev.EventID})
}

// postLedger writes a balanced double-entry pair for a capture.
// amount > 0: DR wallet_clearing / CR driver_earned + platform_fee
// amount < 0: the mirror image (refund).
func (h *Handler) postLedger(ctx context.Context, tx pgx.Tx, txnID string, amount float64) error {
	abs := math.Abs(amount)
	// 85/15 revenue split is the MVP default (configurable later via tenant.policy)
	driverShare := round2(abs * 0.85)
	platformFee := round2(abs - driverShare)
	type entry struct {
		account string
		debit   float64
		credit  float64
	}
	var entries []entry
	if amount >= 0 {
		entries = []entry{
			{"wallet_clearing", abs, 0},
			{"driver_earned", 0, driverShare},
			{"platform_fee", 0, platformFee},
		}
	} else {
		entries = []entry{
			{"driver_earned", driverShare, 0},
			{"platform_fee", platformFee, 0},
			{"wallet_clearing", 0, abs},
		}
	}
	for _, e := range entries {
		if _, err := tx.Exec(ctx, `
			INSERT INTO ledger_entry (transaction_id, account, debit, credit)
			VALUES ($1,$2,$3,$4)`, txnID, e.account, e.debit, e.credit); err != nil {
			return err
		}
	}
	return nil
}

// Status handles GET /v1/payments/{id} minimal lookup.
func (h *Handler) Status(w http.ResponseWriter, r *http.Request) {
	pid, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		middleware.WriteProblem(w, http.StatusBadRequest, "Validation Failed", "bad payment id")
		return
	}
	var out struct {
		ID        string  `json:"id"`
		BookingID string  `json:"booking_id"`
		Amount    float64 `json:"amount"`
		Status    string  `json:"status"`
	}
	err = h.pool.QueryRow(r.Context(),
		`SELECT id::text, booking_id::text, amount, status FROM payment WHERE id=$1`, pid).
		Scan(&out.ID, &out.BookingID, &out.Amount, &out.Status)
	if err == pgx.ErrNoRows {
		middleware.WriteProblem(w, http.StatusNotFound, "Not Found", "payment does not exist")
		return
	}
	if err != nil {
		middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "lookup failed")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func statusFor(eventType string) string {
	switch eventType {
	case "payment.captured":
		return "CAPTURED"
	case "payment.failed":
		return "FAILED"
	case "payment.refunded":
		return "REFUNDED"
	}
	return "CREATED"
}

func isUnique23505(err error) bool {
	return err != nil && strings.Contains(err.Error(), "23505")
}

// paymentStatusAllowed guards the payment-row state machine:
// REFUNDED is terminal; CAPTURED never regresses to FAILED.
func paymentStatusAllowed(cur, next string) bool {
	if cur == "" {
		return true // no row yet
	}
	switch {
	case cur == "REFUNDED":
		return false
	case cur == "CAPTURED" && next == "FAILED":
		return false
	}
	return true
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
