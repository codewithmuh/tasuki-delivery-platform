package demand

import (
	"math/rand/v2"
	"time"
)

// Arrivals produces the arrival times of a non-homogeneous Poisson process: customers who order
// independently of each other, at a rate that changes through the day.
//
// It uses thinning (Lewis and Shedler, 1979). Candidate arrivals are proposed at a constant
// rate max, which is never below the real rate; the gaps between them are exponentially
// distributed, as in any constant-rate Poisson process. Each candidate at time t is kept with
// probability rate(t)/max. The kept arrivals follow the changing rate exactly.
type Arrivals struct {
	rate func(time.Duration) float64 // orders per hour at a simulated time
	max  float64                     // an upper bound on rate
	rng  *rand.Rand
	now  time.Duration // simulated time of the last candidate
}

// NewArrivals starts a process at simulated time start. ceiling must be at least the highest
// value rate can return; the same random source and inputs always give the same arrival times.
func NewArrivals(r *rand.Rand, start time.Duration, ceiling float64, rate func(time.Duration) float64) *Arrivals {
	return &Arrivals{rate: rate, max: ceiling, rng: r, now: start}
}

// Next returns the simulated time of the next arrival. It returns false when no order can ever
// arrive because the upper bound is zero.
func (a *Arrivals) Next() (time.Duration, bool) {
	if a.max <= 0 {
		return 0, false
	}
	for {
		gapHours := a.rng.ExpFloat64() / a.max // mean gap is 1/max hours
		a.now += time.Duration(gapHours * float64(time.Hour))
		if a.rng.Float64()*a.max < a.rate(a.now) {
			return a.now, true
		}
	}
}
