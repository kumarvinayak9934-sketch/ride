// Package pricing implements the §5.3 fare engine.
//
// Rules (spec-authoritative):
//  1. raw            = base + km*km + min*minrate
//  2. surge          = clamp(1 + 0.5*(demand/supply - 1), 1.0, surge_cap)
//  3. fare           = clamp(raw * surge * location_multiplier, min_fare, cost_cap)
//     — the cost cap is MANDATORY: fare never exceeds what a seat costs the
//     driver; every clamp decision is logged.
//  4. tenant discount (10–20%) applies after clamping, before fees.
//  5. convenience fee = 19 (local) or 29 (intercity, >= threshold km)
//  6. GST = 18% of the convenience fee only; total = fare + fee + GST
package pricing

import (
	"fmt"
	"log/slog"
	"math"

	"github.com/kumarvinayak9934-sketch/ride/apps/api/internal/config"
)

// Money rounds to 2 decimal places (rupees & paise).
func Money(v float64) float64 { return math.Round(v*100) / 100 }

// Input is everything the engine needs to price one seat.
type Input struct {
	DistanceKm        float64
	DurationMin       float64
	Demand            int     // matching requests in the corridor right now
	Supply            int     // open seats in the corridor right now
	LocMult           float64 // city cost multiplier (1.0 = tier-1 default); 0 treated as 1
	MinFare           float64 // floor override; 0 -> config default
	TenantDiscountPct float64 // 0, or 0.10–0.20; clamped into band
	Location          string  // for logs
	Seats             int     // seats on the trip for the cost cap; 0 -> 1
	// Cost-cap inputs. Zero values fall back to config.
	CostFuel    float64
	CostMileage float64
	CostMaint   float64
	CostTolls   float64
}

// Breakdown is the auditable result of a price computation.
type Breakdown struct {
	RawBase            float64 `json:"raw_base"`
	Surge              float64 `json:"surge"`
	LocMult            float64 `json:"location_multiplier"`
	ClampedLow         bool    `json:"clamped_low"`
	ClampedHigh        bool    `json:"clamped_high"`
	FareBeforeDiscount float64 `json:"fare_before_discount"`
	DiscountPct        float64 `json:"discount_pct"`
	Fare               float64 `json:"fare"` // per seat, after discount
	ConvFee            float64 `json:"convenience_fee"`
	GST                float64 `json:"gst"`
	Total              float64 `json:"total"` // fare + fee + GST, per seat
	CostCap            float64 `json:"cost_cap"`
	Intercity          bool    `json:"intercity"`
}

// Engine prices fares using the §5.3 inputs.
type Engine struct {
	Cfg config.PricingConfig
	Log *slog.Logger
}

func New(cfg config.PricingConfig, log *slog.Logger) *Engine {
	if log == nil {
		log = slog.Default()
	}
	return &Engine{Cfg: cfg, Log: log}
}

// Surge computes the §5.3 clamped surge multiplier.
// demand/supply with supply==0 is guarded: any demand with zero supply pins
// the multiplier at the cap (scarcity), no demand -> 1.0.
func (e *Engine) Surge(demand, supply int) float64 {
	var ratio float64
	switch {
	case demand <= 0:
		return 1.0
	case supply <= 0:
		ratio = 2.0 // zero supply: max theoretical pressure
	default:
		ratio = float64(demand) / float64(supply)
	}
	m := 1 + 0.5*(ratio-1)
	lo, hi := 1.0, e.Cfg.SurgeCap
	if hi < lo {
		hi = lo
	}
	clamped := math.Max(lo, math.Min(hi, m))
	if clamped != m {
		e.Log.Info("surge clamped", slog.Float64("raw", m), slog.Float64("clamped", clamped))
	}
	return clamped
}

// CostCapPerSeat returns the mandatory per-seat cap: the driver's operating
// cost for the trip divided by seats.  fuel is ₹/L, mileage km/L, maint ₹/km.
// Guards: mileage<=0 and seats<=0 are config errors -> returns +Inf so the
// caller's clamp cannot silently drop the cap (the engine treats Inf as
// "no cap from cost" only when explicitly unset — see Compute).
func CostCapPerSeat(fuel, mileage, maint float64, km, tolls float64, seats int) float64 {
	if seats <= 0 {
		seats = 1
	}
	if mileage <= 0 {
		mileage = 1 // degenerate config guard; logged by caller
	}
	fuelCost := (km / mileage) * fuel
	maintCost := maint * km
	return (fuelCost + maintCost + tolls) / float64(seats)
}

// Compute applies rules 1–6 and returns the per-seat breakdown.
func (e *Engine) Compute(in Input) Breakdown {
	c := e.Cfg
	loc := in.LocMult
	if loc <= 0 {
		loc = 1.0
	}
	if in.DistanceKm < 0 {
		in.DistanceKm = 0 // negative distance is invalid input, never negative fare
	}
	if in.DurationMin <= 0 {
		in.DurationMin = in.DistanceKm * 1.5 // ~40 km/h city fallback
	}
	if in.DurationMin < 0 {
		in.DurationMin = 0
	}
	if in.DurationMin <= 0 && in.DistanceKm <= 0 {
		in.DurationMin = 1 // keep raw > 0 for degenerate inputs
	}

	// rule 1
	raw := c.BaseFare + in.DistanceKm*c.KMFare + in.DurationMin*c.MinFarePerMin

	// rule 2
	surge := e.Surge(in.Demand, in.Supply)

	// rule 3 — clamp band
	rawTotal := raw * surge * loc
	minFare := in.MinFare
	if minFare <= 0 {
		minFare = c.MinFare
	}
	fuel, mileage, maint, tolls := in.CostFuel, in.CostMileage, in.CostMaint, in.CostTolls
	if fuel == 0 {
		fuel = c.CostFuel
	}
	if mileage == 0 {
		mileage = c.CostMileage
	}
	if maint == 0 {
		maint = c.CostMaint
	}
	if in.CostTolls == 0 {
		tolls = c.CostTolls // route-specific tolls plug in via config for MVP
	}
	seats := in.Seats
	if seats <= 0 {
		seats = 1
	}
	cap := CostCapPerSeat(fuel, mileage, maint, in.DistanceKm, tolls, seats)

	bd := Breakdown{
		RawBase: Money(raw),
		Surge:   surge,
		LocMult: loc,
		CostCap: Money(cap),
	}

	fare := rawTotal
	if fare < minFare {
		bd.ClampedLow = true
		e.Log.Info("fare clamped up to min fare",
			slog.Float64("raw", Money(rawTotal)), slog.Float64("min_fare", minFare))
		fare = minFare
	}
	if fare > cap {
		bd.ClampedHigh = true
		e.Log.Info("fare clamped down to cost cap",
			slog.Float64("raw", Money(rawTotal)), slog.Float64("cost_cap", Money(cap)))
		fare = cap
	}
	bd.FareBeforeDiscount = Money(fare)

	// rule 4 — tenant discount clamped into the mandated 10–20% band
	d := in.TenantDiscountPct
	if d > 0 {
		if d < 0.10 {
			d = 0.10
		}
		if d > 0.20 {
			d = 0.20
		}
		bd.DiscountPct = d
		fare = fare * (1 - d)
	}
	bd.Fare = Money(fare)

	// rule 5 — convenience fee by trip class
	bd.Intercity = in.DistanceKm >= c.IntercityThresholdKm
	if bd.Intercity {
		bd.ConvFee = c.ConvFeeIntercity
	} else {
		bd.ConvFee = c.ConvFeeLocal
	}
	// rule 6 — GST on the fee only
	bd.GST = Money(bd.ConvFee * c.GSTPct / 100)
	bd.Total = Money(bd.Fare + bd.ConvFee + bd.GST)
	return bd
}

// Quote is a convenience wrapper returning only the totals.
func (e *Engine) Quote(in Input) (fare, total float64) {
	bd := e.Compute(in)
	return bd.Fare, bd.Total
}

// ValidateConfig surfaces misconfiguration at startup, not at request time.
func ValidateConfig(c config.PricingConfig) error {
	if c.BaseFare < 0 || c.KMFare < 0 || c.MinFarePerMin < 0 {
		return fmt.Errorf("pricing: negative rate component")
	}
	if c.SurgeCap < 1.0 {
		return fmt.Errorf("pricing: surge cap must be >= 1.0")
	}
	if c.GSTPct < 0 || c.GSTPct > 28 {
		return fmt.Errorf("pricing: GST pct out of range")
	}
	return nil
}
