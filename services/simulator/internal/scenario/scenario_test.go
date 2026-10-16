package scenario

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/scenarios"
)

func builtin(t *testing.T, name string) *Scenario {
	t.Helper()
	data, ok := scenarios.Get(name)
	if !ok {
		t.Fatalf("no built-in scenario %q", name)
	}
	s, err := Parse(data)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return s
}

func TestBuiltinScenariosAreValid(t *testing.T) {
	names := scenarios.Names()
	if len(names) < 3 {
		t.Fatalf("expected at least 3 built-in scenarios, got %v", names)
	}
	for _, name := range names {
		s := builtin(t, name)
		if s.Name != name {
			t.Errorf("file %s.yaml has name %q; keep them equal", name, s.Name)
		}
	}
}

func TestNormalWeekdayMatchesTheDesignAtOneTenthScale(t *testing.T) {
	s := builtin(t, "normal-weekday")
	curve, err := s.Curve()
	if err != nil {
		t.Fatal(err)
	}
	// docs/design.md §4: 50,000 orders a day, the busiest hour carries 12 % (6,000/h).
	// At one-tenth scale: about 5,000 a day and 600 in the busiest hour.
	day := curve.Expected(0, 24*time.Hour)
	lunch := curve.Expected(12*time.Hour, 13*time.Hour)
	if lunch != 600 {
		t.Errorf("12:00–13:00 expects %.1f orders, want 600", lunch)
	}
	if day < 4800 || day > 5500 {
		t.Errorf("expected %.0f orders a day, want about 5,000", day)
	}
	if share := lunch / day; share < 0.10 || share > 0.13 {
		t.Errorf("busiest hour carries %.1f %% of the day, want about 12 %%", share*100)
	}
	for _, peak := range []struct {
		name     string
		from, to time.Duration
	}{
		{"lunch", 11*time.Hour + 30*time.Minute, 13*time.Hour + 30*time.Minute},
		{"dinner", 18 * time.Hour, 20*time.Hour + 30*time.Minute},
	} {
		perHour := curve.Expected(peak.from, peak.to) / (peak.to - peak.from).Hours()
		if offPeak := curve.Expected(15*time.Hour, 16*time.Hour); perHour < 2*offPeak {
			t.Errorf("%s peak averages %.0f/h, want at least twice the afternoon's %.0f/h", peak.name, perHour, offPeak)
		}
	}
}

func TestRainyFridayTriplesDemandFromHalfPastEleven(t *testing.T) {
	s := builtin(t, "rainy-friday")
	w := s.WeatherEvents()
	before, after := 11*time.Hour+29*time.Minute, 11*time.Hour+30*time.Minute
	if w.DemandMultiplier(before) != 1 || w.DemandMultiplier(after) != 3 {
		t.Errorf("demand multiplier: %v before, %v after 11:30", w.DemandMultiplier(before), w.DemandMultiplier(after))
	}
	if got := w.SpeedMultiplier(after); math.Abs(got-0.85) > 1e-9 {
		t.Errorf("speed multiplier in the rain = %v, want 0.85", got)
	}
	if got := s.Weather[0].OfflineShare; got != 0.10 {
		t.Errorf("offline share = %v, want 0.10", got)
	}
}

func TestDesignScaleScenarioMatchesTheDesign(t *testing.T) {
	s := builtin(t, "rainy-friday-design-scale")
	curve, err := s.Curve()
	if err != nil {
		t.Fatal(err)
	}
	// docs/design.md §4: 50,000 orders a day, 6,000 in the busiest hour, ×3 in the rain ≈ 5/s.
	if day := curve.Expected(0, 24*time.Hour); day < 48_000 || day > 55_000 {
		t.Errorf("expected %.0f orders a day before weather, want about 50,000", day)
	}
	if peak := curve.Expected(12*time.Hour, 13*time.Hour); peak != 6000 {
		t.Errorf("12:00–13:00 expects %.0f orders, want 6,000", peak)
	}
	noon := 12 * time.Hour
	if perSecond := curve.Rate(noon) * s.WeatherEvents().DemandMultiplier(noon) / 3600; perSecond != 5 {
		t.Errorf("rainy lunch peak = %.2f orders/s, want 5", perSecond)
	}
	// About 3,000 couriers online at the lunch peak (before 10 % log off in the rain).
	var onShare, total float64
	for _, sh := range s.Couriers.Shifts {
		total += sh.Share
		if sh.Start.Duration() <= noon && noon < sh.End.Duration() {
			onShare += sh.Share
		}
	}
	if online := float64(s.Couriers.Count) * onShare / total; online < 2800 || online > 3200 {
		t.Errorf("%.0f couriers online at 12:00, want about 3,000", online)
	}
	// GPS at the design's ~750 points/s: 3,000 couriers reporting every 4 s.
	if s.GPS.MovingInterval != 4*time.Second {
		t.Errorf("moving GPS interval = %v, want 4s", s.GPS.MovingInterval)
	}
	// About 60 % of customers watch (§4), and the cap leaves room for the 10,000 viewers tracking is sized for.
	if s.Demand.WatchShare != 0.6 || s.Demand.MaxViewers < 10_000 {
		t.Errorf("watch_share %.2f, max_viewers %d: want 0.6 and at least 10,000", s.Demand.WatchShare, s.Demand.MaxViewers)
	}
}

func TestSmallScenarioIsLaptopSized(t *testing.T) {
	s := builtin(t, "small")
	if s.Couriers.Count != 50 || len(s.Wards) != 2 {
		t.Errorf("small: %d couriers in %d wards, want 50 in 2", s.Couriers.Count, len(s.Wards))
	}
}

func TestDefaults(t *testing.T) {
	s, err := Parse([]byte(`
name: minimal
wards: [shibuya]
demand:
  curve: [{ at: "12:00", per_hour: 10 }]
`))
	if err != nil {
		t.Fatal(err)
	}
	checks := map[string]bool{
		"speedup 1":           s.Clock.Speedup == 1,
		"radius 1500 m":       s.WardRadiusM == 1500,
		"1 merchant":          s.Merchants == 1,
		"3 stores per ward":   s.StoresPerWard == 3,
		"bicycle 15 km/h":     s.Couriers.BicycleKmh == 15,
		"scooter 25 km/h":     s.Couriers.ScooterKmh == 25,
		"one all-day shift":   len(s.Couriers.Shifts) == 1 && s.Couriers.Shifts[0].End.Duration() == 24*time.Hour,
		"accept 0.95":         s.Couriers.Accept.Probability == 0.95,
		"halving 2 km":        s.Couriers.Accept.HalvingDistanceM == 2000,
		"think 2–8 s":         s.Couriers.ThinkTime == DurationRange{Min: 2 * time.Second, Max: 8 * time.Second},
		"prep 5–15 min":       s.Demand.PrepTime == DurationRange{Min: 5 * time.Minute, Max: 15 * time.Minute},
		"200 in flight":       s.Demand.MaxInFlight == 200,
		"nobody watches":      s.Demand.WatchShare == 0,
		"12,000 viewers max":  s.Demand.MaxViewers == 12000,
		"GPS 4 s moving":      s.GPS.MovingInterval == 4*time.Second,
		"GPS 15 s idle":       s.GPS.IdleInterval == 15*time.Second,
		"real time of day":    s.Clock.Start == nil,
		"no couriers":         s.Couriers.Count == 0,
		"runs until stopped":  s.Duration == 0,
		"perfect GPS":         s.GPS.NoiseM == 0,
		"no weather":          len(s.Weather) == 0,
		"weight 1 by default": s.WardShare("shibuya") == 1,
	}
	for name, ok := range checks {
		if !ok {
			t.Errorf("default not applied: %s", name)
		}
	}
}

func TestWeatherDefaultsToNoEffect(t *testing.T) {
	s, err := Parse([]byte(`
name: drizzle
wards: [shibuya]
demand: { curve: [{ at: "12:00", per_hour: 10 }] }
weather: [{ name: drizzle, from: "09:00", to: "10:00" }]
`))
	if err != nil {
		t.Fatal(err)
	}
	if w := s.Weather[0]; w.DemandMultiplier != 1 || w.SpeedMultiplier != 1 {
		t.Errorf("weather multipliers default to %v and %v, want 1", w.DemandMultiplier, w.SpeedMultiplier)
	}
}

func TestValidationReportsEveryProblem(t *testing.T) {
	_, err := Parse([]byte(`
duration: -1s
clock: { speedup: 5000 }
wards: [shibuya, atlantis, shibuya]
ward_weights: { minato: 2, shibuya: -1 }
ward_radius_m: 9000
merchants: 99
couriers:
  count: -1
  scooter_share: 2
  bicycle_kmh: 100
  scooter_kmh: -5
  shifts: [{ start: "14:00", end: "10:00", share: 0 }]
  accept: { probability: 1.5, halving_distance_m: -1 }
  think_time: { min: 9s, max: 1s }
demand:
  curve: [{ at: "12:00", per_hour: 10 }, { at: "11:00", per_hour: 10 }]
  prep_time: { min: -1m, max: 1m }
  max_in_flight: -3
  watch_share: 1.5
  max_viewers: -1
weather: [{ from: "18:00", to: "17:00", demand_multiplier: -2, speed_multiplier: 5, offline_share: 2 }]
gps: { moving_interval: -1s, idle_interval: -1s, noise_m: -3 }
`))
	if err == nil {
		t.Fatal("expected validation errors")
	}
	for _, want := range []string{
		"name is required", "duration must not be negative", "clock.speedup", `unknown ward "atlantis"`,
		`"shibuya" is listed twice`, `ward_weights: "minato" is not in wards`, `"shibuya" must be above 0`,
		"ward_radius_m", "merchants must be between 1 and the number of stores (9)", "couriers.count",
		"scooter_share", "bicycle_kmh", "scooter_kmh", "shifts[0]: start 14:00 must be before end 10:00",
		"shifts[0]: share", "accept.probability", "halving_distance_m", "think_time", "demand.curve",
		"prep_time", "max_in_flight", "demand.watch_share", "demand.max_viewers", "weather[0]: from 18:00", "weather[0]: demand_multiplier",
		"weather[0]: speed_multiplier", "weather[0]: offline_share", "gps.moving_interval",
		"gps.idle_interval", "gps.noise_m",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q", want)
		}
	}
}

func TestUnknownFieldsAreRejected(t *testing.T) {
	_, err := Parse([]byte("name: typo\nwards: [shibuya]\ncourier: { count: 5 }\n"))
	if err == nil || !strings.Contains(err.Error(), "field courier not found") {
		t.Fatalf("expected an unknown-field error, got %v", err)
	}
}

func TestTimeOfDay(t *testing.T) {
	valid := map[string]time.Duration{
		"00:00": 0, "11:30": 11*time.Hour + 30*time.Minute, "9:05": 9*time.Hour + 5*time.Minute, "24:00": 24 * time.Hour,
	}
	for in, want := range valid {
		var tod TimeOfDay
		if err := tod.UnmarshalText([]byte(in)); err != nil || tod.Duration() != want {
			t.Errorf("%q: got %v, %v; want %v", in, tod.Duration(), err, want)
		}
	}
	for _, in := range []string{"", "11", "11:3", "11:60", "25:00", "24:01", "-1:00", "ab:cd", "11:30:00"} {
		var tod TimeOfDay
		if err := tod.UnmarshalText([]byte(in)); err == nil {
			t.Errorf("%q: expected an error", in)
		}
	}
	if got := TimeOfDay(13*time.Hour + 5*time.Minute).String(); got != "13:05" {
		t.Errorf("String = %q", got)
	}
}

func TestWardShare(t *testing.T) {
	s := &Scenario{Wards: []string{"shibuya", "minato", "chuo"}, WardWeights: map[string]float64{"minato": 2}}
	if got := s.WardShare("minato"); got != 0.5 {
		t.Errorf("minato share = %v, want 0.5", got)
	}
	if got := s.WardShare("chuo"); got != 0.25 {
		t.Errorf("chuo share = %v, want 0.25", got)
	}
}

func TestLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.yaml")
	if err := os.WriteFile(path, []byte("name: file\nwards: [chuo]\ndemand: { curve: [{ at: \"00:00\", per_hour: 1 }] }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := Load(path); err != nil || s.Name != "file" {
		t.Fatalf("Load = %v, %v", s, err)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("expected an error for a missing file")
	}
	if _, err := Parse([]byte("name: [not, a, string]")); err == nil {
		t.Fatal("expected a YAML type error")
	}
}

func TestWards(t *testing.T) {
	ids := AllWardIDs()
	if len(ids) != 23 {
		t.Fatalf("got %d wards, want Tokyo's 23", len(ids))
	}
	w, ok := LookupWard("shibuya")
	if !ok || w.NameJA != "渋谷区" {
		t.Fatalf("LookupWard(shibuya) = %+v, %v", w, ok)
	}
	if !CityBBox().Contains(w.Centre()) {
		t.Error("Shibuya's centre should be inside the city box")
	}
	if _, ok := LookupWard("yokohama"); ok {
		t.Error("Yokohama is not one of the 23 wards")
	}
	if _, err := parseCity([]byte("{")); err == nil {
		t.Error("expected an error for broken JSON")
	}
	if _, err := parseCity([]byte(`{"wards": []}`)); err == nil {
		t.Error("expected an error for no wards")
	}
}

// The simulator embeds a copy of contracts/data/wards.json. This test keeps the copy honest.
func TestEmbeddedWardsMatchContract(t *testing.T) {
	contract, err := os.ReadFile("../../../../contracts/data/wards.json")
	if err != nil {
		t.Fatalf("read the contract (run tests from a full checkout): %v", err)
	}
	if !bytes.Equal(contract, wardsJSON) {
		t.Fatal("internal/scenario/wards.json differs from contracts/data/wards.json; " +
			"copy it: cp contracts/data/wards.json services/simulator/internal/scenario/wards.json")
	}
}
