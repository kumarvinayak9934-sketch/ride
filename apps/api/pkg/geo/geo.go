// Package geo provides the pure geometry maths used by corridor matching:
// Haversine distance, polyline simplification (Douglas–Peucker) and simple
// point-to-segment distance for detour estimates. Pure functions, unit-tested
// per §12 (pkg/geo test requirement).
package geo

import (
	"math"
)

const earthRadiusM = 6371000.0

// Point is a WGS84 coordinate.
type Point struct {
	Lat float64
	Lng float64
}

// HaversineM returns the great-circle distance between two points in metres.
func HaversineM(a, b Point) float64 {
	toRad := math.Pi / 180
	dLat := (b.Lat - a.Lat) * toRad
	dLng := (b.Lng - a.Lng) * toRad
	la1 := a.Lat * toRad
	la2 := b.Lat * toRad
	h := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(la1)*math.Cos(la2)*math.Sin(dLng/2)*math.Sin(dLng/2)
	return 2 * earthRadiusM * math.Asin(math.Min(1, math.Sqrt(h)))
}

// HaversineKm is HaversineM in kilometres.
func HaversineKm(a, b Point) float64 { return HaversineM(a, b) / 1000.0 }

// PathLengthKm sums consecutive segment lengths.
func PathLengthKm(pts []Point) float64 {
	if len(pts) < 2 {
		return 0
	}
	total := 0.0
	for i := 1; i < len(pts); i++ {
		total += HaversineKm(pts[i-1], pts[i])
	}
	return total
}

// perpendicularDistanceM computes the distance from p to the great-circle
// segment a-b, using a local equirectangular projection (accurate at the
// sub-100km scales this app deals with).
func perpendicularDistanceM(p, a, b Point) float64 {
	if a.Lat == b.Lat && a.Lng == b.Lng {
		return HaversineM(p, a)
	}
	latRef := (a.Lat + b.Lat) / 2 * math.Pi / 180
	kx := math.Cos(latRef) * math.Pi / 180 * earthRadiusM
	ky := math.Pi / 180 * earthRadiusM

	ax, ay := a.Lng*kx, a.Lat*ky
	bx, by := b.Lng*kx, b.Lat*ky
	px, py := p.Lng*kx, p.Lat*ky

	dx, dy := bx-ax, by-ay
	if dx == 0 && dy == 0 {
		return math.Hypot(px-ax, py-ay)
	}
	t := ((px-ax)*dx + (py-ay)*dy) / (dx*dx + dy*dy)
	t = math.Max(0, math.Min(1, t))
	return math.Hypot(px-(ax+t*dx), py-(ay+t*dy))
}

// Simplify returns a Douglas–Peucker simplification of pts with tolerance in
// metres. Always keeps the first and last points. Used to trim stored route
// polylines so the published corridor stays compact.
func Simplify(pts []Point, toleranceM float64) []Point {
	if len(pts) <= 2 {
		out := make([]Point, len(pts))
		copy(out, pts)
		return out
	}
	keep := make([]bool, len(pts))
	keep[0] = true
	keep[len(pts)-1] = true
	dp(pts, 0, len(pts)-1, toleranceM, keep)
	out := make([]Point, 0, len(pts))
	for i, k := range keep {
		if k {
			out = append(out, pts[i])
		}
	}
	return out
}

func dp(pts []Point, first, last int, tol float64, keep []bool) {
	if last <= first+1 {
		return
	}
	maxDist := -1.0
	idx := -1
	for i := first + 1; i < last; i++ {
		d := perpendicularDistanceM(pts[i], pts[first], pts[last])
		if d > maxDist {
			maxDist = d
			idx = i
		}
	}
	if maxDist > tol && idx > 0 {
		keep[idx] = true
		dp(pts, first, idx, tol, keep)
		dp(pts, idx, last, tol, keep)
	}
}

// DetourKm estimates how far a rider must travel off the straight line:
// (path along corridor between pickup and dropoff) − (straight line), floored
// at 0. The corridor points are passed pre-sliced by the caller.
func DetourKm(corridor []Point, pickup, dropoff Point) float64 {
	along := PathLengthKm(corridor)
	straight := HaversineKm(pickup, dropoff)
	d := along - straight
	if d < 0 {
		return 0
	}
	return d
}
