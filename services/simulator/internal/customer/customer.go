// Package customer simulates the customers of one ward. Orders arrive as a Poisson process
// that follows the scenario's demand curve and weather. Each one is placed through the public
// merchant API: a quote, then an order with a fresh Idempotency-Key.
//
// The load is open-loop: an order is placed when it is due, whether or not earlier ones have
// finished. A closed loop (wait for one answer before sending the next) would slow down
// exactly when the service slows down, hiding the latency the SLOs are meant to catch.
package customer

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"math"
	mathrand "math/rand/v2"
	"sync"
	"time"

	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/clients"
	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/clock"
	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/demand"
	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/geo"
	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/metrics"
)

// Orders is the part of the merchant API customers use.
type Orders interface {
	Quote(ctx context.Context, apiKey, storeID string, d clients.Dropoff) (clients.Quote, error)
	CreateOrder(ctx context.Context, apiKey, quoteID, idempotencyKey string) (clients.Order, error)
}

// Kitchen is told when each new order's food will be ready, so couriers know how long to wait.
type Kitchen interface {
	Prepare(orderID string, readyAt time.Time)
}

// Viewers opens the tracking page for an order, in the background. *viewer.Pool implements it.
type Viewers interface {
	Watch(orderID, trackingURL string)
}

// Store is a store customers can order from, with its merchant's API key.
type Store struct {
	ID     string
	APIKey string
}

// Config describes one ward's customers.
type Config struct {
	WardID   string
	WardName string
	Centre   geo.Point
	RadiusM  float64  // drop-offs fall within this distance of the centre
	BBox     geo.BBox // and inside this box (the orders API rejects anything outside it)
	Share    float64  // this ward's fraction of the scenario's demand
	Stores   []Store
	Curve    demand.Curve
	Weather  demand.Weather
	Clock    clock.Clock
	PrepMin  time.Duration
	PrepMax  time.Duration
	// WatchShare is the fraction of customers who open the tracking page and keep it open
	// until delivery (0 = nobody watches).
	WatchShare float64
}

// Deps are shared by every ward.
type Deps struct {
	Orders  Orders
	Kitchen Kitchen
	Viewers Viewers // nil: nobody watches
	Metrics *metrics.Metrics
	Logger  *slog.Logger
	// InFlight is a semaphore shared by all wards: its capacity is the most orders that may be
	// in progress at once. When it is full, new orders fail with reason "backpressure".
	InFlight chan struct{}
}

// Agent generates one ward's orders.
type Agent struct {
	cfg  Config
	deps Deps
	rng  *mathrand.Rand
	log  *slog.Logger
	seq  int // orders drawn so far, for synthetic customer names
}

// New creates the customers of one ward. rng is the ward's own seeded random source.
func New(cfg Config, deps Deps, rng *mathrand.Rand) *Agent {
	logger := deps.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Agent{cfg: cfg, deps: deps, rng: rng, log: logger.With("ward", cfg.WardID)}
}

// Rate returns the ward's expected orders per hour at simulated time t: the scenario's curve,
// times the weather, times the ward's share.
func (a *Agent) Rate(t time.Duration) float64 {
	return a.cfg.Curve.Rate(t) * a.cfg.Weather.DemandMultiplier(t) * a.cfg.Share
}

// order is everything random about one order, drawn before it is placed.
type order struct {
	store   Store
	dropoff clients.Dropoff
	prep    time.Duration
	watch   bool // the customer opens the tracking page
}

// Run places orders until ctx ends, then waits for the orders already in flight to finish
// (they get their own timeouts, so a shutdown does not cut them off half way). With no demand
// at all it returns straight away.
func (a *Agent) Run(ctx context.Context) {
	var inFlight sync.WaitGroup
	defer inFlight.Wait()

	ceiling := a.cfg.Curve.Max() * a.cfg.Weather.MaxDemandMultiplier() * a.cfg.Share
	arrivals := demand.NewArrivals(a.rng, a.cfg.Clock.SimTime(time.Now()), ceiling, a.Rate)
	for {
		at, ok := arrivals.Next()
		if !ok || !a.waitUntil(ctx, a.cfg.Clock.RealTime(at)) {
			return
		}
		// Draw everything random now, in arrival order, so the same seed gives the same orders
		// however long the HTTP calls take.
		o := a.draw()
		a.deps.Metrics.OrdersAttempted.WithLabelValues(a.cfg.WardID).Inc()
		select {
		case a.deps.InFlight <- struct{}{}:
		default:
			a.deps.Metrics.OrdersFailed.WithLabelValues("order", "backpressure").Inc()
			continue
		}
		inFlight.Go(func() {
			defer func() { <-a.deps.InFlight }()
			a.place(context.WithoutCancel(ctx), o)
		})
	}
}

// waitUntil sleeps until the real time due, waking at least every 30 s to publish the
// current demand rate. It returns false if ctx ends first.
func (a *Agent) waitUntil(ctx context.Context, due time.Time) bool {
	for {
		a.deps.Metrics.DemandRate.WithLabelValues(a.cfg.WardID).Set(a.Rate(a.cfg.Clock.SimTime(time.Now())))
		wait := time.Until(due)
		if wait <= 0 {
			return true
		}
		timer := time.NewTimer(min(wait, 30*time.Second))
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
}

// draw picks a store, a drop-off point in the ward, a customer and a preparation time.
func (a *Agent) draw() order {
	a.seq++
	point := a.cfg.BBox.Clamp(geo.RandomInDisc(a.rng, a.cfg.Centre, a.cfg.RadiusM))
	o := order{
		store: a.cfg.Stores[a.rng.IntN(len(a.cfg.Stores))],
		prep:  a.cfg.PrepMin + time.Duration(a.rng.Float64()*float64(a.cfg.PrepMax-a.cfg.PrepMin)),
		dropoff: clients.Dropoff{
			Lat: round6(point.Lat), Lng: round6(point.Lng), Ward: a.cfg.WardID,
			// Synthetic, obviously fake details. Tasuki is fictional and nothing is ever sent
			// to these addresses or numbers (SMS is stubbed).
			Address:       fmt.Sprintf("%d-%d-%d %s, Tokyo (simulated)", 1+a.rng.IntN(5), 1+a.rng.IntN(30), 1+a.rng.IntN(20), a.cfg.WardName),
			CustomerName:  fmt.Sprintf("Sim Customer %s-%06d", a.cfg.WardID, a.seq),
			CustomerPhone: fmt.Sprintf("090-0000-%04d", a.seq%10000),
		},
	}
	// Only drawn when someone may watch, so scenarios without viewers keep their random sequence.
	if a.cfg.WatchShare > 0 {
		o.watch = a.rng.Float64() < a.cfg.WatchShare
	}
	return o
}

// place gets a quote and turns it into an order.
func (a *Agent) place(ctx context.Context, o order) {
	m := a.deps.Metrics
	quote, err := a.deps.Orders.Quote(ctx, o.store.APIKey, o.store.ID, o.dropoff)
	if err != nil {
		m.OrdersFailed.WithLabelValues("quote", clients.Reason(err)).Inc()
		a.log.Warn("quote failed", "store_id", o.store.ID, "err", err)
		return
	}
	// A fresh, random key per order. It must not come from the scenario seed: a re-run with
	// the same seed would send the same keys and get the previous run's orders back.
	key := rand.Text()
	created, err := a.deps.Orders.CreateOrder(ctx, o.store.APIKey, quote.ID, key)
	if err != nil {
		m.OrdersFailed.WithLabelValues("order", clients.Reason(err)).Inc()
		a.log.Warn("order failed", "quote_id", quote.ID, "idempotency_key", key, "err", err)
		return
	}
	m.OrdersCreated.WithLabelValues(a.cfg.WardID).Inc()
	if created.Replayed {
		m.OrdersReplayed.Inc()
	}
	a.deps.Kitchen.Prepare(created.ID, time.Now().Add(a.cfg.Clock.Scale(o.prep)))
	if o.watch && a.deps.Viewers != nil {
		a.deps.Viewers.Watch(created.ID, created.TrackingURL)
	}
	a.log.Debug("order created", "order_id", created.ID, "fee_yen", created.FeeYen, "eta_seconds", quote.ETASeconds)
}

// round6 rounds a coordinate to 6 decimal places (about 10 cm), plenty for an address.
func round6(x float64) float64 { return math.Round(x*1e6) / 1e6 }
