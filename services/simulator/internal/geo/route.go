package geo

import (
	"errors"
	"sort"
)

// Route is a path through a list of points. It precomputes how far each point is from the
// start, so finding "where is the courier after 1.2 km" is a binary search, not a walk.
type Route struct {
	points []Point
	cum    []float64 // cum[i] is the distance in metres from points[0] to points[i]
}

// NewRoute builds a route through points, dropping consecutive duplicates (OSRM repeats the
// snapped start point, for example). It needs at least one point.
func NewRoute(points []Point) (*Route, error) {
	if len(points) == 0 {
		return nil, errors.New("a route needs at least one point")
	}
	r := &Route{points: []Point{points[0]}, cum: []float64{0}}
	for _, p := range points[1:] {
		last := r.points[len(r.points)-1]
		if p == last {
			continue
		}
		r.cum = append(r.cum, r.cum[len(r.cum)-1]+Distance(last, p))
		r.points = append(r.points, p)
	}
	return r, nil
}

// Length returns the total length of the route in metres.
func (r *Route) Length() float64 { return r.cum[len(r.cum)-1] }

// End returns the last point of the route.
func (r *Route) End() Point { return r.points[len(r.points)-1] }

// At returns the position after travelling d metres from the start, and the heading in degrees
// of the stretch of road at that position. d is clamped to the route, so a courier who
// overshoots simply stands at the end.
func (r *Route) At(d float64) (Point, float64) {
	if len(r.points) == 1 {
		return r.points[0], 0
	}
	if d <= 0 {
		return r.points[0], Bearing(r.points[0], r.points[1])
	}
	if d >= r.Length() {
		n := len(r.points)
		return r.points[n-1], Bearing(r.points[n-2], r.points[n-1])
	}
	// i is the first point at or beyond d, so the courier is between points i-1 and i.
	i := sort.SearchFloat64s(r.cum, d)
	a, b := r.points[i-1], r.points[i]
	f := (d - r.cum[i-1]) / (r.cum[i] - r.cum[i-1]) // fraction of this segment already covered
	p := Point{Lat: a.Lat + (b.Lat-a.Lat)*f, Lng: a.Lng + (b.Lng-a.Lng)*f}
	return p, Bearing(a, b)
}
