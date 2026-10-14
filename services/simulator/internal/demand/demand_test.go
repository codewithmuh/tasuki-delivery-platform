package demand

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"
)

const h = time.Hour

// lunchCurve is quiet (10 orders/h) at night, ramps to 100/h for lunch, and back down.
func lunchCurve(t *testing.T) Curve {
	t.Helper()
	c, err := NewCurve([]Point{
		{At: 6 * h, PerHour: 10},
		{At: 11 * h, PerHour: 100},
		{At: 13 * h, PerHour: 100},
		{At: 15 * h, PerHour: 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCurveRate(t *testing.T) {
	c := lunchCurve(t)
	cases := []struct {
		at   time.Duration
		want float64
	}{
		{0, 10},                    // between 15:00 (10) and 06:00 (10): flat overnight
		{6 * h, 10},                // on a point
		{8*h + 30*time.Minute, 55}, // half way up the morning ramp
		{12 * h, 100},              // lunch plateau
		{14 * h, 55},               // half way down
		{24*h + 12*h, 100},         // the curve repeats the next day
	}
	for _, tc := range cases {
		if got := c.Rate(tc.at); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("Rate(%v) = %v, want %v", tc.at, got, tc.want)
		}
	}
	if c.Max() != 100 {
		t.Errorf("Max = %v, want 100", c.Max())
	}
}

func TestCurveWrapsSmoothlyAtMidnight(t *testing.T) {
	// 20:00 → 100/h and 04:00 → 20/h: midnight is half way, so 60/h.
	c, err := NewCurve([]Point{{At: 4 * h, PerHour: 20}, {At: 20 * h, PerHour: 100}})
	if err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Duration{0, 24 * h} {
		if got := c.Rate(at); math.Abs(got-60) > 1e-9 {
			t.Errorf("Rate(%v) = %v, want 60", at, got)
		}
	}
	if got := c.Rate(22 * h); math.Abs(got-80) > 1e-9 {
		t.Errorf("Rate(22h) = %v, want 80", got)
	}
}

func TestSinglePointCurveIsFlat(t *testing.T) {
	c, err := NewCurve([]Point{{At: 9 * h, PerHour: 42}})
	if err != nil {
		t.Fatal(err)
	}
	if c.Rate(3*h) != 42 || c.Rate(23*h) != 42 || c.Expected(0, 24*h) != 42*24 {
		t.Errorf("flat curve: Rate(3h)=%v Expected(day)=%v", c.Rate(3*h), c.Expected(0, 24*h))
	}
}

func TestExpectedOrdersPerHour(t *testing.T) {
	c := lunchCurve(t)
	// Expected orders in an hour = area under the curve for that hour.
	cases := []struct {
		hour int
		want float64
	}{
		{3, 10},    // flat at 10/h
		{6, 19},    // 10 → 28 over the hour: average 19
		{10, 91},   // 82 → 100: average 91
		{11, 100},  // the lunch plateau
		{12, 100},  //
		{13, 77.5}, // 100 → 55
		{14, 32.5}, // 55 → 10
		{20, 10},
	}
	for _, tc := range cases {
		from := time.Duration(tc.hour) * h
		if got := c.Expected(from, from+h); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("hour %02d: Expected = %v, want %v", tc.hour, got, tc.want)
		}
	}

	// The hours add up to the day, and the day repeats.
	var sum float64
	for hour := range 24 {
		from := time.Duration(hour) * h
		sum += c.Expected(from, from+h)
	}
	day := c.Expected(0, 24*h)
	if math.Abs(sum-day) > 1e-9 {
		t.Errorf("sum of hours %v != day total %v", sum, day)
	}
	// 15 h at 10/h, ramps 5 h × 55 avg and 2 h × 55 avg, plateau 2 h × 100.
	if want := 15*10 + 5*55 + 2*55 + 2*100.0; math.Abs(day-want) > 1e-9 {
		t.Errorf("day total = %v, want %v", day, want)
	}
	if got := c.Expected(24*h+11*h, 24*h+12*h); math.Abs(got-100) > 1e-9 {
		t.Errorf("second day lunch hour = %v, want 100", got)
	}
	if got := c.Expected(5*h, 5*h); got != 0 {
		t.Errorf("empty interval = %v, want 0", got)
	}
}

func TestNewCurveRejectsBadPoints(t *testing.T) {
	bad := map[string][]Point{
		"empty":         nil,
		"negative rate": {{At: h, PerHour: -1}},
		"not sorted":    {{At: 2 * h, PerHour: 1}, {At: h, PerHour: 1}},
		"duplicate":     {{At: h, PerHour: 1}, {At: h, PerHour: 2}},
		"24:00":         {{At: 24 * h, PerHour: 1}},
	}
	for name, points := range bad {
		if _, err := NewCurve(points); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestWeather(t *testing.T) {
	w := Weather{
		{Name: "rain", From: 11*h + 30*time.Minute, To: 24 * h, DemandMultiplier: 3, SpeedMultiplier: 0.85},
		{Name: "storm", From: 18 * h, To: 19 * h, DemandMultiplier: 1.5, SpeedMultiplier: 0.5},
	}
	if got := w.DemandMultiplier(11 * h); got != 1 {
		t.Errorf("before the rain: %v, want 1", got)
	}
	if got := w.DemandMultiplier(12 * h); got != 3 {
		t.Errorf("in the rain: %v, want 3", got)
	}
	if got := w.DemandMultiplier(18*h + 30*time.Minute); got != 4.5 {
		t.Errorf("rain and storm overlap: %v, want 4.5", got)
	}
	if got := w.SpeedMultiplier(12 * h); got != 0.85 {
		t.Errorf("speed in the rain: %v, want 0.85", got)
	}
	if got := w.SpeedMultiplier(24*h + 12*h); got != 0.85 {
		t.Errorf("the weather repeats the next day: %v", got)
	}
	if got := w.MaxDemandMultiplier(); got != 4.5 {
		t.Errorf("MaxDemandMultiplier = %v, want 4.5", got)
	}
}

func TestArrivalsAreDeterministic(t *testing.T) {
	c := lunchCurve(t)
	draw := func(seed uint64) []time.Duration {
		a := NewArrivals(rand.New(rand.NewPCG(seed, 1)), 11*h, c.Max(), c.Rate)
		var out []time.Duration
		for range 50 {
			at, _ := a.Next()
			out = append(out, at)
		}
		return out
	}
	first, again, other := draw(7), draw(7), draw(8)
	for i := range first {
		if first[i] != again[i] {
			t.Fatalf("arrival %d differs with the same seed: %v vs %v", i, first[i], again[i])
		}
		if i > 0 && first[i] <= first[i-1] {
			t.Fatalf("arrival times must increase: %v then %v", first[i-1], first[i])
		}
	}
	if first[0] == other[0] && first[1] == other[1] {
		t.Error("a different seed gave the same arrivals")
	}
}

func TestArrivalsFollowTheCurve(t *testing.T) {
	// Run the process for many simulated days and compare the average number of arrivals in
	// each hour with the curve's expected count. Thinning must reproduce the shape.
	c := lunchCurve(t)
	weather := Weather{{From: 12 * h, To: 13 * h, DemandMultiplier: 2, SpeedMultiplier: 1}}
	rate := func(at time.Duration) float64 { return c.Rate(at) * weather.DemandMultiplier(at) }
	a := NewArrivals(rand.New(rand.NewPCG(2026, 10)), 0, c.Max()*weather.MaxDemandMultiplier(), rate)

	const days = 300
	var counts [24]float64
	for {
		at, _ := a.Next()
		if at >= days*24*h {
			break
		}
		counts[int(at%(24*h)/h)]++
	}
	for hour, total := range counts {
		from := time.Duration(hour) * h
		want := c.Expected(from, from+h)
		if hour == 12 {
			want *= 2 // the weather event doubles the 12:00 hour
		}
		got := total / days
		// The count in an hour over 300 days has a standard deviation of sqrt(want/300);
		// allow four of them plus a little.
		if tolerance := 4*math.Sqrt(want/days) + 0.5; math.Abs(got-want) > tolerance {
			t.Errorf("hour %02d: %.2f orders on average, want %.2f ± %.2f", hour, got, want, tolerance)
		}
	}
}

func TestNoArrivalsWhenTheRateIsZero(t *testing.T) {
	a := NewArrivals(rand.New(rand.NewPCG(1, 1)), 0, 0, func(time.Duration) float64 { return 0 })
	if _, ok := a.Next(); ok {
		t.Error("expected no arrivals")
	}
}
