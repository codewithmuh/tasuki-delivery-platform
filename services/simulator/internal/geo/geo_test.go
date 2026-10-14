package geo

import (
	"math"
	"math/rand/v2"
	"testing"
)

var (
	shibuya  = Point{Lat: 35.6580, Lng: 139.7016} // Shibuya station
	shinjuku = Point{Lat: 35.6900, Lng: 139.7005} // Shinjuku station
)

func near(got, want, tolerance float64) bool { return math.Abs(got-want) <= tolerance }

// angleDiff is the smallest difference between two compass bearings, so 359.9 and 0 are 0.1 apart.
func angleDiff(a, b float64) float64 {
	d := math.Mod(math.Abs(a-b), 360)
	return math.Min(d, 360-d)
}

func TestDistance(t *testing.T) {
	// Shibuya → Shinjuku stations is about 3.56 km in a straight line.
	if d := Distance(shibuya, shinjuku); !near(d, 3560, 15) {
		t.Errorf("Distance = %.0f m, want about 3560 m", d)
	}
	if d := Distance(shibuya, shibuya); d != 0 {
		t.Errorf("Distance to itself = %v, want 0", d)
	}
	if a, b := Distance(shibuya, shinjuku), Distance(shinjuku, shibuya); !near(a, b, 1e-9) {
		t.Errorf("Distance is not symmetric: %v vs %v", a, b)
	}
}

func TestBearing(t *testing.T) {
	cases := []struct {
		name string
		to   Point
		want float64
	}{
		{"north", Offset(shibuya, 1000, 0), 0},
		{"east", Offset(shibuya, 0, 1000), 90},
		{"south", Offset(shibuya, -1000, 0), 180},
		{"west", Offset(shibuya, 0, -1000), 270},
	}
	for _, tc := range cases {
		got := Bearing(shibuya, tc.to)
		if angleDiff(got, tc.want) > 0.1 {
			t.Errorf("%s: Bearing = %.2f, want %.0f", tc.name, got, tc.want)
		}
		if got < 0 || got >= 360 {
			t.Errorf("%s: Bearing = %v, outside [0, 360)", tc.name, got)
		}
	}
}

func TestOffsetMovesTheRequestedDistance(t *testing.T) {
	for _, step := range []struct{ north, east float64 }{{1000, 0}, {0, 1000}, {-600, 800}} {
		want := math.Hypot(step.north, step.east)
		if got := Distance(shibuya, Offset(shibuya, step.north, step.east)); !near(got, want, 1) {
			t.Errorf("Offset(%v, %v) moved %.2f m, want %.0f m", step.north, step.east, got, want)
		}
	}
}

func TestRandomInDiscStaysInsideAndIsDeterministic(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	var sumLat, sumLng float64
	const n = 5000
	for range n {
		p := RandomInDisc(r, shibuya, 1500)
		if d := Distance(shibuya, p); d > 1500.5 {
			t.Fatalf("point %.0f m from centre, radius is 1500 m", d)
		}
		sumLat += p.Lat
		sumLng += p.Lng
	}
	// Spread evenly around the centre, the average lands close to it.
	if d := Distance(shibuya, Point{Lat: sumLat / n, Lng: sumLng / n}); d > 50 {
		t.Errorf("mean point is %.0f m from centre, want < 50 m", d)
	}
	a := RandomInDisc(rand.New(rand.NewPCG(7, 7)), shibuya, 1000)
	b := RandomInDisc(rand.New(rand.NewPCG(7, 7)), shibuya, 1000)
	if a != b {
		t.Errorf("same seed gave %v and %v", a, b)
	}
}

func TestNoisyHasTheRequestedSpread(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	var sumSq float64
	const n = 5000
	for range n {
		d := Distance(shibuya, Noisy(r, shibuya, 5))
		sumSq += d * d
	}
	// With σ = 5 m in each of two directions, the mean squared distance is 2σ² = 50 m².
	if rms := math.Sqrt(sumSq / n); !near(rms, math.Sqrt(50), 0.3) {
		t.Errorf("RMS noise = %.2f m, want about %.2f m", rms, math.Sqrt(50))
	}
}

func TestBBox(t *testing.T) {
	box := BBox{MinLat: 35.52, MaxLat: 35.82, MinLng: 139.56, MaxLng: 139.92}
	if !box.Contains(shibuya) {
		t.Error("Shibuya should be inside the 23-ward box")
	}
	outside := Point{Lat: 35.40, Lng: 140.10}
	if box.Contains(outside) {
		t.Error("a point in Chiba should be outside")
	}
	if got := box.Clamp(outside); got != (Point{Lat: 35.52, Lng: 139.92}) || !box.Contains(got) {
		t.Errorf("Clamp = %v", got)
	}
	if got := box.Clamp(shibuya); got != shibuya {
		t.Errorf("Clamp moved an inside point to %v", got)
	}
}
