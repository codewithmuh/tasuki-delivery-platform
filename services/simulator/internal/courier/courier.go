// Package courier simulates one courier and their phone app: working shifts, receiving offers
// from dispatch, riding to the store and then the customer along real roads, and reporting
// GPS positions to fleet-ingest.
//
// A courier is a small state machine, driven by one goroutine:
//
//	offline ──shift starts──▶ idle ──accepts offer──▶ to_store ──arrives──▶ at_store
//	   ▲                       ▲                                              │ food ready, pickup
//	   └──shift ends (idle)────┴──────────delivered────── to_dropoff ◀────────┘
//
// A cancelled order, or one the orders service says is no longer ours, sends the courier
// back to idle from any state.
package courier

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/clients"
	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/clock"
	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/demand"
	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/geo"
	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/metrics"
)

// State is what the courier is doing. The values are the statuses in the location contract.
type State string

// The courier states.
const (
	Offline   State = "offline"
	Idle      State = "idle"
	ToStore   State = "to_store"
	AtStore   State = "at_store"
	ToDropoff State = "to_dropoff"
)

// Orders is the part of the orders API a courier uses.
type Orders interface {
	Pickup(ctx context.Context, token, orderID string) error
	Deliver(ctx context.Context, token, orderID string, at geo.Point) error
}

// Fleet is fleet-ingest, where GPS points go.
type Fleet interface {
	SendLocations(ctx context.Context, token string, points []clients.Location) (clients.LocationsResult, error)
}

// Dispatch sends offers and takes answers.
type Dispatch interface {
	FollowOffers(ctx context.Context, token func() string, out chan<- clients.OfferEvent) error
	Accept(ctx context.Context, token, offerID string) error
	Decline(ctx context.Context, token, offerID string) error
}

// Router plans a road route. It may fail; the courier then rides in a straight line.
type Router interface {
	Route(ctx context.Context, from, to geo.Point) ([]geo.Point, error)
}

// Kitchen knows when each order's food will be ready. The simulator plays the stores too, so
// it decides this when it places the order.
type Kitchen interface {
	ReadyAt(orderID string) (time.Time, bool)
}

// Config is what makes one courier different from another.
type Config struct {
	ID       string
	Token    func() string // the courier's current JWT (tokens are re-issued before they expire)
	SpeedKmh float64       // riding speed in dry weather
	Home     geo.Point     // where the first shift starts
	// ShiftStart and ShiftEnd are times of day. The shift repeats every simulated day.
	ShiftStart, ShiftEnd time.Duration
}

// Settings are shared by every courier in a scenario.
type Settings struct {
	Clock               clock.Clock
	Weather             demand.Weather
	MovingInterval      time.Duration // GPS interval with an order
	IdleInterval        time.Duration // GPS interval without one
	NoiseM              float64       // GPS error, standard deviation in metres
	AcceptProbability   float64       // chance of accepting an offer for a store next door
	HalvingDistanceM    float64       // every this many metres to the store halves that chance
	ThinkMin, ThinkMax  time.Duration // how long the courier looks at an offer
	DefaultPrep         time.Duration // food preparation time when the kitchen does not know the order
	MaxActionFailures   int           // failed pickup/deliver attempts before giving up on a job
	OfflineReportBudget time.Duration // time allowed for the last "offline" GPS report
}

// Deps are the services a courier talks to.
type Deps struct {
	Orders   Orders
	Fleet    Fleet
	Dispatch Dispatch
	Router   Router // nil means "no OSRM": always ride in a straight line
	Kitchen  Kitchen
	Metrics  *metrics.Metrics
	Logger   *slog.Logger
}

// Courier is one simulated courier. Create it with New and call Run once.
type Courier struct {
	cfg  Config
	set  Settings
	deps Deps
	log  *slog.Logger
	// Two random sources: decisions (accept or not, how long to think) and GPS noise. Keeping
	// them apart means the number of GPS readings, which depends on timing, never changes the
	// courier's decisions.
	decide *rand.Rand
	gps    *rand.Rand

	state        State
	pos          geo.Point // true position; the phone reports it with noise
	heading      float64
	lastMove     time.Time
	job          *job
	buffer       []clients.Location // points fleet-ingest has not accepted yet
	pauseUntil   time.Time          // fleet-ingest asked us to stay away until then
	unauthorized bool               // a service rejected our token: stop working
}

// job is an accepted order.
type job struct {
	offerID, orderID string
	store, dropoff   geo.Point
	route            *geo.Route // the current leg: to the store, then to the drop-off
	travelled        float64    // metres along the current leg
	readyAt          time.Time  // when the food is ready for pickup
	failures         int        // failed pickup or deliver attempts
}

// New creates a courier. decide and gps are its own seeded random sources.
func New(cfg Config, set Settings, deps Deps, decide, gps *rand.Rand) *Courier {
	if set.MaxActionFailures <= 0 {
		set.MaxActionFailures = 5
	}
	if set.OfflineReportBudget <= 0 {
		set.OfflineReportBudget = 2 * time.Second
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Courier{
		cfg: cfg, set: set, deps: deps, decide: decide, gps: gps,
		log: logger.With("courier_id", cfg.ID), state: Offline, pos: cfg.Home,
	}
}

// AcceptProbability is the chance of accepting an offer whose store is distanceM metres away:
// base for a store next door, halving every halvingM metres. Couriers are paid per delivery,
// so a long unpaid ride to the store is a reason to say no.
func AcceptProbability(base, halvingM, distanceM float64) float64 {
	return base * math.Pow(0.5, distanceM/halvingM)
}

// Run plays the courier until ctx ends. When drain closes (the simulator is shutting down),
// the courier stops taking offers, finishes the order in hand and logs off.
func (c *Courier) Run(ctx context.Context, drain <-chan struct{}) {
	for {
		if !c.waitForShift(ctx, drain) {
			return
		}
		if keepGoing := c.work(ctx, drain); !keepGoing {
			return
		}
	}
}

// waitForShift sleeps until the courier's shift has started. It returns false if ctx ends or
// the simulator starts draining first.
func (c *Courier) waitForShift(ctx context.Context, drain <-chan struct{}) bool {
	now := time.Now()
	if c.onShift(now) {
		return true
	}
	sim := c.set.Clock.SimTime(now)
	start := sim - c.set.Clock.TimeOfDay(now) + c.cfg.ShiftStart // today's shift start
	if start <= sim {
		start += clock.Day // today's shift is over: wait for tomorrow's
	}
	timer := time.NewTimer(c.set.Clock.RealTime(start).Sub(now))
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	case <-drain:
		return false
	}
}

// onShift reports whether real time t falls inside the courier's shift.
func (c *Courier) onShift(t time.Time) bool {
	tod := c.set.Clock.TimeOfDay(t)
	return tod >= c.cfg.ShiftStart && tod < c.cfg.ShiftEnd
}

// work runs one shift: online, taking offers and reporting GPS until the shift is over and
// the courier has no order. It returns false when the courier should stop for good.
func (c *Courier) work(ctx context.Context, drain <-chan struct{}) (keepGoing bool) {
	shiftCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	offers := make(chan clients.OfferEvent)
	streamDone := make(chan error, 1)
	go func() { streamDone <- c.deps.Dispatch.FollowOffers(shiftCtx, c.cfg.Token, offers) }()

	c.lastMove = time.Now()
	c.setState(Idle)
	c.deps.Metrics.CouriersActive.Inc()
	c.log.Debug("shift started")
	defer func() {
		c.goOffline(ctx)
		c.deps.Metrics.CouriersActive.Dec()
		c.log.Debug("shift ended")
	}()

	draining := false
	timer := time.NewTimer(0) // report the starting position straight away
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-drain:
			draining, drain = true, nil // a nil channel is never ready, so this fires once
		case err := <-streamDone:
			if errors.Is(err, clients.ErrUnauthorized) {
				c.log.Warn("dispatch rejected the courier token; logging off")
			}
			return false
		case ev := <-offers:
			before := c.state
			c.handleOffer(shiftCtx, ev, draining || !c.onShift(time.Now()))
			if c.state != before {
				timer.Reset(c.interval()) // switch to the faster GPS interval at once
			}
		case now := <-timer.C:
			c.tick(shiftCtx, now)
			timer.Reset(c.interval())
		}
		if c.unauthorized {
			return false
		}
		if c.job == nil && (draining || !c.onShift(time.Now())) {
			return !draining
		}
	}
}

// interval is how often the phone reports its position in the current state.
func (c *Courier) interval() time.Duration {
	if c.job != nil {
		return c.set.MovingInterval
	}
	return c.set.IdleInterval
}

// tick moves the courier and reports a GPS point.
func (c *Courier) tick(ctx context.Context, now time.Time) {
	c.advance(ctx, now)
	c.report(ctx, now, c.state)
}

// advance moves the courier along the current leg for the real time since the last move,
// and handles arriving at the store or the drop-off.
func (c *Courier) advance(ctx context.Context, now time.Time) {
	elapsed := now.Sub(c.lastMove)
	c.lastMove = now
	switch c.state {
	case ToStore, ToDropoff:
		c.job.travelled += c.speedMPS(now) * c.set.Clock.Speedup() * elapsed.Seconds()
		c.pos, c.heading = c.job.route.At(c.job.travelled)
		if c.job.travelled < c.job.route.Length() {
			return
		}
		if c.state == ToStore {
			c.setState(AtStore)
			c.tryPickup(ctx, now) // the food may be ready already
		} else {
			c.tryDeliver(ctx)
		}
	case AtStore:
		c.tryPickup(ctx, now)
	}
}

// speedMPS is the courier's riding speed in metres per second, slowed by the weather.
func (c *Courier) speedMPS(now time.Time) float64 {
	return c.cfg.SpeedKmh / 3.6 * c.set.Weather.SpeedMultiplier(c.set.Clock.SimTime(now))
}

// handleOffer answers an offer, or drops the current job if its offer was cancelled.
// unavailable means the courier is not taking new work (shift over or shutting down).
func (c *Courier) handleOffer(ctx context.Context, ev clients.OfferEvent, unavailable bool) {
	if ev.Cancelled != nil {
		if c.job != nil && c.job.offerID == ev.Cancelled.OfferID {
			c.deps.Metrics.Offers.WithLabelValues("cancelled").Inc()
			c.abandon("offer_cancelled")
		}
		return
	}
	offer := ev.Offer
	c.deps.Metrics.Offers.WithLabelValues("received").Inc()
	if c.state != Idle || unavailable {
		c.decline(ctx, offer.ID)
		return
	}

	store := geo.Point{Lat: offer.Store.Lat, Lng: offer.Store.Lng}
	p := AcceptProbability(c.set.AcceptProbability, c.set.HalvingDistanceM, geo.Distance(c.pos, store))
	accept := c.decide.Float64() < p
	think := c.set.ThinkMin + time.Duration(c.decide.Float64()*float64(c.set.ThinkMax-c.set.ThinkMin))
	if !sleep(ctx, think) {
		return
	}
	if !accept {
		c.decline(ctx, offer.ID)
		return
	}
	if err := c.deps.Dispatch.Accept(ctx, c.cfg.Token(), offer.ID); err != nil {
		outcome := "failed"
		if clients.StatusOf(err) == http.StatusConflict {
			outcome = "lost" // expired while we thought about it, or someone else got it
		}
		c.deps.Metrics.Offers.WithLabelValues(outcome).Inc()
		c.checkAuth(err)
		c.log.Debug("accept failed", "offer_id", offer.ID, "err", err)
		return
	}
	c.deps.Metrics.Offers.WithLabelValues("accepted").Inc()

	now := time.Now()
	readyAt, known := c.deps.Kitchen.ReadyAt(offer.OrderID)
	if !known {
		readyAt = now.Add(c.set.Clock.Scale(c.set.DefaultPrep))
	}
	c.job = &job{
		offerID: offer.ID, orderID: offer.OrderID, store: store, dropoff: offer.Dropoff,
		route: c.plan(ctx, c.pos, store), readyAt: readyAt,
	}
	c.lastMove = now
	c.setState(ToStore)
	c.log.Debug("offer accepted", "offer_id", offer.ID, "order_id", offer.OrderID)
}

func (c *Courier) decline(ctx context.Context, offerID string) {
	c.deps.Metrics.Offers.WithLabelValues("declined").Inc()
	if err := c.deps.Dispatch.Decline(ctx, c.cfg.Token(), offerID); err != nil {
		c.checkAuth(err)
		c.log.Debug("decline failed", "offer_id", offerID, "err", err) // the offer simply expires
	}
}

// tryPickup collects the order once the food is ready.
func (c *Courier) tryPickup(ctx context.Context, now time.Time) {
	if now.Before(c.job.readyAt) {
		return // still waiting at the counter
	}
	err := c.deps.Orders.Pickup(ctx, c.cfg.Token(), c.job.orderID)
	if err != nil {
		c.actionFailed("pickup", err)
		return
	}
	c.job.route = c.plan(ctx, c.pos, c.job.dropoff)
	c.job.travelled = 0
	c.job.failures = 0
	c.setState(ToDropoff)
}

// tryDeliver hands the order over, with the phone's GPS reading as proof of delivery.
func (c *Courier) tryDeliver(ctx context.Context) {
	proof := c.reading()
	if err := c.deps.Orders.Deliver(ctx, c.cfg.Token(), c.job.orderID, proof); err != nil {
		c.actionFailed("deliver", err)
		return
	}
	c.deps.Metrics.Deliveries.Inc()
	c.log.Debug("delivered", "order_id", c.job.orderID)
	c.job = nil
	c.setState(Idle)
}

// actionFailed decides what a failed pickup or delivery means. 404 and 409 mean the order is
// no longer ours (cancelled, or reassigned); 422 means the orders service rejected our proof.
// Anything else may be temporary, so the courier tries again on the next tick, up to a limit.
func (c *Courier) actionFailed(action string, err error) {
	c.checkAuth(err)
	switch clients.StatusOf(err) {
	case http.StatusNotFound, http.StatusConflict:
		c.abandon("order_gone")
	case http.StatusUnprocessableEntity:
		c.log.Warn(action+" rejected", "order_id", c.job.orderID, "code", clients.CodeOf(err))
		c.abandon(action + "_rejected")
	default:
		c.job.failures++
		if c.job.failures >= c.set.MaxActionFailures {
			c.log.Warn(action+" keeps failing; giving up the order", "order_id", c.job.orderID, "err", err)
			c.abandon(action + "_failed")
		}
	}
}

// abandon drops the current job and goes back to waiting for offers.
func (c *Courier) abandon(reason string) {
	c.deps.Metrics.JobsAbandoned.WithLabelValues(reason).Inc()
	c.log.Debug("job abandoned", "order_id", c.job.orderID, "reason", reason)
	c.job = nil
	c.setState(Idle)
}

// plan returns a route from one point to another: OSRM's road route when it is available,
// otherwise a straight line. The exact start and end points are added, because OSRM snaps
// both to the nearest road and the courier must reach the actual door.
func (c *Courier) plan(ctx context.Context, from, to geo.Point) *geo.Route {
	path := []geo.Point{from}
	source := "straight_line"
	if c.deps.Router != nil {
		road, err := c.deps.Router.Route(ctx, from, to)
		if err == nil {
			path = append(path, road...)
			source = "osrm"
		} else {
			c.log.Debug("routing unavailable; riding in a straight line", "err", err)
		}
	}
	c.deps.Metrics.Routes.WithLabelValues(source).Inc()
	route, _ := geo.NewRoute(append(path, to)) // never empty, so never an error
	return route
}

// reading is what the phone's GPS says: the true position plus noise.
func (c *Courier) reading() geo.Point {
	if c.set.NoiseM <= 0 {
		return c.pos
	}
	return geo.Noisy(c.gps, c.pos, c.set.NoiseM)
}

// report adds a GPS point to the buffer and posts the buffer to fleet-ingest.
//
// The location path is lossy by design (docs/design.md §5): if fleet-ingest sheds load with
// 503 + Retry-After, the batch is dropped and the phone stays quiet until Retry-After has
// passed. On network errors the points are kept and sent with the next batch, up to
// clients.MaxBatch; the oldest are dropped first, since only recent positions matter.
func (c *Courier) report(ctx context.Context, now time.Time, status State) {
	point := c.reading()
	loc := clients.Location{
		Lat: point.Lat, Lng: point.Lng,
		AccuracyM:  math.Max(3, 2*c.set.NoiseM), // a phone reports roughly a 95 % radius
		HeadingDeg: c.heading,
		DeviceTime: now.UTC().Truncate(time.Millisecond),
		Status:     string(status),
	}
	if status == ToStore || status == ToDropoff {
		loc.SpeedMPS = c.speedMPS(now)
	}
	if c.job != nil && status != Idle && status != Offline {
		loc.OrderID = c.job.orderID
	}
	c.buffer = append(c.buffer, loc)
	if extra := len(c.buffer) - clients.MaxBatch; extra > 0 {
		c.deps.Metrics.LocationPoints.WithLabelValues("lost").Add(float64(extra))
		c.buffer = c.buffer[extra:]
	}
	if now.Before(c.pauseUntil) {
		return
	}

	res, err := c.deps.Fleet.SendLocations(ctx, c.cfg.Token(), c.buffer)
	if err == nil {
		c.deps.Metrics.LocationBatches.WithLabelValues("sent").Inc()
		c.deps.Metrics.LocationPoints.WithLabelValues("accepted").Add(float64(res.Accepted))
		c.deps.Metrics.LocationPoints.WithLabelValues("dropped").Add(float64(res.Dropped))
		c.buffer = c.buffer[:0]
		return
	}
	if wait, shed := clients.IsLoadShed(err); shed {
		if wait <= 0 {
			wait = 5 * time.Second // fleet-ingest's default Retry-After
		}
		c.deps.Metrics.LocationBatches.WithLabelValues("shed").Inc()
		c.deps.Metrics.LocationPoints.WithLabelValues("lost").Add(float64(len(c.buffer)))
		c.buffer = c.buffer[:0]
		c.pauseUntil = now.Add(wait)
		return
	}
	c.checkAuth(err)
	if status := clients.StatusOf(err); status >= 400 && status < 500 {
		// The batch itself is wrong (400, 413, 422): sending it again would fail again.
		c.deps.Metrics.LocationBatches.WithLabelValues("rejected").Inc()
		c.deps.Metrics.LocationPoints.WithLabelValues("lost").Add(float64(len(c.buffer)))
		c.buffer = c.buffer[:0]
		c.log.Warn("fleet-ingest rejected a GPS batch", "status", status, "code", clients.CodeOf(err))
		return
	}
	c.deps.Metrics.LocationBatches.WithLabelValues("failed").Inc() // keep the points for next time
}

// goOffline sends a last "offline" point so the fleet stops counting this courier at once,
// instead of waiting for its position to go stale.
func (c *Courier) goOffline(ctx context.Context) {
	if !c.unauthorized {
		// The shift's context may already be cancelled; the last report gets a short budget of its own.
		reportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.set.OfflineReportBudget)
		c.pauseUntil = time.Time{}
		c.report(reportCtx, time.Now(), Offline)
		cancel()
	}
	c.buffer = nil
	c.setState(Offline)
}

// checkAuth notices a rejected token, which ends the courier's shift.
func (c *Courier) checkAuth(err error) {
	if clients.StatusOf(err) == http.StatusUnauthorized && !c.unauthorized {
		c.unauthorized = true
		c.log.Warn("courier token rejected; logging off")
	}
}

// setState changes state and keeps the per-state gauge right. Offline couriers are not counted.
func (c *Courier) setState(s State) {
	from, to := string(c.state), string(s)
	if c.state == Offline {
		from = ""
	}
	if s == Offline {
		to = ""
	}
	c.deps.Metrics.MoveCourier(from, to)
	c.state = s
}

// sleep waits for d unless ctx ends first; it reports whether the full wait happened.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
