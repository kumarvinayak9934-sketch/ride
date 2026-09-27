package routes

import (
	"testing"
	"time"

	"github.com/kumarvinayak9934-sketch/ride/apps/api/pkg/geo"
)

func validReq() PublishRequest {
	return PublishRequest{
		VehicleID:     "6f0a1e2b-1234-4567-8901-234567890abc",
		OriginLabel:   "Koramangala",
		DestLabel:     "Whitefield",
		Waypoints:     []Waypoint{{12.9352, 77.6245}, {12.9698, 77.7500}},
		DepartureTime: "08:30",
		WeekdayMask:   0b0011111, // Mon–Fri
		DurationMin:   45,
		Seats:         3,
	}
}

func TestPublishValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*PublishRequest)
		ok     bool
	}{
		{"valid", func(r *PublishRequest) {}, true},
		{"one waypoint", func(r *PublishRequest) { r.Waypoints = r.Waypoints[:1] }, false},
		{"zero waypoints", func(r *PublishRequest) { r.Waypoints = nil }, false},
		{"lat out of range", func(r *PublishRequest) { r.Waypoints[0].Lat = 123 }, false},
		{"lng out of range", func(r *PublishRequest) { r.Waypoints[1].Lng = -400 }, false},
		{"mask too big", func(r *PublishRequest) { r.WeekdayMask = 128 }, false},
		{"mask zero", func(r *PublishRequest) { r.WeekdayMask = 0 }, false},
		{"mask negative", func(r *PublishRequest) { r.WeekdayMask = -1 }, false},
		{"bad time", func(r *PublishRequest) { r.DepartureTime = "8:30am" }, false},
		{"time with seconds", func(r *PublishRequest) { r.DepartureTime = "08:30:00" }, false},
		{"zero duration", func(r *PublishRequest) { r.DurationMin = 0 }, false},
		{"duration too big", func(r *PublishRequest) { r.DurationMin = 2000 }, false},
		{"zero seats", func(r *PublishRequest) { r.Seats = 0 }, false},
		{"too many seats", func(r *PublishRequest) { r.Seats = 12 }, false},
		{"missing origin", func(r *PublishRequest) { r.OriginLabel = "" }, false},
		{"missing dest", func(r *PublishRequest) { r.DestLabel = "" }, false},
		{"missing vehicle", func(r *PublishRequest) { r.VehicleID = "" }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := validReq()
			c.mutate(&req)
			err := req.validate()
			if c.ok && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !c.ok && err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

// Weekday mask round-trip: bit0 = Monday per §5.1.
func TestWeekdayBitIndex(t *testing.T) {
	bit := func(tm time.Time) int {
		return (int(tm.Weekday()) + 6) % 7
	}
	mon := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC) // a Monday
	sun := time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC) // a Sunday
	if bit(mon) != 0 {
		t.Fatalf("Monday bit = %d, want 0", bit(mon))
	}
	if bit(sun) != 6 {
		t.Fatalf("Sunday bit = %d, want 6", bit(sun))
	}
	// Mon-Fri mask = 0b0011111 = 31; weekend mask = 0b1000001 = 65
	weekdays := 0
	for d := 0; d < 7; d++ {
		day := mon.AddDate(0, 0, d)
		if 31&(1<<bit(day)) != 0 {
			weekdays++
		}
	}
	if weekdays != 5 {
		t.Fatalf("weekday mask selected %d days, want 5", weekdays)
	}
}

func TestBuildLineString(t *testing.T) {
	got := buildLineString([]geo.Point{{Lat: 12.9, Lng: 77.6}, {Lat: 12.97, Lng: 77.75}})
	want := "LINESTRING(77.600000 12.900000,77.750000 12.970000)"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
