// Package clock maps real time to the simulated time of day in Tokyo.
//
// The simulated day runs at real speed, so order rates and HTTP timestamps stay real: an order
// placed now carries the real time, and a lunch peak lasts two real hours. A scenario may start
// the day at a chosen time, so a demo can begin at the lunch peak whenever it is run.
// Separately, the speed-up factor makes couriers ride and wait faster than real time, so a
// 30-minute delivery can be watched in 3 minutes.
package clock

import "time"

// Tokyo is Japan Standard Time. Japan has no daylight saving time, so a fixed offset is exact
// and the binary does not need the time zone database (the distroless image has none).
var Tokyo = time.FixedZone("JST", 9*60*60)

// Day is the length of a simulated day.
const Day = 24 * time.Hour

// Clock tells the simulation what time of day it is.
type Clock struct {
	realStart time.Time
	simStart  time.Duration // simulated time since midnight at realStart
	speedup   float64
}

// New returns a clock that reads startOfDay (time since midnight) at the real time realStart.
// Speedup is how much faster than real time couriers move and wait; values below 1 mean 1.
func New(realStart time.Time, startOfDay time.Duration, speedup float64) Clock {
	if speedup < 1 {
		speedup = 1
	}
	return Clock{realStart: realStart, simStart: startOfDay, speedup: speedup}
}

// SinceMidnight returns how long after midnight in Tokyo t is.
func SinceMidnight(t time.Time) time.Duration {
	y, m, d := t.In(Tokyo).Date()
	return t.Sub(time.Date(y, m, d, 0, 0, 0, 0, Tokyo))
}

// SimTime returns the simulated time at real time t, measured from midnight of the first
// simulated day. It keeps growing past 24 h on runs that last more than a day.
func (c Clock) SimTime(t time.Time) time.Duration { return c.simStart + t.Sub(c.realStart) }

// TimeOfDay returns the simulated time of day at real time t, between 0 and 24 h.
func (c Clock) TimeOfDay(t time.Time) time.Duration { return c.SimTime(t) % Day }

// RealTime returns the real time at which the simulated clock reads sim. It is the inverse of
// SimTime, used to sleep until the next order is due.
func (c Clock) RealTime(sim time.Duration) time.Time { return c.realStart.Add(sim - c.simStart) }

// Speedup returns how much faster than real time couriers move and wait.
func (c Clock) Speedup() float64 { return c.speedup }

// Scale converts a duration of courier activity (riding, waiting for food) into real time:
// with a speed-up of 10, ten minutes of waiting takes one real minute.
func (c Clock) Scale(d time.Duration) time.Duration { return time.Duration(float64(d) / c.speedup) }
