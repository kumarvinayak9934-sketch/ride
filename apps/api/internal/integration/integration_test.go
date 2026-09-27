//go:build integration

// Package integration contains Testcontainers-driven integration tests (§12):
// PostGIS corridor search, the booking race on the last seat, payment webhook
// end-to-end and outbox delivery. Run with:
//
//	go test -tags=integration ./internal/integration/...
//
// Requires a local Docker daemon; CI runs these on every push.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/go-connections/nat"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx" for wait.ForSQL
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/kumarvinayak9934-sketch/ride/apps/api/internal/cache"
	"github.com/kumarvinayak9934-sketch/ride/apps/api/internal/config"
	mw "github.com/kumarvinayak9934-sketch/ride/apps/api/internal/middleware"
	"github.com/kumarvinayak9934-sketch/ride/apps/api/internal/modules/booking"
	"github.com/kumarvinayak9934-sketch/ride/apps/api/internal/modules/matching"
	"github.com/kumarvinayak9934-sketch/ride/apps/api/internal/modules/payment"
	"github.com/kumarvinayak9934-sketch/ride/apps/api/internal/modules/pricing"
	"github.com/kumarvinayak9934-sketch/ride/apps/api/internal/outbox"
	"github.com/kumarvinayak9934-sketch/ride/apps/api/pkg/geo"
)

// env spins up PostGIS + Redis once for the whole package.
type env struct {
	pool  *pgxpool.Pool
	cache *cache.Cache
	stop  func()
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()

	pg, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "postgis/postgis:16-3.4",
			ExposedPorts: []string{"5432/tcp"},
			WaitingFor: wait.ForSQL("5432/tcp", "pgx",
				func(host string, port nat.Port) string {
					return fmt.Sprintf("postgres://safar:safar@%s:%s/postgres?sslmode=disable",
						host, port.Port())
				}).WithStartupTimeout(90 * time.Second),
			Env: map[string]string{
				"POSTGRES_USER":     "safar",
				"POSTGRES_PASSWORD": "safar",
				"POSTGRES_DB":       "safar",
			},
		},
		Started: true,
	})
	if err != nil {
		t.Skipf("postgres container unavailable: %v", err)
	}

	port, err := pg.MappedPort(ctx, "5432")
	if err != nil {
		t.Fatal(err)
	}
	dsn := fmt.Sprintf("postgres://safar:safar@localhost:%s/safar?sslmode=disable", port.Port())
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	applyMigrations(t, ctx, pool)

	rd, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "redis:7-alpine",
			ExposedPorts: []string{"6379/tcp"},
			WaitingFor:   wait.ForListeningPort("6379/tcp").WithStartupTimeout(30 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	rport, err := rd.MappedPort(ctx, "6379")
	if err != nil {
		t.Fatal(err)
	}
	cch, err := cache.New("localhost:"+rport.Port(), "")
	if err != nil {
		t.Fatal(err)
	}

	return &env{
		pool:  pool,
		cache: cch,
		stop: func() {
			pool.Close()
			_ = cch.Close()
			_ = pg.Terminate(ctx)
			_ = rd.Terminate(ctx)
		},
	}
}

// discard returns a logger that drops everything — most integration tests
// exercise handlers whose output is irrelevant to the assertions.
func discard(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func applyMigrations(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	// walk up to the repo's migrations/001_init.sql
	candidates := []string{
		"migrations/001_init.sql",
		"apps/api/migrations/001_init.sql",
		"../migrations/001_init.sql",
	}
	var sqlBytes []byte
	var err error
	for _, c := range candidates {
		if sqlBytes, err = os.ReadFile(c); err == nil {
			break
		}
	}
	if err != nil {
		// allow running from package dir two levels below apps/api
		wd, _ := os.Getwd()
		p := filepath.Join(wd, "..", "..", "migrations", "001_init.sql")
		sqlBytes, err = os.ReadFile(p)
		if err != nil {
			t.Fatalf("cannot locate migrations: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, string(sqlBytes)); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
}

// --------------------------------------------------------------------------

// §12 integration case 1: corridor search + ranking on real PostGIS.
func TestSearchCorridorEndToEnd(t *testing.T) {
	e := newEnv(t)
	defer e.stop()
	ctx := context.Background()

	seedRoute(t, ctx, e.pool, routeSeed{
		origin: "Koramangala", dest: "Whitefield",
		waypoints: [][2]float64{{77.6245, 12.9352}, {77.6800, 12.9550}, {77.7500, 12.9698}},
		seats:     4, dep: "08:30", mask: 127,
	})

	engine := pricing.New(config.PricingConfig{
		BaseFare: 15, KMFare: 4.5, MinFarePerMin: 0.5, MinFare: 40,
		SurgeCap: 1.5, IntercityThresholdKm: 50, ConvFeeLocal: 19,
		ConvFeeIntercity: 29, GSTPct: 18, CostFuel: 104, CostMileage: 15, CostMaint: 1.8,
	}, discard(t))
	h := matching.NewHandler(e.pool, engine, config.MatchConfig{RadiusM: 2000}, discard(t))

	// pickup ~12.9352,77.6245 (at route start), drop ~12.9698,77.7500 (at end)
	body := fmt.Sprintf(`{"pickup_lat":12.9352,"pickup_lng":77.6245,"drop_lat":12.9698,"drop_lng":77.7500,"date":%q,"seats":1}`,
		time.Now().AddDate(0, 0, 1).Format("2006-01-02"))
	req := httptest.NewRequest(http.MethodPost, "/v1/rides/search", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.Handle(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("search status %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Results []matching.Ride `json:"results"`
		Count   int             `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Count == 0 || len(out.Results) == 0 {
		t.Fatal("expected at least one corridor match")
	}
	top := out.Results[0]
	if top.Score <= 0 || top.Score > 1.5 {
		t.Fatalf("score out of range: %v", top.Score)
	}
	if top.Fare <= 0 || top.Total < top.Fare {
		t.Fatalf("bad money: fare=%v total=%v", top.Fare, top.Total)
	}
	// ranking invariant: sorted by score desc
	for i := 1; i < len(out.Results); i++ {
		if out.Results[i].Score > out.Results[i-1].Score+1e-9 {
			t.Fatalf("results not sorted by score at %d", i)
		}
	}

	// empty result set => 200 with [], never 404 (§5.2)
	body2 := `{"pickup_lat":28.61,"pickup_lng":77.20,"drop_lat":28.62,"drop_lng":77.21,"date":"2030-01-01"}`
	req2 := httptest.NewRequest(http.MethodPost, "/v1/rides/search", strings.NewReader(body2))
	rec2 := httptest.NewRecorder()
	h.Handle(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("empty search status %d, want 200", rec2.Code)
	}
	if !strings.Contains(rec2.Body.String(), `"results":[]`) &&
		!strings.Contains(rec2.Body.String(), `"results": []`) {
		t.Fatalf("empty results must be []: %s", rec2.Body.String())
	}
}

// §12 integration case 2: last-seat race — exactly one winner, seat count
// ends at capacity, loser gets 409.
func TestBookingRaceLastSeat(t *testing.T) {
	e := newEnv(t)
	defer e.stop()
	ctx := context.Background()

	routeID := seedRoute(t, ctx, e.pool, routeSeed{
		origin: "A", dest: "B",
		waypoints: [][2]float64{{77.0, 12.0}, {77.1, 12.1}},
		seats:     1, // single seat: N concurrent1-seat bookings fight for it
		dep:       "09:00", mask: 127,
	})
	var instanceID uuid.UUID
	err := e.pool.QueryRow(ctx,
		`SELECT id FROM ride_instance WHERE route_id=$1 LIMIT 1`, routeID).Scan(&instanceID)
	if err != nil {
		t.Fatal(err)
	}

	engine := pricing.New(config.PricingConfig{
		BaseFare: 15, KMFare: 4.5, MinFarePerMin: 0.5, MinFare: 40, SurgeCap: 1.5,
		IntercityThresholdKm: 50, ConvFeeLocal: 19, ConvFeeIntercity: 29, GSTPct: 18,
		CostFuel: 104, CostMileage: 15, CostMaint: 1.8,
	}, discard(t))
	ob := outbox.NewStore(e.pool)
	h := booking.NewHandler(e.pool, e.cache, ob, engine, discard(t),
		[]byte("test-secret"), "test@upi")

	const racers = 4
	passengers := make([]uuid.UUID, racers)
	for i := range passengers {
		passengers[i] = uuid.New()
		_, err := e.pool.Exec(ctx,
			`INSERT INTO account (id, phone_hash, phone_enc, age_confirmed)
			 VALUES ($1,$2,$3,true)`,
			passengers[i], fmt.Sprintf("hash-%d", i), []byte("enc"))
		if err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	codes := make([]int, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"ride_instance_id":%q,"seats":1}`, instanceID)
			req := httptest.NewRequest(http.MethodPost, "/v1/bookings", strings.NewReader(body))
			req.Header.Set("Idempotency-Key", uuid.NewString())
			// real session JWT so UserAuth injects the passenger id (§5.1)
			tok := jwt.NewWithClaims(jwt.SigningMethodHS256, &mw.Claims{
				UserID: passengers[i].String(),
				RegisteredClaims: jwt.RegisteredClaims{
					ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
				},
			})
			signed, err := tok.SignedString([]byte("test-secret"))
			if err != nil {
				codes[i] = -1
				return
			}
			req.Header.Set("Authorization", "Bearer "+signed)
			rec := httptest.NewRecorder()
			// wrap with the same handlers the router uses
			mw.UserAuth("test-secret", e.cache)(
				mw.Idempotency(e.cache, "booking")(
					http.HandlerFunc(h.Create),
				),
			).ServeHTTP(rec, req)
			codes[i] = rec.Code
		}(i)
	}
	wg.Wait()

	wins := 0
	for _, c := range codes {
		switch c {
		case http.StatusCreated:
			wins++
		case http.StatusConflict:
			// expected for losers
		default:
			t.Errorf("unexpected status %d", c)
		}
	}
	if wins != 1 {
		t.Fatalf("exactly 1 winner expected, got %d (codes=%v)", wins, codes)
	}

	var booked int
	if err := e.pool.QueryRow(ctx,
		`SELECT seats_booked FROM ride_instance WHERE id=$1`, instanceID).Scan(&booked); err != nil {
		t.Fatal(err)
	}
	if booked != 1 {
		t.Fatalf("seats_booked = %d, want 1 (never overbook)", booked)
	}
	var confirmed int
	_ = e.pool.QueryRow(ctx,
		`SELECT count(*) FROM booking WHERE ride_instance_id=$1`, instanceID).Scan(&confirmed)
	if confirmed != 1 {
		t.Fatalf("booking rows = %d, want 1", confirmed)
	}
	// outbox row was written in the same tx as the booking
	var obCount int
	_ = e.pool.QueryRow(ctx,
		`SELECT count(*) FROM outbox WHERE topic='booking.created'`).Scan(&obCount)
	if obCount == 0 {
		t.Fatal("booking.created outbox row missing")
	}
}

// §12 integration case 3: HMAC webhook drives the booking state machine.
func TestPaymentWebhookLifecycle(t *testing.T) {
	e := newEnv(t)
	defer e.stop()
	ctx := context.Background()
	secret := []byte("whsec-test")

	seed := seedBooking(t, ctx, e.pool)
	engine := pricing.New(config.PricingConfig{
		BaseFare: 15, KMFare: 4.5, MinFarePerMin: 0.5, MinFare: 40, SurgeCap: 1.5,
		IntercityThresholdKm: 50, ConvFeeLocal: 19, ConvFeeIntercity: 29, GSTPct: 18,
		CostFuel: 104, CostMileage: 15, CostMaint: 1.8,
	}, discard(t))
	ob := outbox.NewStore(e.pool)
	ph := payment.NewHandler(e.pool, ob, secret, discard(t))

	fire := func(ev map[string]any) *httptest.ResponseRecorder {
		body, _ := json.Marshal(ev)
		req := httptest.NewRequest(http.MethodPost, "/v1/payments/webhook", strings.NewReader(string(body)))
		ts := fmt.Sprintf("%d", time.Now().Unix())
		req.Header.Set("X-Timestamp", ts)
		req.Header.Set("X-Signature", payment.VerifySignature(secret, ts, string(body)))
		rec := httptest.NewRecorder()
		ph.Webhook(rec, req)
		return rec
	}

	// captured -> CONFIRMED
	rec := fire(map[string]any{
		"event_id": uuid.NewString(), "type": "payment.captured",
		"booking_id": seed.bookingID.String(), "amount": seed.total,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("captured webhook: %d %s", rec.Code, rec.Body.String())
	}
	var status string
	if err := e.pool.QueryRow(ctx,
		`SELECT status FROM booking WHERE id=$1`, seed.bookingID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != booking.BookingConfirmed {
		t.Fatalf("booking status = %s, want CONFIRMED", status)
	}
	// ledger balanced
	var deb, cred float64
	_ = e.pool.QueryRow(ctx,
		`SELECT COALESCE(sum(debit),0), COALESCE(sum(credit),0) FROM ledger_entry
		 WHERE transaction_id LIKE 'capture:%'`).Scan(&deb, &cred)
	if deb != cred || deb == 0 {
		t.Fatalf("ledger unbalanced: debit=%v credit=%v", deb, cred)
	}

	// exact replay -> duplicate, no double ledger
	before := float64(0)
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM ledger_entry`).Scan(&before)
	// (replay with SAME event id)
	// rebuild the same event: capture it once
	evtID := uuid.NewString()
	body := fmt.Sprintf(`{"event_id":%q,"type":"payment.captured","booking_id":%q,"amount":%v}`,
		evtID, seed.bookingID.String(), seed.total)
	ts := fmt.Sprintf("%d", time.Now().Unix())
	mk := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/payments/webhook", strings.NewReader(body))
		req.Header.Set("X-Timestamp", ts)
		req.Header.Set("X-Signature", payment.VerifySignature(secret, ts, body))
		rec := httptest.NewRecorder()
		ph.Webhook(rec, req)
		return rec
	}
	// use a fresh PENDING booking for the duplicate test
	seed2 := seedBooking(t, ctx, e.pool)
	body = fmt.Sprintf(`{"event_id":%q,"type":"payment.captured","booking_id":%q,"amount":%v}`,
		evtID, seed2.bookingID.String(), seed.total)
	r1 := mk()
	if r1.Code != http.StatusOK {
		t.Fatalf("first event: %d", r1.Code)
	}
	r2 := mk()
	if r2.Code != http.StatusOK || !strings.Contains(r2.Body.String(), "duplicate") {
		t.Fatalf("replay must be 200 duplicate, got %d %s", r2.Code, r2.Body.String())
	}
	var ledgerRows int
	_ = e.pool.QueryRow(ctx,
		`SELECT count(*) FROM ledger_entry WHERE transaction_id=$1`, "capture:"+evtID).Scan(&ledgerRows)
	if ledgerRows == 0 {
		t.Fatal("capture ledger entries missing")
	}
	var n int
	_ = e.pool.QueryRow(ctx,
		`SELECT count(*) FROM ledger_entry WHERE transaction_id=$1`, "capture:"+evtID).Scan(&n)
	if n != 2 { // balanced pair
		t.Fatalf("capture must post exactly 2 entries, got %d", n)
	}

	// bad signature -> 401
	req := httptest.NewRequest(http.MethodPost, "/v1/payments/webhook",
		strings.NewReader(body))
	req.Header.Set("X-Timestamp", ts)
	req.Header.Set("X-Signature", "deadbeef")
	rec401 := httptest.NewRecorder()
	ph.Webhook(rec401, req)
	if rec401.Code != http.StatusUnauthorized {
		t.Fatalf("bad sig: %d, want 401", rec401.Code)
	}

	// stale timestamp -> 401
	stale := fmt.Sprintf("%d", time.Now().Unix()-600)
	reqStale := httptest.NewRequest(http.MethodPost, "/v1/payments/webhook", strings.NewReader(body))
	reqStale.Header.Set("X-Timestamp", stale)
	reqStale.Header.Set("X-Signature", payment.VerifySignature(secret, stale, body))
	recStale := httptest.NewRecorder()
	ph.Webhook(recStale, reqStale)
	if recStale.Code != http.StatusUnauthorized {
		t.Fatalf("stale ts: %d, want 401", recStale.Code)
	}

	_ = engine
	_ = ob
}

// Outbox: pending rows eventually go SENT, failures hit DLQ after 5 attempts.
func TestOutboxPollerLifecycle(t *testing.T) {
	e := newEnv(t)
	defer e.stop()
	ctx := context.Background()

	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ob := outbox.NewStore(e.pool)
	if err := ob.Enqueue(ctx, tx, "booking.created", map[string]any{"x": 1}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// success publisher
	p := outbox.NewPoller(ob, func(ctx context.Context, ev outbox.Event) error { return nil }, discard(t))
	n, err := p.PollOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("processed = %d, want 1", n)
	}
	var status string
	_ = e.pool.QueryRow(ctx, `SELECT status FROM outbox LIMIT 1`).Scan(&status)
	if status != "SENT" {
		t.Fatalf("status = %s, want SENT", status)
	}

	// failing publisher -> DLQ after MaxAttempts
	tx2, _ := e.pool.Begin(ctx)
	_ = ob.Enqueue(ctx, tx2, "booking.confirmed", map[string]any{"y": 2})
	_ = tx2.Commit(ctx)
	fail := outbox.NewPoller(ob, func(ctx context.Context, ev outbox.Event) error {
		return fmt.Errorf("always fails")
	}, discard(t))
	// force due-time each round
	for i := 0; i < outbox.MaxAttempts+1; i++ {
		_, _ = e.pool.Exec(ctx, `UPDATE outbox SET next_attempt_at = now() WHERE status='PENDING'`)
		if _, err := fail.PollOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var dlq string
	_ = e.pool.QueryRow(ctx,
		`SELECT status FROM outbox WHERE topic='booking.confirmed'`).Scan(&dlq)
	if dlq != "DLQ" {
		t.Fatalf("status = %s, want DLQ after 5 attempts", dlq)
	}
}

// ---------------------------------------------------- fixtures -------------

type routeSeed struct {
	origin, dest string
	waypoints    [][2]float64 // {lng, lat}
	seats        int
	dep          string
	mask         int
}

func seedRoute(t *testing.T, ctx context.Context, pool *pgxpool.Pool, s routeSeed) uuid.UUID {
	t.Helper()
	driverID := uuid.New()
	_, err := pool.Exec(ctx,
		`INSERT INTO account (id, phone_hash, phone_enc, age_confirmed, role)
		 VALUES ($1,$2,$3,true,'both')`, driverID, uuid.NewString(), []byte("e"))
	if err != nil {
		t.Fatal(err)
	}
	vehID := uuid.New()
	_, err = pool.Exec(ctx,
		`INSERT INTO vehicle (id, owner_id, make_model, reg_number, is_private, seats_available)
		 VALUES ($1,$2,'Maruti Swift','KA-01-1111',true,$3)`, vehID, driverID, s.seats+1)
	if err != nil {
		t.Fatal(err)
	}
	var pts []string
	for _, w := range s.waypoints {
		pts = append(pts, fmt.Sprintf("%.6f %.6f", w[0], w[1]))
	}
	line := "LINESTRING(" + strings.Join(pts, ",") + ")"
	var routeID uuid.UUID
	err = pool.QueryRow(ctx, `
		INSERT INTO route (driver_id, vehicle_id, origin_label, dest_label, waypoints_line,
		 departure_time, weekday_mask, duration_min, distance_km, seats, price_estimate, status)
		 VALUES ($1,$2,$3,$4, ST_GeomFromText($5,4326)::geography, $6,$7,45,
		         $8,$9,150,'PUBLISHED')
		 RETURNING id`,
		driverID, vehID, s.origin, s.dest, line, s.dep, s.mask,
		geo.PathLengthKm(mustPts(s.waypoints)), s.seats).
		Scan(&routeID)
	if err != nil {
		t.Fatal(err)
	}
	// materialise today..+7
	for d := 0; d < 8; d++ {
		day := time.Now().AddDate(0, 0, d).Format("2006-01-02")
		_, err := pool.Exec(ctx, `
			INSERT INTO ride_instance (route_id, driver_id, ride_date, status)
			VALUES ($1,$2,$3::date,'PUBLISHED')
			ON CONFLICT (route_id, ride_date) DO NOTHING`, routeID, driverID, day)
		if err != nil {
			t.Fatal(err)
		}
	}
	return routeID
}

func mustPts(in [][2]float64) []geo.Point {
	out := make([]geo.Point, len(in))
	for i, p := range in {
		out[i] = geo.Point{Lat: p[1], Lng: p[0]}
	}
	return out
}

type bookingSeed struct {
	bookingID  uuid.UUID
	instanceID uuid.UUID
	total      float64
}

func seedBooking(t *testing.T, ctx context.Context, pool *pgxpool.Pool) bookingSeed {
	t.Helper()
	routeID := seedRoute(t, ctx, pool, routeSeed{
		origin: "X", dest: "Y",
		waypoints: [][2]float64{{77.0, 12.0}, {77.2, 12.2}},
		seats:     3, dep: "10:00", mask: 127,
	})
	var inst uuid.UUID
	_ = pool.QueryRow(ctx, `SELECT id FROM ride_instance WHERE route_id=$1 LIMIT 1`, routeID).Scan(&inst)
	pax := uuid.New()
	_, err := pool.Exec(ctx,
		`INSERT INTO account (id, phone_hash, phone_enc, age_confirmed) VALUES ($1,$2,$3,true)`,
		pax, uuid.NewString(), []byte("e"))
	if err != nil {
		t.Fatal(err)
	}
	var bid uuid.UUID
	err = pool.QueryRow(ctx, `
		INSERT INTO booking (ride_instance_id, passenger_id, seats, status, fare, idempotency_key)
		VALUES ($1,$2,1,'PENDING_PAYMENT',150,$3) RETURNING id`,
		inst, pax, uuid.NewString()).Scan(&bid)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx,
		`INSERT INTO payment (booking_id, amount, status, provider) VALUES ($1,180,'CREATED','upi')`, bid)
	if err != nil {
		t.Fatal(err)
	}
	return bookingSeed{bookingID: bid, instanceID: inst, total: 180}
}
