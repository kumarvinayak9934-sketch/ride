package pricing

import (
	"io"
	"log/slog"
	"testing"

	"github.com/kumarvinayak9934-sketch/ride/apps/api/internal/config"
)

func testCfg() config.PricingConfig {
	return config.PricingConfig{
		BaseFare:             15,
		KMFare:               4.5,
		MinFarePerMin:        0.5,
		MinFare:              40,
		SurgeCap:             1.5,
		IntercityThresholdKm: 50,
		ConvFeeLocal:         19,
		ConvFeeIntercity:     29,
		GSTPct:               18,
		CostFuel:             104,
		CostMileage:          15,
		CostMaint:            1.8,
		CostTolls:            0,
	}
}

func testEngine() *Engine {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(testCfg(), log)
}

// rule 1 sanity: raw = base + km*km + min*minrate
func TestRawFormula(t *testing.T) {
	e := testEngine()
	// 10 km, 20 min, surge 1, loc 1 -> 15 + 45 + 10 = 70
	bd := e.Compute(Input{DistanceKm: 10, DurationMin: 20, Seats: 4})
	if bd.RawBase != 70 {
		t.Fatalf("raw base = %v, want 70", bd.RawBase)
	}
	if bd.Surge != 1.0 {
		t.Fatalf("surge = %v, want 1.0", bd.Surge)
	}
}

// rule 2: surge clamp band [1.0, cap]
func TestSurgeClamp(t *testing.T) {
	e := testEngine()
	cases := []struct {
		demand, supply int
		want           float64
	}{
		{0, 10, 1.0},  // no demand
		{10, 10, 1.0}, // balanced -> 1 + 0.5*0 = 1
		{20, 10, 1.5}, // 1 + 0.5*(2-1) = 1.5
		{100, 1, 1.5}, // way over cap -> clamped to 1.5
		{5, 100, 1.0}, // low ratio -> below 1 -> clamped to 1.0
		{10, 0, 1.5},  // demand with no supply -> cap (guarded div)
	}
	for _, c := range cases {
		if got := e.Surge(c.demand, c.supply); got != c.want {
			t.Errorf("Surge(%d,%d) = %v, want %v", c.demand, c.supply, got, c.want)
		}
	}
}

// rule 3: min fare floor — with cap >= min fare (cap wins whenever they
// conflict; see TestCostCapBeatsMinFare).
func TestMinFareFloor(t *testing.T) {
	e := testEngine()
	// 4.7 km, 4 min, 1 seat: raw = 15+21.15+2 = 38.15 < 40 -> floored to 40.
	// cost cap/seat at 4.7 km = 8.73*4.7 = 41.03 >= 40, so the floor stands.
	bd := e.Compute(Input{DistanceKm: 4.7, DurationMin: 4, Seats: 1})
	if !bd.ClampedLow {
		t.Fatal("expected low clamp")
	}
	if bd.ClampedHigh {
		t.Fatal("cap must not bind here (41.03 > 40)")
	}
	if bd.FareBeforeDiscount != 40 {
		t.Fatalf("fare = %v, want 40", bd.FareBeforeDiscount)
	}
}

// The cost cap is MANDATORY (§5.3 rule 3): when it conflicts with the min
// fare floor on a short trip, the cap wins — riders never overpay per seat.
func TestCostCapBeatsMinFare(t *testing.T) {
	e := testEngine()
	bd := e.Compute(Input{DistanceKm: 1, DurationMin: 1, Seats: 4})
	if !bd.ClampedHigh {
		t.Fatal("expected cap clamp on a 1 km trip")
	}
	if bd.FareBeforeDiscount > bd.CostCap+0.001 {
		t.Fatalf("fare %v exceeded mandatory cap %v", bd.FareBeforeDiscount, bd.CostCap)
	}
	if bd.FareBeforeDiscount >= 40 {
		t.Fatalf("min fare must lose to the cap, got %v", bd.FareBeforeDiscount)
	}
}

// rule 3 (critical): cost cap must NEVER be exceeded, even with surge + loc mult.
func TestCostCapMandatory(t *testing.T) {
	e := testEngine()
	// 100 km intercity, 150 min, max surge, loc 2.0
	// raw = 15 + 450 + 75 = 540; *1.5*2.0 = 1620
	// cost cap/seat (4 seats): fuel (100/15*104=693.33) + maint(180) = 873.33/4 = 218.33
	bd := e.Compute(Input{
		DistanceKm: 100, DurationMin: 150,
		Demand: 1000, Supply: 1, LocMult: 2.0, Seats: 4,
	})
	if !bd.ClampedHigh {
		t.Fatal("expected high clamp to cost cap")
	}
	if bd.FareBeforeDiscount > bd.CostCap+0.001 {
		t.Fatalf("fare %v exceeds cost cap %v", bd.FareBeforeDiscount, bd.CostCap)
	}
	want := CostCapPerSeat(104, 15, 1.8, 100, 0, 4)
	if bd.CostCap != Money(want) {
		t.Fatalf("cap = %v, want %v", bd.CostCap, Money(want))
	}
}

func TestCostCapFormula(t *testing.T) {
	// (100/15*104) + 1.8*100 + 0 = 693.333+180 = 873.333 /4 = 218.33
	got := CostCapPerSeat(104, 15, 1.8, 100, 0, 4)
	if Money(got) != 218.33 {
		t.Fatalf("cap = %v, want 218.33", Money(got))
	}
	// guards
	if CostCapPerSeat(104, 0, 1.8, 100, 0, 0) <= 0 {
		t.Fatal("degenerate config must not yield non-positive cap")
	}
}

// rule 4: tenant discount stays inside the 10–20% band
func TestTenantDiscountBand(t *testing.T) {
	e := testEngine()
	// seats=1 keeps cost cap (87.3) above raw (70) so no high-clamp interferes
	base := e.Compute(Input{DistanceKm: 10, DurationMin: 20, Seats: 1})

	tooLow := e.Compute(Input{DistanceKm: 10, DurationMin: 20, Seats: 1, TenantDiscountPct: 0.05})
	if tooLow.DiscountPct != 0.10 {
		t.Fatalf("discount 5%% should lift to 10%%, got %v", tooLow.DiscountPct)
	}
	tooHigh := e.Compute(Input{DistanceKm: 10, DurationMin: 20, Seats: 1, TenantDiscountPct: 0.5})
	if tooHigh.DiscountPct != 0.20 {
		t.Fatalf("discount 50%% should clamp to 20%%, got %v", tooHigh.DiscountPct)
	}
	// 15% off 70 = 59.50
	mid := e.Compute(Input{DistanceKm: 10, DurationMin: 20, Seats: 1, TenantDiscountPct: 0.15})
	if mid.Fare != 59.50 {
		t.Fatalf("15%% off 70 = %v, want 59.50", mid.Fare)
	}
	if base.Fare <= mid.Fare {
		t.Fatal("discount must reduce fare")
	}
}

// rules 5+6: fees and GST
func TestFeesAndGST(t *testing.T) {
	e := testEngine()
	local := e.Compute(Input{DistanceKm: 10, DurationMin: 20, Seats: 4})
	if local.Intercity {
		t.Fatal("10km must be local")
	}
	if local.ConvFee != 19 {
		t.Fatalf("local fee = %v, want 19", local.ConvFee)
	}
	if local.GST != 3.42 { // 19*0.18 = 3.42
		t.Fatalf("GST = %v, want 3.42", local.GST)
	}
	wantTotal := Money(local.Fare + 19 + 3.42)
	if local.Total != wantTotal {
		t.Fatalf("total = %v, want %v", local.Total, wantTotal)
	}

	inter := e.Compute(Input{DistanceKm: 60, DurationMin: 90, Seats: 4})
	if !inter.Intercity {
		t.Fatal("60km must be intercity")
	}
	if inter.ConvFee != 29 {
		t.Fatalf("intercity fee = %v, want 29", inter.ConvFee)
	}
	if inter.GST != 5.22 { // 29*0.18
		t.Fatalf("GST = %v, want 5.22", inter.GST)
	}
}

// money never has sub-paise dust
func TestMoneyRounding(t *testing.T) {
	if Money(1.005) != 1.0 && Money(1.005) != 1.01 {
		t.Fatalf("Money(1.005) = %v", Money(1.005))
	}
	if Money(873.333333) != 873.33 {
		t.Fatalf("Money(873.333333) = %v, want 873.33", Money(873.333333))
	}
}

// totality: no input may panic or return a negative/non-finite fare
func TestTotalityNoPanics(t *testing.T) {
	e := testEngine()
	inputs := []Input{
		{},
		{DistanceKm: -5},
		{DurationMin: -1},
		{DistanceKm: 1e9, DurationMin: 1e9, Demand: 1e9, Supply: 0, LocMult: 100},
		{DistanceKm: 0, DurationMin: 0, Demand: -1, Supply: -1, LocMult: -1},
		{DistanceKm: 12.3, DurationMin: 25.7, TenantDiscountPct: -0.5, Seats: -3},
	}
	for i, in := range inputs {
		bd := e.Compute(in)
		if bd.Fare < 0 || bd.Total < 0 {
			t.Errorf("case %d: negative fare %v/%v", i, bd.Fare, bd.Total)
		}
		if bd.Surge < 1.0 || bd.Surge > 1.5+1e-9 {
			t.Errorf("case %d: surge %v outside [1, cap]", i, bd.Surge)
		}
	}
}
