package geo

import (
	"math"
	"testing"
)

func eq(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// Known pair: Bengaluru (12.9716,77.5946) -> Mysuru (12.2958,76.6394)
// ≈ 128.0 km great-circle (road distance is longer; Haversine is straight-line).
func TestHaversineKnownDistance(t *testing.T) {
	blr := Point{Lat: 12.9716, Lng: 77.5946}
	mys := Point{Lat: 12.2958, Lng: 76.6394}
	km := HaversineKm(blr, mys)
	if km < 126 || km > 130 {
		t.Fatalf("BLR->MYS = %.2f km, want ~128", km)
	}
}

func TestHaversineZero(t *testing.T) {
	p := Point{Lat: 19.0760, Lng: 72.8777}
	if !eq(HaversineM(p, p), 0) {
		t.Fatal("same point must be 0")
	}
}

// Symmetry + triangle sanity
func TestHaversineProperties(t *testing.T) {
	a := Point{Lat: 28.6139, Lng: 77.2090} // Delhi
	b := Point{Lat: 17.3850, Lng: 78.4867} // Hyderabad
	c := Point{Lat: 13.0827, Lng: 80.2707} // Chennai
	if !eq(HaversineM(a, b), HaversineM(b, a)) {
		t.Fatal("not symmetric")
	}
	if HaversineM(a, c) > HaversineM(a, b)+HaversineM(b, c)+1 {
		t.Fatal("triangle inequality violated")
	}
}

func TestPathLength(t *testing.T) {
	pts := []Point{{0, 0}, {0, 1}, {0, 2}}
	// 0->1 and 1->2 are equal segments along the equator
	d1 := HaversineKm(pts[0], pts[1])
	got := PathLengthKm(pts)
	if math.Abs(got-2*d1) > 1e-6 {
		t.Fatalf("path = %v, want %v", got, 2*d1)
	}
	if PathLengthKm(pts[:1]) != 0 {
		t.Fatal("single point path must be 0")
	}
	if PathLengthKm(nil) != 0 {
		t.Fatal("nil path must be 0")
	}
}

// Douglas–Peucker: a straight line collapses to its endpoints,
// an L-shape keeps the corner.
func TestSimplifyStraightLine(t *testing.T) {
	line := []Point{{0, 0}, {0, 0.001}, {0, 0.002}, {0, 0.003}, {0, 0.004}}
	out := Simplify(line, 50) // 50 m tolerance
	if len(out) != 2 {
		t.Fatalf("straight line simplified to %d pts, want 2", len(out))
	}
	if out[0] != line[0] || out[1] != line[len(line)-1] {
		t.Fatal("endpoints must be preserved")
	}
}

func TestSimplifyKeepsCorner(t *testing.T) {
	// sharp L-corner: interior points sit ~111 m off the baseline chord —
	// far beyond the 10 m tolerance, so they must survive.
	corner := []Point{{0, 0}, {0, 0.001}, {0.001, 0.001}, {0.001, 0.002}}
	out := Simplify(corner, 10)
	if len(out) < 3 {
		t.Fatalf("corner lost: %d pts", len(out))
	}
}

func TestSimplifyEdgeCases(t *testing.T) {
	if len(Simplify(nil, 10)) != 0 {
		t.Fatal("nil in -> nil out")
	}
	one := []Point{{1, 2}}
	if len(Simplify(one, 10)) != 1 || Simplify(one, 10)[0] != one[0] {
		t.Fatal("single point must round-trip")
	}
}

func TestPerpendicularDistance(t *testing.T) {
	// point sits 0.001° off a horizontal line at the equator (~111 m)
	p := Point{Lat: 0.001, Lng: 0.5}
	a := Point{Lat: 0, Lng: 0}
	b := Point{Lat: 0, Lng: 1}
	d := perpendicularDistanceM(p, a, b)
	if d < 100 || d > 125 {
		t.Fatalf("perp dist = %.1f m, want ~111", d)
	}
	// projection falls outside segment -> distance to nearest endpoint
	// (short segment + nearby point keeps equirectangular error negligible)
	sa := Point{Lat: 0, Lng: 0}
	sb := Point{Lat: 0.001, Lng: 0}
	p2 := Point{Lat: -0.001, Lng: 0.0005}
	d2 := perpendicularDistanceM(p2, sa, sb)
	if math.Abs(d2-HaversineM(p2, sa)) > 5 {
		t.Fatalf("endpoint clamp failed: %.1f vs %.1f", d2, HaversineM(p2, sa))
	}
}

// Detour: along-corridor must be >= straight line; detour floored at 0.
func TestDetourNonNegative(t *testing.T) {
	corridor := []Point{{0, 0}, {0.01, 0}, {0.01, 0.01}, {0, 0.01}}
	pickup := Point{Lat: 0.005, Lng: 0.005}
	drop := Point{Lat: 0.004, Lng: 0.006}
	d := DetourKm(corridor, pickup, drop)
	if d < 0 {
		t.Fatalf("detour = %v, must be >= 0", d)
	}
	// degenerate: corridor == straight line -> detour ~0
	line := []Point{{0, 0}, {0, 1}}
	if DetourKm(line, Point{0, 0}, Point{0, 1}) > 1e-6 {
		t.Fatal("straight corridor should have ~0 detour")
	}
}
