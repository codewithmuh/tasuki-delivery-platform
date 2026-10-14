package geo

import "testing"

func TestNewRouteNeedsAPoint(t *testing.T) {
	if _, err := NewRoute(nil); err == nil {
		t.Fatal("expected an error for an empty route")
	}
}

func TestRouteLengthAndDuplicates(t *testing.T) {
	east := Offset(shibuya, 0, 300)
	north := Offset(east, 400, 0)
	// OSRM often repeats a point; duplicates must not create zero-length segments.
	r, err := NewRoute([]Point{shibuya, shibuya, east, east, north})
	if err != nil {
		t.Fatal(err)
	}
	if !near(r.Length(), 700, 1) {
		t.Errorf("Length = %.2f, want 700", r.Length())
	}
	if r.End() != north {
		t.Errorf("End = %v, want %v", r.End(), north)
	}
}

func TestRouteAt(t *testing.T) {
	east := Offset(shibuya, 0, 300) // 300 m east
	north := Offset(east, 400, 0)   // then 400 m north
	r, _ := NewRoute([]Point{shibuya, east, north})

	cases := []struct {
		name        string
		d           float64
		want        Point
		wantHeading float64
	}{
		{"before the start is the start", -5, shibuya, 90},
		{"half way along the first leg", 150, Offset(shibuya, 0, 150), 90},
		{"just before the corner", 299.5, Offset(shibuya, 0, 299.5), 90},
		{"on the second leg", 500, Offset(east, 200, 0), 0},
		{"past the end is the end", 10_000, north, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, heading := r.At(tc.d)
			if d := Distance(got, tc.want); d > 0.5 {
				t.Errorf("At(%v) is %.2f m from the expected point", tc.d, d)
			}
			if angleDiff(heading, tc.wantHeading) > 0.5 {
				t.Errorf("heading = %.2f, want %.0f", heading, tc.wantHeading)
			}
		})
	}
}

func TestRouteSinglePoint(t *testing.T) {
	r, _ := NewRoute([]Point{shibuya, shibuya})
	if r.Length() != 0 {
		t.Errorf("Length = %v, want 0", r.Length())
	}
	if p, h := r.At(10); p != shibuya || h != 0 {
		t.Errorf("At = %v, %v", p, h)
	}
}

func TestTravelAtBicycleSpeed(t *testing.T) {
	// A bicycle at 15 km/h covers 250 m in a minute. Moving along the route by
	// speed × time puts the courier 250 m along a straight 1 km road.
	r, _ := NewRoute([]Point{shibuya, Offset(shibuya, 1000, 0)})
	speedMPS := 15.0 * 1000 / 3600
	got, _ := r.At(speedMPS * 60)
	if d := Distance(shibuya, got); !near(d, 250, 0.5) {
		t.Errorf("after 60 s the courier is %.1f m along, want 250 m", d)
	}
}
