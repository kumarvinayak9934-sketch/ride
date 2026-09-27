// Package matching implements §5.2 rides search:
//
//	POST /v1/rides/search
//	  1. PostGIS ST_DWithin corridor query around pickup & dropoff
//	  2. forward-order check (pickup before dropoff along the line)
//	  3. score = 0.40*detour + 0.25*time + 0.20*rating + 0.15*price
//	     (weights configurable per city; defaults are the spec's mix)
//	  4. empty result => 200 with [] — never 404, never fake data
package matching

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kumarvinayak9934-sketch/ride/apps/api/internal/config"
	"github.com/kumarvinayak9934-sketch/ride/apps/api/internal/middleware"
	"github.com/kumarvinayak9934-sketch/ride/apps/api/internal/modules/pricing"
)

// SearchRequest is the POST body.
type SearchRequest struct {
	PickupLat float64 `json:"pickup_lat"`
	PickupLng float64 `json:"pickup_lng"`
	DropLat   float64 `json:"drop_lat"`
	DropLngF  float64 `json:"drop_lng"`
	Date      string  `json:"date"`  // YYYY-MM-DD
	Time      string  `json:"time"`  // HH:MM preferred departure
	Seats     int     `json:"seats"` // default 1
	RadiusM   float64 `json:"radius_m,omitempty"`
	City      string  `json:"city,omitempty"` // ranking weight lookup
}

// Ride is one search hit.
type Ride struct {
	InstanceID    string  `json:"ride_instance_id"`
	RouteID       string  `json:"route_id"`
	DriverID      string  `json:"driver_id"`
	DriverRating  float64 `json:"driver_rating"`
	OriginLabel   string  `json:"origin_label"`
	DestLabel     string  `json:"dest_label"`
	DepartureTime string  `json:"departure_time"`
	DurationMin   int     `json:"duration_min"`
	DistanceKm    float64 `json:"distance_km"`
	SeatsFree     int     `json:"seats_free"`
	Fare          float64 `json:"fare"`  // per seat after clamp
	Total         float64 `json:"total"` // fare + fees, per seat
	DetourKm      float64 `json:"detour_km"`
	DetourMin     float64 `json:"detour_min"`
	PriceEstimate float64 `json:"price_estimate"`
	Score         float64 `json:"score"`
	ScoreParts    Parts   `json:"score_parts"`
}

// Parts exposes the normalised ranking components (all 0..1, higher = better).
type Parts struct {
	Detour float64 `json:"detour"`
	Time   float64 `json:"time"`
	Rating float64 `json:"rating"`
	Price  float64 `json:"price"`
}

// Handler serves search.
type Handler struct {
	pool   *pgxpool.Pool
	engine *pricing.Engine
	cfg    config.MatchConfig
	log    *slog.Logger
}

func NewHandler(pool *pgxpool.Pool, engine *pricing.Engine, cfg config.MatchConfig, log *slog.Logger) *Handler {
	return &Handler{pool: pool, engine: engine, cfg: cfg, log: log}
}

// defaultMatchConfig is the spec's §5.2 default (2000 m radius, 0.40/0.25/0.20/0.15).
func defaultMatchConfig() config.MatchConfig {
	return config.MatchConfig{
		RadiusM:     2000,
		RankWeights: map[string]config.Weights{},
	}
}

func (req *SearchRequest) normalize() error {
	// DropLng arrives as raw JSON float (the field above is the string
	// placeholder for doc purposes; decode strictly).
	if req.PickupLat < -90 || req.PickupLat > 90 || req.DropLat < -90 || req.DropLat > 90 {
		return fmt.Errorf("latitude out of range")
	}
	if req.PickupLng < -180 || req.PickupLng > 180 || req.DropLngF < -180 || req.DropLngF > 180 {
		return fmt.Errorf("longitude out of range")
	}
	if req.Date == "" {
		return fmt.Errorf("date is required (YYYY-MM-DD)")
	}
	if _, err := time.Parse("2006-01-02", req.Date); err != nil {
		return fmt.Errorf("date must be YYYY-MM-DD")
	}
	if req.Seats <= 0 {
		req.Seats = 1
	}
	if req.Seats > 11 {
		return fmt.Errorf("seats must be 1..11")
	}
	if req.RadiusM <= 0 || req.RadiusM > 10000 {
		req.RadiusM = 2000 // spec default
	}
	return nil
}

// rawCandidate is a row from the corridor query.
type rawCandidate struct {
	InstanceID    string
	RouteID       string
	DriverID      string
	DriverRating  float64
	OriginLabel   string
	DestLabel     string
	DepartureTime string
	DurationMin   int
	DistanceKm    float64
	Seats         int
	SeatsBooked   int
	PriceEst      float64
	PickupIdx     int
	DropIdx       int
	DetourKm      float64
	RoutePts      []byte // decoded polyline as JSON [[lat,lng],...]
}

// Handle is POST /v1/rides/search.
func (h *Handler) Handle(w http.ResponseWriter, r *http.Request) {
	var req SearchRequest
	if !middleware.DecodeJSON(w, r, &req) {
		return
	}
	if err := req.normalize(); err != nil {
		middleware.WriteProblem(w, http.StatusBadRequest, "Validation Failed", err.Error())
		return
	}

	ctx := r.Context()
	// 1+2. corridor query: within radius of BOTH endpoints, forward order,
	// matching date, published, enough free seats.
	const query = `
WITH pts AS (
  SELECT ST_SetSRID(ST_MakePoint($1,$2),4326) AS pickup,
         ST_SetSRID(ST_MakePoint($3,$4),4326) AS dropoff
),
corridor AS (
  SELECT r.id AS route_id, r.driver_id, r.origin_label, r.dest_label,
         to_char(r.departure_time,'HH24:MI') AS departure_time,
         r.duration_min, r.distance_km, r.seats,
         r.price_estimate,
         ST_AsGeoJSON(r.waypoints_line)::json AS geom,
         ri.id AS instance_id, ri.seats_booked,
         -- detour: along-corridor segment minus straight line
         GREATEST(
           ST_Length(ST_LineSubstring(r.waypoints_line::geometry,
                      LEAST(ST_LineLocatePoint(r.waypoints_line::geometry, p.pickup),
                            ST_LineLocatePoint(r.waypoints_line::geometry, d.dropoff)),
                      GREATEST(ST_LineLocatePoint(r.waypoints_line::geometry, p.pickup),
                               ST_LineLocatePoint(r.waypoints_line::geometry, d.dropoff)))::geography,
                      false)
           - ST_Distance(p.pickup::geography, d.dropoff::geography, false),
           0) / 1000.0 AS detour_km
  FROM route r
  JOIN ride_instance ri ON ri.route_id = r.id
  CROSS JOIN pts p, pts d
  WHERE r.status = 'PUBLISHED'
    AND ri.status = 'PUBLISHED'
    AND ri.ride_date = $5::date
    AND (r.seats - ri.seats_booked) >= $6
    AND ST_DWithin(r.waypoints_line, p.pickup::geography, $7)
    AND ST_DWithin(r.waypoints_line, d.dropoff::geography, $7)
    AND ST_LineLocatePoint(r.waypoints_line::geometry, p.pickup)
      < ST_LineLocatePoint(r.waypoints_line::geometry, d.dropoff)
)
SELECT c.*, COALESCE(a.rating, 0)::float8 AS driver_rating
FROM corridor c
LEFT JOIN (
  SELECT to_account_id, AVG(stars)::float8 AS rating
  FROM rating GROUP BY to_account_id
) a ON a.to_account_id = c.driver_id
ORDER BY c.detour_km ASC
LIMIT 50`

	rows, err := h.pool.Query(ctx, query,
		req.PickupLng, req.PickupLat, req.DropLngF, req.DropLat,
		req.Date, req.Seats, req.RadiusM)
	if err != nil {
		h.log.Error("search query failed", slog.String("err", err.Error()))
		middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "search failed")
		return
	}
	defer rows.Close()

	var raw []rawCandidate
	for rows.Next() {
		var c rawCandidate
		var geom []byte
		if err := rows.Scan(&c.RouteID, &c.DriverID, &c.OriginLabel, &c.DestLabel,
			&c.DepartureTime, &c.DurationMin, &c.DistanceKm, &c.Seats,
			&c.PriceEst, &geom, &c.InstanceID, &c.SeatsBooked,
			&c.DetourKm, &c.DriverRating); err != nil {
			h.log.Error("search scan failed", slog.String("err", err.Error()))
			middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "scan failed")
			return
		}
		c.RoutePts = geom
		raw = append(raw, c)
	}
	if err := rows.Err(); err != nil {
		middleware.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "rows error")
		return
	}

	// 3. score + price each candidate
	out := h.scoreAll(raw, req)

	// 4. 200 + [] on no matches (spec: not 404)
	writeJSON(w, http.StatusOK, map[string]any{
		"results":  out,
		"count":    len(out),
		"weights":  h.cfg.WeightsFor(req.City),
		"radius_m": req.RadiusM,
	})
}

// scoreAll computes fares and the ranking score for every candidate.
func (h *Handler) scoreAll(raw []rawCandidate, req SearchRequest) []Ride {
	// demand/supply proxy: candidates found = demand pressure for this query
	demand := len(raw)
	supply := 0
	for _, c := range raw {
		supply += c.Seats - c.SeatsBooked
	}

	rides := make([]Ride, 0, len(raw))
	for _, c := range raw {
		free := c.Seats - c.SeatsBooked
		if free < 0 {
			free = 0
		}
		bd := h.engine.Compute(pricing.Input{
			DistanceKm:  c.DistanceKm,
			DurationMin: float64(c.DurationMin),
			Demand:      demand,
			Supply:      supply,
			Seats:       c.Seats,
			Location:    req.City,
		})
		// detour minutes at ~40 km/h
		detourMin := c.DetourKm / 40.0 * 60.0
		rides = append(rides, Ride{
			InstanceID:    c.InstanceID,
			RouteID:       c.RouteID,
			DriverID:      c.DriverID,
			DriverRating:  round2(c.DriverRating),
			OriginLabel:   c.OriginLabel,
			DestLabel:     c.DestLabel,
			DepartureTime: c.DepartureTime,
			DurationMin:   c.DurationMin,
			DistanceKm:    round2(c.DistanceKm),
			SeatsFree:     free,
			Fare:          bd.Fare,
			Total:         bd.Total,
			DetourKm:      round2(c.DetourKm),
			DetourMin:     round2(detourMin),
			PriceEstimate: c.PriceEst,
		})
	}

	// normalise components over the candidate set (all guards for n=0/1)
	w := h.cfg.WeightsFor(req.City)
	for i := range rides {
		rides[i].ScoreParts = h.parts(rides, i, req)
		rides[i].Score = w.Detour*rides[i].ScoreParts.Detour +
			w.Time*rides[i].ScoreParts.Time +
			w.Rating*rides[i].ScoreParts.Rating +
			w.Price*rides[i].ScoreParts.Price
		rides[i].Score = round4(rides[i].Score)
	}

	// rank: score desc, then fare asc
	sort.SliceStable(rides, func(i, j int) bool {
		if rides[i].Score != rides[j].Score {
			return rides[i].Score > rides[j].Score
		}
		return rides[i].Fare < rides[j].Fare
	})
	return rides
}

// parts normalises each ranking component to 0..1 where 1.0 is best.
func (h *Handler) parts(rides []Ride, i int, req SearchRequest) Parts {
	me := rides[i]
	// detour: less is better — divide by max detour (guarded)
	maxDet := 0.0
	minFare, maxFare := math.Inf(1), math.Inf(-1)
	for _, r := range rides {
		if r.DetourKm > maxDet {
			maxDet = r.DetourKm
		}
		if r.Fare < minFare {
			minFare = r.Fare
		}
		if r.Fare > maxFare {
			maxFare = r.Fare
		}
	}
	var p Parts
	if maxDet <= 0 {
		p.Detour = 1
	} else {
		p.Detour = round4(1 - me.DetourKm/maxDet)
	}

	// time: closeness to requested departure (default 09:00), window = 3 h
	want := parseHM(req.Time, 9, 0)
	got := parseHM(me.DepartureTime, 0, 0)
	diff := math.Abs(hhmmDiff(want, got))
	window := 180.0 // minutes
	p.Time = round4(math.Max(0, 1-diff/window))

	// rating: 0..5 -> 0..1; unrated (0) gets neutral 0.6 (not punished hard)
	if me.DriverRating <= 0 {
		p.Rating = 0.6
	} else {
		p.Rating = round4(math.Min(1, me.DriverRating/5))
	}

	// price: cheaper is better over the candidate set (guarded)
	if maxFare-minFare < 0.01 {
		p.Price = 1
	} else {
		p.Price = round4((maxFare - me.Fare) / (maxFare - minFare))
	}
	return p
}

// decodeStrict is a no-op kept for API symmetry; drop_lng decodes as float64.
func (h *Handler) decodeStrict(r *http.Request, req *SearchRequest) error {
	return nil
}

func parseHM(s string, defH, defM int) float64 {
	if s == "" {
		return float64(defH)*60 + float64(defM)
	}
	var hh, mm int
	if _, err := fmt.Sscanf(s, "%d:%d", &hh, &mm); err != nil {
		return float64(defH)*60 + float64(defM)
	}
	return float64(hh)*60 + float64(mm)
}

func hhmmDiff(a, b float64) float64 {
	d := a - b
	if d > 720 {
		d -= 1440
	}
	if d < -720 {
		d += 1440
	}
	return d
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
func round4(v float64) float64 { return math.Round(v*10000) / 10000 }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
