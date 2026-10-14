package demand

import "time"

// Event is a stretch of weather, repeated every day between From and To, that changes how many
// orders come in, how fast couriers ride, and how many couriers stop working.
type Event struct {
	Name             string
	From, To         time.Duration // time of day; the event is active from From until just before To
	DemandMultiplier float64       // 3 means three times the usual orders
	SpeedMultiplier  float64       // 0.85 means couriers ride 15 % slower
	OfflineShare     float64       // share of couriers who log off when the event starts
}

// Active reports whether the event is in effect at simulated time t.
func (e Event) Active(t time.Duration) bool {
	tod := wrap(t)
	return tod >= e.From && tod < e.To
}

// Weather is every weather event in a scenario. Overlapping events multiply.
type Weather []Event

// DemandMultiplier returns how much the weather multiplies demand at simulated time t (1 when
// no event is active).
func (w Weather) DemandMultiplier(t time.Duration) float64 {
	m := 1.0
	for _, e := range w {
		if e.Active(t) {
			m *= e.DemandMultiplier
		}
	}
	return m
}

// SpeedMultiplier returns how much the weather multiplies courier speed at simulated time t.
func (w Weather) SpeedMultiplier(t time.Duration) float64 {
	m := 1.0
	for _, e := range w {
		if e.Active(t) {
			m *= e.SpeedMultiplier
		}
	}
	return m
}

// MaxDemandMultiplier returns an upper bound on DemandMultiplier: the product of every
// multiplier above 1, as if all events overlapped. The arrival process needs such a bound.
func (w Weather) MaxDemandMultiplier() float64 {
	m := 1.0
	for _, e := range w {
		if e.DemandMultiplier > 1 {
			m *= e.DemandMultiplier
		}
	}
	return m
}
