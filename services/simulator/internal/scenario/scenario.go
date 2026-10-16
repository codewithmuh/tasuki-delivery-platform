// Package scenario loads and checks the YAML files that describe a simulated day in Tokyo:
// which wards, how many merchants and couriers, the demand curve, the weather and the GPS
// settings. See scenarios/*.yaml for examples and the README for every field.
package scenario

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/demand"
)

// Scenario is one simulated day: the city, the people in it and what they do.
type Scenario struct {
	Name          string             `yaml:"name"`
	Description   string             `yaml:"description"`
	Seed          uint64             `yaml:"seed"`
	Duration      time.Duration      `yaml:"duration"` // 0 runs until stopped
	Clock         Clock              `yaml:"clock"`
	Wards         []string           `yaml:"wards"`
	WardWeights   map[string]float64 `yaml:"ward_weights"`  // relative demand; missing wards weigh 1
	WardRadiusM   float64            `yaml:"ward_radius_m"` // stores, couriers and drop-offs stay this close to the centre
	Merchants     int                `yaml:"merchants"`
	StoresPerWard int                `yaml:"stores_per_ward"`
	Couriers      Couriers           `yaml:"couriers"`
	Demand        Demand             `yaml:"demand"`
	Weather       []Weather          `yaml:"weather"`
	GPS           GPS                `yaml:"gps"`
}

// Clock sets the simulated time of day.
type Clock struct {
	// Start is the time of day the simulation begins at. Empty means the real time in Tokyo.
	Start *TimeOfDay `yaml:"start"`
	// Speedup makes couriers ride and wait this many times faster than real time (for demos).
	// Order arrivals and HTTP timestamps stay real.
	Speedup float64 `yaml:"speedup"`
}

// Couriers describes the courier fleet.
type Couriers struct {
	Count        int           `yaml:"count"`
	ScooterShare float64       `yaml:"scooter_share"` // the rest ride bicycles
	BicycleKmh   float64       `yaml:"bicycle_kmh"`
	ScooterKmh   float64       `yaml:"scooter_kmh"`
	Shifts       []Shift       `yaml:"shifts"`
	Accept       Accept        `yaml:"accept"`
	ThinkTime    DurationRange `yaml:"think_time"` // how long a courier looks at an offer
}

// Shift is a working period. Share is the fraction of couriers who work it.
type Shift struct {
	Start TimeOfDay `yaml:"start"`
	End   TimeOfDay `yaml:"end"`
	Share float64   `yaml:"share"`
}

// Accept sets how likely a courier is to accept an offer: Probability for a store next door,
// halving for every HalvingDistanceM metres to the store.
type Accept struct {
	Probability      float64 `yaml:"probability"`
	HalvingDistanceM float64 `yaml:"halving_distance_m"`
}

// Demand sets how many orders arrive and how long stores take to prepare them.
type Demand struct {
	// Curve is the order rate for the whole simulated area through the day. It is split
	// between the wards by WardWeights.
	Curve    []CurvePoint  `yaml:"curve"`
	PrepTime DurationRange `yaml:"prep_time"`
	// MaxInFlight caps concurrent order requests, so a stalled orders service cannot make
	// the simulator pile up unlimited goroutines. Orders over the cap count as failed.
	MaxInFlight int `yaml:"max_in_flight"`
	// WatchShare is the fraction of customers who keep the tracking page open until their
	// order is delivered (design §4 assumes about 60 %). 0 means nobody watches.
	WatchShare float64 `yaml:"watch_share"`
	// MaxViewers caps open tracking streams; viewers over the cap are counted and skipped.
	MaxViewers int `yaml:"max_viewers"`
}

// CurvePoint is the order rate at a time of day; the rate changes linearly between points.
type CurvePoint struct {
	At      TimeOfDay `yaml:"at"`
	PerHour float64   `yaml:"per_hour"`
}

// Weather is a weather event, repeated daily between From and To.
type Weather struct {
	Name             string    `yaml:"name"`
	From             TimeOfDay `yaml:"from"`
	To               TimeOfDay `yaml:"to"`
	DemandMultiplier float64   `yaml:"demand_multiplier"`
	SpeedMultiplier  float64   `yaml:"speed_multiplier"`
	OfflineShare     float64   `yaml:"offline_share"` // share of couriers who log off when it starts
}

// GPS sets how the simulated phones report positions.
type GPS struct {
	MovingInterval time.Duration `yaml:"moving_interval"` // with an order
	IdleInterval   time.Duration `yaml:"idle_interval"`   // waiting for an offer
	NoiseM         float64       `yaml:"noise_m"`         // standard deviation of GPS error
}

// DurationRange is a range of durations; values are drawn evenly between Min and Max.
type DurationRange struct {
	Min time.Duration `yaml:"min"`
	Max time.Duration `yaml:"max"`
}

// TimeOfDay is a time written "HH:MM" in YAML, stored as the time since midnight. "24:00" is
// allowed so a shift or event can run to the end of the day.
type TimeOfDay time.Duration

// UnmarshalText parses "HH:MM".
func (t *TimeOfDay) UnmarshalText(text []byte) error {
	hh, mm, ok := strings.Cut(string(text), ":")
	h, errH := strconv.Atoi(hh)
	m, errM := strconv.Atoi(mm)
	if !ok || errH != nil || errM != nil || len(mm) != 2 || h < 0 || h > 24 || m < 0 || m > 59 || (h == 24 && m > 0) {
		return fmt.Errorf("time of day %q: want HH:MM between 00:00 and 24:00", text)
	}
	*t = TimeOfDay(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute)
	return nil
}

// Duration returns the time since midnight.
func (t TimeOfDay) Duration() time.Duration { return time.Duration(t) }

// String formats the time as HH:MM.
func (t TimeOfDay) String() string {
	d := time.Duration(t)
	return fmt.Sprintf("%02d:%02d", int(d.Hours()), int(d.Minutes())%60)
}

// Load reads, defaults and validates a scenario file.
func Load(path string) (*Scenario, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the path comes from the operator's command line
	if err != nil {
		return nil, fmt.Errorf("read scenario: %w", err)
	}
	return Parse(data)
}

// Parse decodes a scenario from YAML, fills in defaults and validates it. Unknown fields are
// errors, so a typo cannot silently fall back to a default.
func Parse(data []byte) (*Scenario, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var s Scenario
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("parse scenario: %w", err)
	}
	s.applyDefaults()
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("invalid scenario %q: %w", s.Name, err)
	}
	return &s, nil
}

// applyDefaults fills in every setting a scenario may leave out.
func (s *Scenario) applyDefaults() {
	setIfZero(&s.Clock.Speedup, 1)
	setIfZero(&s.WardRadiusM, 1500)
	setIfZero(&s.Merchants, 1)
	setIfZero(&s.StoresPerWard, 3)
	setIfZero(&s.Couriers.BicycleKmh, 15)
	setIfZero(&s.Couriers.ScooterKmh, 25)
	if len(s.Couriers.Shifts) == 0 {
		s.Couriers.Shifts = []Shift{{Start: 0, End: TimeOfDay(24 * time.Hour), Share: 1}}
	}
	setIfZero(&s.Couriers.Accept.Probability, 0.95)
	setIfZero(&s.Couriers.Accept.HalvingDistanceM, 2000)
	if s.Couriers.ThinkTime == (DurationRange{}) {
		s.Couriers.ThinkTime = DurationRange{Min: 2 * time.Second, Max: 8 * time.Second}
	}
	if s.Demand.PrepTime == (DurationRange{}) {
		s.Demand.PrepTime = DurationRange{Min: 5 * time.Minute, Max: 15 * time.Minute}
	}
	setIfZero(&s.Demand.MaxInFlight, 200)
	setIfZero(&s.Demand.MaxViewers, 12000)
	for i := range s.Weather {
		setIfZero(&s.Weather[i].DemandMultiplier, 1)
		setIfZero(&s.Weather[i].SpeedMultiplier, 1)
	}
	setIfZero(&s.GPS.MovingInterval, 4*time.Second)
	setIfZero(&s.GPS.IdleInterval, 15*time.Second)
}

func setIfZero[T comparable](field *T, value T) {
	var zero T
	if *field == zero {
		*field = value
	}
}

// Validate checks every setting and reports all problems at once.
func (s *Scenario) Validate() error {
	var errs []error
	check := func(ok bool, format string, args ...any) {
		if !ok {
			errs = append(errs, fmt.Errorf(format, args...))
		}
	}

	check(s.Name != "", "name is required")
	check(s.Duration >= 0, "duration must not be negative")
	check(s.Clock.Speedup >= 1 && s.Clock.Speedup <= 1000, "clock.speedup must be between 1 and 1000")

	check(len(s.Wards) > 0, "wards: list at least one ward")
	seen := map[string]bool{}
	for _, id := range s.Wards {
		_, known := LookupWard(id)
		check(known, "wards: unknown ward %q (see contracts/data/wards.json)", id)
		check(!seen[id], "wards: %q is listed twice", id)
		seen[id] = true
	}
	for id, w := range s.WardWeights {
		check(seen[id], "ward_weights: %q is not in wards", id)
		check(w > 0, "ward_weights: %q must be above 0", id)
	}
	check(s.WardRadiusM > 0 && s.WardRadiusM <= 5000, "ward_radius_m must be between 0 and 5000")
	check(s.StoresPerWard >= 1, "stores_per_ward must be at least 1")
	check(s.Merchants >= 1 && s.Merchants <= len(s.Wards)*s.StoresPerWard,
		"merchants must be between 1 and the number of stores (%d)", len(s.Wards)*s.StoresPerWard)

	c := s.Couriers
	check(c.Count >= 0 && c.Count <= 20000, "couriers.count must be between 0 and 20000")
	check(c.ScooterShare >= 0 && c.ScooterShare <= 1, "couriers.scooter_share must be between 0 and 1")
	check(c.BicycleKmh > 0 && c.BicycleKmh <= 60, "couriers.bicycle_kmh must be between 0 and 60")
	check(c.ScooterKmh > 0 && c.ScooterKmh <= 60, "couriers.scooter_kmh must be between 0 and 60")
	for i, sh := range c.Shifts {
		check(sh.Start < sh.End, "couriers.shifts[%d]: start %v must be before end %v", i, sh.Start, sh.End)
		check(sh.Share > 0, "couriers.shifts[%d]: share must be above 0", i)
	}
	check(c.Accept.Probability > 0 && c.Accept.Probability <= 1, "couriers.accept.probability must be in (0, 1]")
	check(c.Accept.HalvingDistanceM > 0, "couriers.accept.halving_distance_m must be above 0")
	checkRange(check, "couriers.think_time", c.ThinkTime)

	if _, err := s.Curve(); err != nil {
		errs = append(errs, fmt.Errorf("demand.curve: %w", err))
	}
	checkRange(check, "demand.prep_time", s.Demand.PrepTime)
	check(s.Demand.MaxInFlight >= 1, "demand.max_in_flight must be at least 1")
	check(s.Demand.WatchShare >= 0 && s.Demand.WatchShare <= 1, "demand.watch_share must be between 0 and 1")
	check(s.Demand.MaxViewers >= 1 && s.Demand.MaxViewers <= 50000, "demand.max_viewers must be between 1 and 50000")

	for i, w := range s.Weather {
		check(w.From < w.To, "weather[%d]: from %v must be before to %v", i, w.From, w.To)
		check(w.DemandMultiplier > 0, "weather[%d]: demand_multiplier must be above 0", i)
		check(w.SpeedMultiplier > 0 && w.SpeedMultiplier <= 2, "weather[%d]: speed_multiplier must be in (0, 2]", i)
		check(w.OfflineShare >= 0 && w.OfflineShare <= 1, "weather[%d]: offline_share must be between 0 and 1", i)
	}

	check(s.GPS.MovingInterval > 0, "gps.moving_interval must be above 0")
	check(s.GPS.IdleInterval > 0, "gps.idle_interval must be above 0")
	check(s.GPS.NoiseM >= 0, "gps.noise_m must not be negative")
	return errors.Join(errs...)
}

func checkRange(check func(bool, string, ...any), name string, r DurationRange) {
	check(r.Min >= 0 && r.Min <= r.Max, "%s: need 0 <= min <= max", name)
}

// Curve returns the demand curve as the demand package uses it.
func (s *Scenario) Curve() (demand.Curve, error) {
	points := make([]demand.Point, len(s.Demand.Curve))
	for i, p := range s.Demand.Curve {
		points[i] = demand.Point{At: p.At.Duration(), PerHour: p.PerHour}
	}
	return demand.NewCurve(points)
}

// WeatherEvents returns the weather as the demand package uses it.
func (s *Scenario) WeatherEvents() demand.Weather {
	events := make(demand.Weather, len(s.Weather))
	for i, w := range s.Weather {
		events[i] = demand.Event{
			Name: w.Name, From: w.From.Duration(), To: w.To.Duration(),
			DemandMultiplier: w.DemandMultiplier, SpeedMultiplier: w.SpeedMultiplier, OfflineShare: w.OfflineShare,
		}
	}
	return events
}

// WardShare returns the fraction of the scenario's demand that falls in ward id.
func (s *Scenario) WardShare(id string) float64 {
	total := 0.0
	for _, w := range s.Wards {
		total += s.wardWeight(w)
	}
	return s.wardWeight(id) / total
}

func (s *Scenario) wardWeight(id string) float64 {
	if w, ok := s.WardWeights[id]; ok {
		return w
	}
	return 1
}
