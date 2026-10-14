package sim

import (
	"context"
	"crypto/rand"
	"fmt"
	"math"
	mathrand "math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/clients"
	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/geo"
	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/rng"
	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/scenario"
)

// storeRadiusM keeps stores close to the ward centre, where the shops are.
const storeRadiusM = 800

// seedWorkers is how many couriers are created in parallel. Thousands of couriers one at a
// time would take minutes; a small pool keeps the load on the orders service reasonable.
const seedWorkers = 16

// Seeder is the part of the orders admin API that seeding uses.
type Seeder interface {
	CreateMerchant(ctx context.Context, name string, stores []clients.NewStore) (clients.Merchant, error)
	CreateCourier(ctx context.Context, name, vehicle string) (clients.Courier, error)
}

// Seed creates the scenario's merchants, stores and couriers through the orders admin API.
// Positions, vehicles and the store-to-merchant split come from the scenario seed; the IDs,
// keys and tokens come from the orders service.
func Seed(ctx context.Context, sc *scenario.Scenario, orders Seeder) (*State, error) {
	// A short random tag keeps names unique if the same scenario is seeded twice against one
	// database (for example after deleting the state file).
	tag := strings.ToLower(rand.Text()[:6])
	st := &State{CreatedAt: time.Now().UTC()}

	for m, stores := range planStores(sc) {
		name := fmt.Sprintf("Sim Kitchen %02d (%s)", m+1, tag)
		merchant, err := orders.CreateMerchant(ctx, name, stores)
		if err != nil {
			return nil, fmt.Errorf("create merchant %q: %w", name, err)
		}
		if len(merchant.Stores) != len(stores) {
			return nil, fmt.Errorf("merchant %q: asked for %d stores, got %d", name, len(stores), len(merchant.Stores))
		}
		st.Merchants = append(st.Merchants, SeededMerchant{ID: merchant.ID, Name: name, APIKey: merchant.APIKey, Stores: merchant.Stores})
	}

	couriers, err := seedCouriers(ctx, sc, orders, tag)
	if err != nil {
		return nil, err
	}
	st.Couriers = couriers
	return st, nil
}

// planStores places stores_per_ward stores near each ward centre and deals them out to the
// merchants in turn, so every merchant has stores and most have them in several wards.
func planStores(sc *scenario.Scenario) [][]clients.NewStore {
	perMerchant := make([][]clients.NewStore, sc.Merchants)
	next := 0
	for _, wardID := range sc.Wards {
		ward, _ := scenario.LookupWard(wardID) // the scenario was validated
		r := rng.New(sc.Seed, "stores/"+wardID)
		for j := range sc.StoresPerWard {
			p := geo.RandomInDisc(r, ward.Centre(), math.Min(storeRadiusM, sc.WardRadiusM))
			m := next % sc.Merchants
			next++
			perMerchant[m] = append(perMerchant[m], clients.NewStore{
				Name: fmt.Sprintf("%s #%d", ward.NameEN, j+1), Lat: round6(p.Lat), Lng: round6(p.Lng), Ward: wardID,
			})
		}
	}
	return perMerchant
}

// seedCouriers creates the couriers with a small worker pool and returns them in order.
func seedCouriers(ctx context.Context, sc *scenario.Scenario, orders Seeder, tag string) ([]SeededCourier, error) {
	couriers := make([]SeededCourier, sc.Couriers.Count)
	errs := make([]error, sc.Couriers.Count)
	indexes := make(chan int)
	var wg sync.WaitGroup
	for range seedWorkers {
		wg.Go(func() {
			for i := range indexes {
				vehicle := planCourier(sc, i).Vehicle
				c, err := orders.CreateCourier(ctx, fmt.Sprintf("Sim Courier %04d (%s)", i+1, tag), vehicle)
				couriers[i] = SeededCourier{Index: i, ID: c.ID, Token: c.Token, Vehicle: vehicle}
				errs[i] = err
			}
		})
	}
	for i := range sc.Couriers.Count {
		indexes <- i
	}
	close(indexes)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			return nil, fmt.Errorf("create courier %d: %w", i+1, err)
		}
	}
	return couriers, nil
}

// courierPlan is everything about a courier that comes from the scenario rather than from
// the orders service.
type courierPlan struct {
	Vehicle              string
	Ward                 string
	Home                 geo.Point
	ShiftStart, ShiftEnd time.Duration
}

// planCourier decides courier i's vehicle, home ward, starting point and shift. It is a pure
// function of the scenario and i, so seeding and every later run agree.
func planCourier(sc *scenario.Scenario, i int) courierPlan {
	r := rng.New(sc.Seed, fmt.Sprintf("courier/%d/setup", i))
	p := courierPlan{Vehicle: "bicycle"}
	if r.Float64() < sc.Couriers.ScooterShare {
		p.Vehicle = "scooter"
	}

	// Couriers live where the demand is: home wards follow the ward weights.
	weights := make([]float64, len(sc.Wards))
	for w, id := range sc.Wards {
		weights[w] = sc.WardShare(id)
	}
	p.Ward = sc.Wards[pickWeighted(r, weights)]
	ward, _ := scenario.LookupWard(p.Ward)
	p.Home = scenario.CityBBox().Clamp(geo.RandomInDisc(r, ward.Centre(), sc.WardRadiusM))

	shares := make([]float64, len(sc.Couriers.Shifts))
	for s, sh := range sc.Couriers.Shifts {
		shares[s] = sh.Share
	}
	shift := sc.Couriers.Shifts[pickWeighted(r, shares)]
	p.ShiftStart, p.ShiftEnd = shift.Start.Duration(), shift.End.Duration()

	// Some couriers log off when bad weather starts during their shift.
	for _, w := range sc.Weather {
		from := w.From.Duration()
		if w.OfflineShare > 0 && r.Float64() < w.OfflineShare && from > p.ShiftStart && from < p.ShiftEnd {
			p.ShiftEnd = from
		}
	}
	return p
}

// pickWeighted returns index i with probability weights[i] / sum(weights).
func pickWeighted(r *mathrand.Rand, weights []float64) int {
	total := 0.0
	for _, w := range weights {
		total += w
	}
	x := r.Float64() * total
	for i, w := range weights {
		if x < w {
			return i
		}
		x -= w
	}
	return len(weights) - 1 // only reached through floating-point rounding
}

func round6(x float64) float64 { return math.Round(x*1e6) / 1e6 }
