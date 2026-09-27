// Package routes implements §5.1 route publishing: waypoints -> PostGIS
// corridor, weekday mask, vehicle ownership validation, price preview.
package routes

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kumarvinayak9934-sketch/ride/apps/api/internal/middleware"
	"github.com/kumarvinayak9934-sketch/ride/apps/api/internal/modules/pricing"
	"github.com/kumarvinayak9934-sketch/ride/apps/api/pkg/geo"
)

// Waypoint is one corridor vertex.
type Waypoint struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

// PublishRequest is the POST /v1/routes/publish body.
type PublishRequest struct {
	VehicleID     string     `json:"vehicle_id"`
	OriginLabel   string     `json:"origin_label"`
	DestLabel     string     `json:"dest_label"`
	Waypoints     []Waypoint `json:"waypoints"`      // >= 2
	DepartureTime string     `json:"departure_time"` // "HH:MM" local
	WeekdayMask   int        `json:"weekday_mask"`   // 0..127, bit0=Mon
	DurationMin   int        `json:"duration_min"`
	Seats         int        `json:"seats"`
}

// Route is the created resource.
type Route struct {
	ID            string     `json:"id"`
	DriverID      string     `json:"driver_id"`
	VehicleID     string     `json:"vehicle_id"`
	OriginLabel   string     `json:"origin_label"`
	DestLabel     string     `json:"dest_label"`
	Waypoints     []Waypoint `json:"waypoints,omitempty"`
	DepartureTime string     `json:"departure_time"`
	WeekdayMask   int        `json:"weekday_mask"`
	DurationMin   int        `json:"duration_min"`
	DistanceKm    float64    `json:"distance_km"`
	Seats         int        `json:"seats"`
	PriceEstimate float64    `json:"price_estimate"`
	Status        string     `json:"status"`
	CreatedAt     time.Time  `json:"created_at"`
}

// Handler serves route endpoints.
type Handler struct {
	pool   *pgxpool.Pool
	engine *pricing.Engine
	log    *slog.Logger
}

func NewHandler(pool *pgxpool.Pool, engine *pricing.Engine, log *slog.Logger) *Handler {
	return &Handler{pool: pool, engine: engine, log: log}
}

// Routes mounts endpoints (all JWT-authed by the caller).
func (h *Handler) Routes(r chi.Router) {
	r.Post("/publish", h.Publish)
	r.Get("/mine", h.Mine)
}

// validate checks the §5.1 publish rules.
func (req *PublishRequest) validate() error {
	if len(req.Waypoints) < 2 {
		return fmt.Errorf("waypoints must contain at least 2 points")
	}
	if len(req.Waypoints) > 200 {
		return fmt.Errorf("too many waypoints (max 200)")
	}
	for i, w := range req.Waypoints {
		if w.Lat < -90 || w.Lat > 90 || w.Lng < -180 || w.Lng > 180 {
			return fmt.Errorf("waypoint %d out of range", i)
		}
	}
	if req.WeekdayMask < 0 || req.WeekdayMask > 127 {
		return fmt.Errorf("weekday_mask must be within 0..127")
	}
	if req.WeekdayMask == 0 {
		return fmt.Errorf("weekday_mask must select at least one day")
	}
	if _, err := time.Parse("15:04", req.DepartureTime); err != nil {
		return fmt.Errorf("departure_time must be HH:MM")
	}
	if req.DurationMin <= 0 || req.DurationMin > 24*60 {
		return fmt.Errorf("duration_min must be 1..1440")
	}
	if req.Seats < 1 || req.Seats > 11 {
		return fmt.Errorf("seats must be 1..11")
	}
	if req.OriginLabel == "" || req.DestLabel == "" {
		return fmt.Errorf("origin_label and dest_label are required")
	}
	if req.VehicleID == "" {
		return fmt.Errorf("vehicle_id is required")
	}
	return nil
}

// Publish handles POST /v1/routes/publish.
func (h *Handler) Publish(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserID(r.Context())
	if userID == "" {
		middleware.WriteProblem(w, http.StatusUnauthorized, "Unauthorized", "login required")
		return
	}
	var req PublishRequest
	if !middleware.DecodeJSON(w, r, &req) {
		return
	}
	if err := req.validate(); err != nil {
		middleware.WriteProblem(w, http.StatusBadRequest, "Validation Failed", err.Error())
		return
	}
	uid, err := uuid.Parse(userID)
	if err != nil {
		middleware.WriteProblem(w, http.StatusUnauthorized, "Unauthorized", "bad user id")
		return
	}
	vid, err := uuid.Parse(req.VehicleID)
	if err != nil {
		middleware.WriteProblem(w, http.StatusBadRequest, "Validation Failed", "vehicle_id must be a UUID")
		return
	}

	ctx := r.Context()
	// vehicle must belong to the caller and be private (pooled only, §1.15)
	var ownerID uuid.UUID
	var isPrivate bool
	var vehSeats int
	err = h.pool.QueryRow(ctx,
		`SELECT owner_id, is_private, seats_available FROM vehicle WHERE id=$1`, vid).
		Scan(&ownerID, &isPrivate, &vehSeats)
	if err != nil {
		middleware.WriteProblem(w, http.StatusNotFound, "Vehicle Not Found", "vehicle does not exist")
		return
	}
	if ownerID != uid {
		middleware.WriteProblem(w, http.StatusForbidden, "Forbidden", "vehicle belongs to another user")
		return
	}
	if !isPrivate {
		middleware.WriteProblem(w, http.StatusUnprocessableEntity, "Vehicle Not Pooled",
			"Safar Share pools private vehicles only")
		return
	}
	if req.Seats > vehSeats-1 { // driver keeps one seat
		middleware.WriteProblem(w, http.StatusUnprocessableEntity, "Too Many Seats",
			fmt.Sprintf("vehicle offers %d bookable seats", vehSeats-1))
		return
	}

	// geometry + distance
	pts := make([]geo.Point, len(req.Waypoints))
	for i, wp := range req.Waypoints {
		pts[i] = geo.Point{Lat: wp.Lat, Lng: wp.Lng}
	}
	simplified := geo.Simplify(pts, 10) // 10 m tolerance trims jitter
	if len(simplified) < 2 {
		simplified = pts
	}
	distKm := geo.PathLengthKm(simplified)
	if distKm <= 0 {
		middleware.WriteProblem(w, http.StatusBadRequest, "Validation Failed", "route has zero length")
		return
	}

	// price preview via the §5.3 engine (published estimate = undiscounted)
	bd := h.engine.Compute(pricing.Input{
		DistanceKm:  distKm,
		DurationMin: float64(req.DurationMin),
		Seats:       req.Seats + 1, // driver seat excluded from cost split
		Location:    "publish",
	})

	linestring := buildLineString(simplified)
	dep, _ := time.Parse("15:04", req.DepartureTime)

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "tx start failed")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var routeID uuid.UUID
	var createdAt time.Time
	err = tx.QueryRow(ctx, `
		INSERT INTO route
		  (driver_id, vehicle_id, origin_label, dest_label, waypoints_line,
		   departure_time, weekday_mask, duration_min, distance_km, seats,
		   price_estimate, status)
		VALUES ($1,$2,$3,$4, ST_GeomFromText($5,4326)::geography, $6,$7,$8,$9,$10,$11,'PUBLISHED')
		RETURNING id, created_at`,
		uid, vid, req.OriginLabel, req.DestLabel, linestring,
		dep.Format("15:04"), req.WeekdayMask, req.DurationMin, distKm, req.Seats,
		bd.Total).
		Scan(&routeID, &createdAt)
	if err != nil {
		h.log.Error("route insert failed", slog.String("err", err.Error()))
		middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "insert failed")
		return
	}

	// H3 corridor cells (best-effort: the GIST index is the live matcher, §2)
	if err := h.insertCells(ctx, tx, routeID, simplified); err != nil {
		h.log.Warn("h3 cell population failed", slog.String("err", err.Error()))
	}

	// materialise ride instances for the next 14 days matching the mask
	if err := h.materialize(ctx, tx, routeID, uid, req.WeekdayMask, dep); err != nil {
		h.log.Error("materialize failed", slog.String("err", err.Error()))
		middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "materialize failed")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "commit failed")
		return
	}

	writeJSON(w, http.StatusCreated, Route{
		ID: routeID.String(), DriverID: uid.String(), VehicleID: vid.String(),
		OriginLabel: req.OriginLabel, DestLabel: req.DestLabel,
		Waypoints: req.Waypoints, DepartureTime: req.DepartureTime,
		WeekdayMask: req.WeekdayMask, DurationMin: req.DurationMin,
		DistanceKm: pricing.Money(distKm), Seats: req.Seats,
		PriceEstimate: bd.Total, Status: "PUBLISHED", CreatedAt: createdAt,
	})
}

// Mine lists the caller's published routes.
func (h *Handler) Mine(w http.ResponseWriter, r *http.Request) {
	uid, err := uuid.Parse(middleware.UserID(r.Context()))
	if err != nil {
		middleware.WriteProblem(w, http.StatusUnauthorized, "Unauthorized", "bad user")
		return
	}
	rows, err := h.pool.Query(r.Context(), `
		SELECT id, driver_id, vehicle_id, origin_label, dest_label,
		       to_char(departure_time,'HH24:MI'), weekday_mask, duration_min,
		       distance_km, seats, price_estimate, status, created_at
		FROM route WHERE driver_id=$1 ORDER BY created_at DESC LIMIT 50`, uid)
	if err != nil {
		middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "query failed")
		return
	}
	defer rows.Close()
	out := []Route{}
	for rows.Next() {
		var rt Route
		if err := rows.Scan(&rt.ID, &rt.DriverID, &rt.VehicleID, &rt.OriginLabel, &rt.DestLabel,
			&rt.DepartureTime, &rt.WeekdayMask, &rt.DurationMin, &rt.DistanceKm, &rt.Seats,
			&rt.PriceEstimate, &rt.Status, &rt.CreatedAt); err != nil {
			middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "scan failed")
			return
		}
		out = append(out, rt)
	}
	writeJSON(w, http.StatusOK, map[string]any{"routes": out})
}

// materialize creates ride_instance rows for the next 14 days where the
// weekday bit (bit0 = Monday) is set. Idempotent via UNIQUE(route_id, ride_date).
func (h *Handler) materialize(ctx context.Context, tx pgx.Tx, routeID, driverID uuid.UUID,
	mask int, dep time.Time) error {

	now := time.Now()
	// Weekday() is Sun=0..Sat=6; bit index is Mon=0..Sun=6.
	bit := func(t time.Time) int {
		w := int(t.Weekday())
		return (w + 6) % 7
	}
	for d := 0; d < 14; d++ {
		day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).
			AddDate(0, 0, d)
		if mask&(1<<bit(day)) == 0 {
			continue
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO ride_instance (route_id, driver_id, ride_date, status)
			VALUES ($1,$2,$3::date,'PUBLISHED')
			ON CONFLICT (route_id, ride_date) DO NOTHING`,
			routeID, driverID, day.Format("2006-01-02"))
		if err != nil {
			return err
		}
	}
	_ = dep
	return nil
}

// insertCells stores a coarse H3-style cell set for the corridor. The live
// matcher uses PostGIS GIST (§2); this table is populated so the later H3
// upgrade needs no backfill. Cells are derived from rounded lat/lng tiles
// (1e-2 degrees ≈ 1.1 km) — good enough for corridor bucketing.
func (h *Handler) insertCells(ctx context.Context, tx pgx.Tx, routeID uuid.UUID, pts []geo.Point) error {
	seen := map[string]bool{}
	for _, p := range pts {
		for dx := -1; dx <= 1; dx++ {
			for dy := -1; dy <= 1; dy++ {
				cell := fmt.Sprintf("tile:%0.2f:%0.2f",
					trunc2(p.Lat)+float64(dy)*0.01,
					trunc2(p.Lng)+float64(dx)*0.01)
				seen[cell] = true
			}
		}
	}
	for cell := range seen {
		_, err := tx.Exec(ctx,
			`INSERT INTO route_cell (route_id, h3_index) VALUES ($1,$2)
			 ON CONFLICT DO NOTHING`, routeID, cell)
		if err != nil {
			return err
		}
	}
	return nil
}

func trunc2(v float64) float64 {
	return float64(int64(v*100)) / 100
}

func buildLineString(pts []geo.Point) string {
	s := "LINESTRING("
	for i, p := range pts {
		if i > 0 {
			s += ","
		}
		s += fmt.Sprintf("%.6f %.6f", p.Lng, p.Lat)
	}
	return s + ")"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
