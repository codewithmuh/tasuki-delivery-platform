// Package demand turns a scenario's demand curve and weather into order arrival times.
package demand

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// Day is the length of a day; the curve and the weather repeat every Day.
const Day = 24 * time.Hour

// Point is one point on a demand curve: the order rate at a time of day.
type Point struct {
	At      time.Duration // time since midnight
	PerHour float64       // orders per hour
}

// Curve is the order rate through a day. The rate changes in straight lines between the points
// and wraps from the last point of one day to the first point of the next, so there is no jump
// at midnight.
type Curve struct {
	// knots are the scenario's points plus one at 00:00 and one at 24:00, so every time of day
	// falls between two knots.
	knots []Point
}

// NewCurve builds a curve from points sorted by time of day.
func NewCurve(points []Point) (Curve, error) {
	if len(points) == 0 {
		return Curve{}, errors.New("a demand curve needs at least one point")
	}
	previous := time.Duration(-1)
	for i, p := range points {
		if p.At < 0 || p.At >= Day {
			return Curve{}, fmt.Errorf("point %d: time of day %v is outside 00:00–23:59", i, p.At)
		}
		if p.PerHour < 0 {
			return Curve{}, fmt.Errorf("point %d: rate %v is negative", i, p.PerHour)
		}
		if p.At <= previous {
			return Curve{}, fmt.Errorf("point %d: times must increase", i)
		}
		previous = p.At
	}

	// The rate at midnight lies on the line from the day's last point to the next day's first.
	first, last := points[0], points[len(points)-1]
	midnight := last.PerHour
	if len(points) > 1 {
		span := (first.At + Day) - last.At
		midnight = last.PerHour + (first.PerHour-last.PerHour)*float64(Day-last.At)/float64(span)
	}

	knots := make([]Point, 0, len(points)+2)
	if first.At > 0 {
		knots = append(knots, Point{At: 0, PerHour: midnight})
	}
	knots = append(knots, points...)
	knots = append(knots, Point{At: Day, PerHour: midnight})
	return Curve{knots: knots}, nil
}

// Rate returns the order rate in orders per hour at simulated time t. Times past 24 h wrap
// around, so the same curve repeats every day.
func (c Curve) Rate(t time.Duration) float64 {
	tod := wrap(t)
	// i is the first knot after tod; knots[0] is at 00:00 and the last at 24:00, so 1 ≤ i < len.
	i := sort.Search(len(c.knots), func(i int) bool { return c.knots[i].At > tod })
	a, b := c.knots[i-1], c.knots[i]
	f := float64(tod-a.At) / float64(b.At-a.At)
	return a.PerHour + (b.PerHour-a.PerHour)*f
}

// Max returns the highest rate on the curve, in orders per hour.
func (c Curve) Max() float64 {
	highest := 0.0
	for _, k := range c.knots {
		highest = max(highest, k.PerHour)
	}
	return highest
}

// Expected returns the expected number of orders between simulated times from and to: the
// area under the curve. Both times count from midnight of the first day.
func (c Curve) Expected(from, to time.Duration) float64 {
	if to <= from {
		return 0
	}
	return c.cumulative(to) - c.cumulative(from)
}

// cumulative returns the expected number of orders from midnight of the first day up to t.
func (c Curve) cumulative(t time.Duration) float64 {
	if t <= 0 {
		return 0
	}
	fullDays := float64(t / Day)
	return fullDays*c.sinceMidnight(Day) + c.sinceMidnight(t%Day)
}

// sinceMidnight returns the expected orders from 00:00 to tod. Each stretch between two knots
// is a trapezoid (the rate is a straight line), so its area is the average rate × the hours.
func (c Curve) sinceMidnight(tod time.Duration) float64 {
	total := 0.0
	for i := 1; i < len(c.knots); i++ {
		a, b := c.knots[i-1], c.knots[i]
		if tod <= a.At {
			break
		}
		if tod < b.At { // tod falls inside this stretch: stop at tod
			b = Point{At: tod, PerHour: c.Rate(tod)}
		}
		total += (a.PerHour + b.PerHour) / 2 * (b.At - a.At).Hours()
	}
	return total
}

// wrap returns the time of day of simulated time t.
func wrap(t time.Duration) time.Duration {
	tod := t % Day
	if tod < 0 {
		tod += Day
	}
	return tod
}
