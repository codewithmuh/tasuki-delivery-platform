package courier

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/clients"
	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/clock"
	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/demand"
	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/geo"
	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/metrics"
)

// The tests run inside synctest bubbles: time is fake and jumps forward whenever every
// goroutine is waiting, so a 3-minute delivery takes microseconds and timings are exact.

var (
	home    = geo.Point{Lat: 35.6640, Lng: 139.6982} // Shibuya ward centre
	store   = geo.Offset(home, 0, 400)               // 400 m east
	dropoff = geo.Offset(store, 800, 0)              // then 800 m north
)

// ---- fakes ----

type fakeOrders struct {
	mu          sync.Mutex
	pickups     []string
	delivers    []string
	deliveredAt []geo.Point
	pickupErr   func(n int) error // n counts calls from 1
	deliverErr  func(n int) error
}

func (f *fakeOrders) Pickup(_ context.Context, _, orderID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pickups = append(f.pickups, orderID)
	if f.pickupErr != nil {
		return f.pickupErr(len(f.pickups))
	}
	return nil
}

func (f *fakeOrders) Deliver(_ context.Context, _, orderID string, at geo.Point) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delivers = append(f.delivers, orderID)
	f.deliveredAt = append(f.deliveredAt, at)
	if f.deliverErr != nil {
		return f.deliverErr(len(f.delivers))
	}
	return nil
}

type fakeFleet struct {
	mu      sync.Mutex
	batches [][]clients.Location
	respond func(n int) error // n counts calls from 1; nil error means 202
}

func (f *fakeFleet) SendLocations(_ context.Context, _ string, points []clients.Location) (clients.LocationsResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batches = append(f.batches, append([]clients.Location(nil), points...))
	if f.respond != nil {
		if err := f.respond(len(f.batches)); err != nil {
			return clients.LocationsResult{}, err
		}
	}
	return clients.LocationsResult{Accepted: len(points)}, nil
}

// accepted returns every point fleet-ingest accepted, in order.
func (f *fakeFleet) points() []clients.Location {
	f.mu.Lock()
	defer f.mu.Unlock()
	var all []clients.Location
	for _, b := range f.batches {
		all = append(all, b...)
	}
	return all
}

type fakeDispatch struct {
	feed      chan clients.OfferEvent
	followErr error // returned straight away when set
	mu        sync.Mutex
	accepted  []string
	declined  []string
	acceptErr error
}

func (f *fakeDispatch) FollowOffers(ctx context.Context, _ func() string, out chan<- clients.OfferEvent) error {
	if f.followErr != nil {
		return f.followErr
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev := <-f.feed:
			select {
			case out <- ev:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

func (f *fakeDispatch) Accept(_ context.Context, _, offerID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accepted = append(f.accepted, offerID)
	return f.acceptErr
}

func (f *fakeDispatch) Decline(_ context.Context, _, offerID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.declined = append(f.declined, offerID)
	return nil
}

type fakeKitchen map[string]time.Time

func (k fakeKitchen) ReadyAt(orderID string) (time.Time, bool) {
	at, ok := k[orderID]
	return at, ok
}

type fakeRouter struct {
	path []geo.Point
	err  error
}

func (r fakeRouter) Route(context.Context, geo.Point, geo.Point) ([]geo.Point, error) {
	return r.path, r.err
}

// ---- harness ----

type harness struct {
	orders   *fakeOrders
	fleet    *fakeFleet
	dispatch *fakeDispatch
	kitchen  fakeKitchen
	router   Router
	metrics  *metrics.Metrics
	cfg      Config
	set      Settings
	drain    chan struct{}
}

// newHarness must be called inside the bubble, so its channels belong to the bubble.
func newHarness() *harness {
	start := time.Now()
	return &harness{
		orders:   &fakeOrders{},
		fleet:    &fakeFleet{},
		dispatch: &fakeDispatch{feed: make(chan clients.OfferEvent)},
		kitchen:  fakeKitchen{"ord_1": start.Add(60 * time.Second)},
		metrics:  metrics.New(prometheus.NewRegistry()),
		cfg: Config{
			ID: "cou_1", Token: func() string { return "jwt" }, SpeedKmh: 36, Home: home,
			ShiftStart: 0, ShiftEnd: clock.Day,
		},
		set: Settings{
			Clock:          clock.New(start, 12*time.Hour, 1),
			MovingInterval: 4 * time.Second, IdleInterval: 15 * time.Second,
			AcceptProbability: 1, HalvingDistanceM: 1e12, // always accept
			ThinkMin: 2 * time.Second, ThinkMax: 2 * time.Second,
			DefaultPrep: 5 * time.Minute,
		},
		drain: make(chan struct{}),
	}
}

// run starts the courier and returns a function that stops it and waits for it to finish.
func (h *harness) run(ctx context.Context) (stop func(), done <-chan struct{}) {
	ctx, cancel := context.WithCancel(ctx)
	c := New(h.cfg, h.set, Deps{
		Orders: h.orders, Fleet: h.fleet, Dispatch: h.dispatch, Router: h.router,
		Kitchen: h.kitchen, Metrics: h.metrics,
	}, rand.New(rand.NewPCG(1, 1)), rand.New(rand.NewPCG(2, 2)))
	finished := make(chan struct{})
	go func() {
		c.Run(ctx, h.drain)
		close(finished)
	}()
	return func() { cancel(); <-finished }, finished
}

func (h *harness) offer(id, orderID string, at geo.Point) {
	h.dispatch.feed <- clients.OfferEvent{Offer: &clients.Offer{
		ID: id, OrderID: orderID, Store: clients.OfferStore{ID: "str_1", Lat: at.Lat, Lng: at.Lng}, Dropoff: dropoff,
	}}
}

func statuses(points []clients.Location) []string {
	var out []string
	for _, p := range points {
		if len(out) == 0 || out[len(out)-1] != p.Status {
			out = append(out, p.Status)
		}
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---- tests ----

func TestAcceptProbabilityFallsWithDistance(t *testing.T) {
	if got := AcceptProbability(0.9, 2000, 0); got != 0.9 {
		t.Errorf("next door: %v, want 0.9", got)
	}
	if got := AcceptProbability(0.9, 2000, 2000); math.Abs(got-0.45) > 1e-9 {
		t.Errorf("at the halving distance: %v, want 0.45", got)
	}
	if near, far := AcceptProbability(0.9, 2000, 500), AcceptProbability(0.9, 2000, 5000); near <= far {
		t.Errorf("a nearer store should be likelier: %v vs %v", near, far)
	}
}

// One full delivery: idle → offer → to_store → wait for the food → pickup → to_dropoff →
// deliver → idle, checking the GPS reports and the courier's speed along the way.
func TestCourierDeliversAnOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness()
		stop, _ := h.run(t.Context())
		start := time.Now()

		time.Sleep(5 * time.Second)
		h.offer("off_1", "ord_1", store)
		time.Sleep(5 * time.Minute)
		stop()

		if !equal(h.dispatch.accepted, []string{"off_1"}) || !equal(h.orders.pickups, []string{"ord_1"}) ||
			!equal(h.orders.delivers, []string{"ord_1"}) {
			t.Fatalf("accepted %v, picked up %v, delivered %v", h.dispatch.accepted, h.orders.pickups, h.orders.delivers)
		}
		if d := geo.Distance(h.orders.deliveredAt[0], dropoff); d > 1 {
			t.Errorf("proof of delivery is %.1f m from the drop-off", d)
		}

		points := h.fleet.points()
		want := []string{"idle", "to_store", "at_store", "to_dropoff", "idle", "offline"}
		if got := statuses(points); !equal(got, want) {
			t.Fatalf("status sequence = %v, want %v", got, want)
		}
		var lastToStore, firstToDropoff time.Time
		for i, p := range points {
			busy := p.Status == "to_store" || p.Status == "at_store" || p.Status == "to_dropoff"
			if busy != (p.OrderID == "ord_1") {
				t.Errorf("point %d (%s) has order_id %q", i, p.Status, p.OrderID)
			}
			if p.Status == "to_store" {
				lastToStore = p.DeviceTime
				if p.SpeedMPS != 10 {
					t.Errorf("to_store speed = %v m/s, want 10", p.SpeedMPS)
				}
			}
			if p.Status == "to_dropoff" && firstToDropoff.IsZero() {
				firstToDropoff = p.DeviceTime
			}
			if i == 0 {
				continue
			}
			// GPS every 4 s with an order, every 15 s without one.
			gap := p.DeviceTime.Sub(points[i-1].DeviceTime)
			if points[i-1].Status == "to_store" && p.Status == "to_store" && gap != 4*time.Second {
				t.Errorf("moving GPS gap = %v, want 4s", gap)
			}
			if points[i-1].Status == "idle" && p.Status == "idle" && gap != 15*time.Second {
				t.Errorf("idle GPS gap = %v, want 15s", gap)
			}
			// At 10 m/s, consecutive points on the straight first leg are 40 m apart.
			if points[i-1].Status == "to_store" && p.Status == "to_store" {
				if d := geo.Distance(geo.Point{Lat: points[i-1].Lat, Lng: points[i-1].Lng}, geo.Point{Lat: p.Lat, Lng: p.Lng}); math.Abs(d-40) > 0.5 {
					t.Errorf("moved %.2f m in 4 s, want 40 m", d)
				}
			}
		}
		// 400 m at 10 m/s from accepting at 7 s (5 s + 2 s thinking): there at about 47 s.
		if arrived := lastToStore.Sub(start); arrived < 40*time.Second || arrived > 48*time.Second {
			t.Errorf("last to_store point at %v, want just before 47 s", arrived)
		}
		// The food is ready at 60 s, so pickup happens on the first tick after it.
		if left := firstToDropoff.Sub(start); left < 60*time.Second || left > 68*time.Second {
			t.Errorf("first to_dropoff point at %v, want soon after 60 s", left)
		}

		m := h.metrics
		if testutil.ToFloat64(m.Deliveries) != 1 || testutil.ToFloat64(m.Offers.WithLabelValues("accepted")) != 1 ||
			testutil.ToFloat64(m.Routes.WithLabelValues("straight_line")) != 2 || testutil.ToFloat64(m.CouriersActive) != 0 {
			t.Error("metrics do not match one delivery with two straight-line legs")
		}
		if got := testutil.ToFloat64(m.CourierStates.WithLabelValues("idle")); got != 0 {
			t.Errorf("idle gauge after logging off = %v, want 0", got)
		}
	})
}

func TestCourierUsesTheRouterAndWeather(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness()
		h.router = fakeRouter{path: []geo.Point{geo.Offset(home, 0, 200), geo.Offset(home, 0, 300)}}
		h.set.Weather = demand.Weather{{From: 0, To: clock.Day, DemandMultiplier: 1, SpeedMultiplier: 0.5}}
		stop, _ := h.run(t.Context())
		h.offer("off_1", "ord_1", store)
		time.Sleep(20 * time.Second)
		stop()

		if got := testutil.ToFloat64(h.metrics.Routes.WithLabelValues("osrm")); got != 1 {
			t.Errorf("osrm routes = %v, want 1", got)
		}
		for _, p := range h.fleet.points() {
			if p.Status == "to_store" && p.SpeedMPS != 5 {
				t.Errorf("speed in the rain = %v, want 5 m/s (half of 10)", p.SpeedMPS)
			}
		}
	})
}

func TestRouterFailureFallsBackToAStraightLine(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness()
		h.router = fakeRouter{err: errors.New("osrm down")}
		stop, _ := h.run(t.Context())
		h.offer("off_1", "ord_1", store)
		time.Sleep(10 * time.Second)
		stop()
		if got := testutil.ToFloat64(h.metrics.Routes.WithLabelValues("straight_line")); got != 1 {
			t.Errorf("straight_line routes = %v, want 1", got)
		}
	})
}

func TestCourierDeclinesAFarAwayStore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness()
		h.set.AcceptProbability, h.set.HalvingDistanceM = 0.95, 1 // halves every metre: 400 m is hopeless
		stop, _ := h.run(t.Context())
		h.offer("off_far", "ord_far", store)
		time.Sleep(5 * time.Second)
		stop()
		if !equal(h.dispatch.declined, []string{"off_far"}) || len(h.dispatch.accepted) != 0 {
			t.Fatalf("declined %v, accepted %v", h.dispatch.declined, h.dispatch.accepted)
		}
	})
}

func TestBusyCourierDeclinesNewOffers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness()
		stop, _ := h.run(t.Context())
		h.offer("off_1", "ord_1", store)
		time.Sleep(5 * time.Second)
		h.offer("off_2", "ord_2", store) // already on the way to a store
		time.Sleep(time.Second)
		stop()

		if !equal(h.dispatch.declined, []string{"off_2"}) || !equal(h.dispatch.accepted, []string{"off_1"}) {
			t.Fatalf("declined %v, accepted %v", h.dispatch.declined, h.dispatch.accepted)
		}
		if got := testutil.ToFloat64(h.metrics.Offers.WithLabelValues("received")); got != 2 {
			t.Errorf("received = %v, want 2", got)
		}
	})
}

func TestLostOfferLeavesTheCourierIdle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness()
		h.dispatch.acceptErr = &clients.APIError{Status: 409, Code: "offer_expired"}
		stop, _ := h.run(t.Context())
		h.offer("off_1", "ord_1", store)
		time.Sleep(time.Minute)
		stop()
		if got := testutil.ToFloat64(h.metrics.Offers.WithLabelValues("lost")); got != 1 {
			t.Errorf("lost = %v, want 1", got)
		}
		if got := statuses(h.fleet.points()); !equal(got, []string{"idle", "offline"}) {
			t.Errorf("statuses = %v, want idle then offline", got)
		}
	})
}

func TestCancelledOfferAbandonsTheJob(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness()
		stop, _ := h.run(t.Context())
		h.offer("off_1", "ord_1", store)
		time.Sleep(10 * time.Second)
		h.dispatch.feed <- clients.OfferEvent{Cancelled: &clients.OfferCancelled{OfferID: "off_other"}} // not ours
		h.dispatch.feed <- clients.OfferEvent{Cancelled: &clients.OfferCancelled{OfferID: "off_1", Reason: "order_cancelled"}}
		time.Sleep(2 * time.Minute)
		stop()
		if len(h.orders.pickups) != 0 {
			t.Errorf("picked up a cancelled order: %v", h.orders.pickups)
		}
		if got := testutil.ToFloat64(h.metrics.JobsAbandoned.WithLabelValues("offer_cancelled")); got != 1 {
			t.Errorf("abandoned = %v, want 1", got)
		}
	})
}

func TestFailedActionsAbandonTheJob(t *testing.T) {
	cases := []struct {
		name       string
		pickupErr  func(int) error
		deliverErr func(int) error
		reason     string
	}{
		{"order cancelled before pickup", func(int) error { return &clients.APIError{Status: 409} }, nil, "order_gone"},
		{"order reassigned", func(int) error { return &clients.APIError{Status: 404} }, nil, "order_gone"},
		{"pickup keeps failing", func(int) error { return errors.New("connection refused") }, nil, "pickup_failed"},
		{"proof too far", nil, func(int) error { return &clients.APIError{Status: 422, Code: "proof_too_far"} }, "deliver_rejected"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness()
				h.orders.pickupErr, h.orders.deliverErr = tc.pickupErr, tc.deliverErr
				stop, _ := h.run(t.Context())
				h.offer("off_1", "ord_1", store)
				time.Sleep(5 * time.Minute)
				stop()
				if got := testutil.ToFloat64(h.metrics.JobsAbandoned.WithLabelValues(tc.reason)); got != 1 {
					t.Errorf("abandoned(%s) = %v, want 1", tc.reason, got)
				}
				if testutil.ToFloat64(h.metrics.Deliveries) != 0 {
					t.Error("no delivery should complete")
				}
			})
		})
	}
}

func TestTransientPickupFailureIsRetried(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness()
		h.orders.pickupErr = func(n int) error {
			if n == 1 {
				return &clients.APIError{Status: 503}
			}
			return nil
		}
		stop, _ := h.run(t.Context())
		h.offer("off_1", "ord_1", store)
		time.Sleep(5 * time.Minute)
		stop()
		if len(h.orders.pickups) != 2 || testutil.ToFloat64(h.metrics.Deliveries) != 1 {
			t.Errorf("pickups %v, deliveries %v", h.orders.pickups, testutil.ToFloat64(h.metrics.Deliveries))
		}
	})
}

// 503 from fleet-ingest: the batch is dropped (lossy path) and the phone stays quiet until
// Retry-After has passed; points taken meanwhile are sent together afterwards.
func TestShedBatchIsDroppedAndTheCourierBacksOff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness()
		h.fleet.respond = func(n int) error {
			if n == 1 {
				return &clients.APIError{Status: 503, RetryAfter: 20 * time.Second}
			}
			return nil
		}
		start := time.Now()
		stop, _ := h.run(t.Context())
		time.Sleep(31 * time.Second) // idle ticks at 0 s (shed), 15 s (paused), 30 s (sent)
		h.fleet.mu.Lock()
		batches := h.fleet.batches
		h.fleet.mu.Unlock()
		stop()

		if len(batches) < 2 {
			t.Fatalf("got %d batches, want at least 2", len(batches))
		}
		second := batches[1]
		if len(second) != 2 || !second[0].DeviceTime.Equal(start.Add(15*time.Second).UTC()) ||
			!second[1].DeviceTime.Equal(start.Add(30*time.Second).UTC()) {
			t.Fatalf("second batch = %+v, want the 15 s and 30 s points (the shed 0 s point is gone)", second)
		}
		if testutil.ToFloat64(h.metrics.LocationBatches.WithLabelValues("shed")) != 1 ||
			testutil.ToFloat64(h.metrics.LocationPoints.WithLabelValues("lost")) != 1 {
			t.Error("expected one shed batch and one lost point")
		}
	})
}

func TestNetworkErrorsKeepPointsUpToTheBatchLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness()
		h.fleet.respond = func(int) error { return errors.New("connection reset") }
		stop, _ := h.run(t.Context())
		time.Sleep(54*15*time.Second + time.Second) // 55 idle ticks
		stop()

		for i, b := range h.fleet.batches {
			if len(b) > clients.MaxBatch {
				t.Fatalf("batch %d has %d points, more than %d", i, len(b), clients.MaxBatch)
			}
		}
		if got := len(h.fleet.batches[1]); got != 2 {
			t.Errorf("second batch has %d points, want the failed one plus a new one", got)
		}
		// 55 idle points plus the final offline one, 50 kept: the 6 oldest were dropped.
		if got := testutil.ToFloat64(h.metrics.LocationPoints.WithLabelValues("lost")); got != 6 {
			t.Errorf("lost points = %v, want 6", got)
		}
	})
}

func TestRejectedBatchIsNotResent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness()
		h.fleet.respond = func(n int) error {
			if n == 1 {
				return &clients.APIError{Status: 422, Code: "invalid_batch"}
			}
			return nil
		}
		stop, _ := h.run(t.Context())
		time.Sleep(16 * time.Second)
		stop()
		if got := len(h.fleet.batches[1]); got != 1 {
			t.Errorf("second batch has %d points, want only the new one", got)
		}
	})
}

func TestShiftsStartAndEndOnTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness()
		start := time.Now()
		h.set.Clock = clock.New(start, 9*time.Hour+59*time.Minute, 1) // 09:59
		h.cfg.ShiftStart, h.cfg.ShiftEnd = 10*time.Hour, 10*time.Hour+5*time.Minute
		stop, _ := h.run(t.Context())

		time.Sleep(30 * time.Second)
		if n := len(h.fleet.points()); n != 0 {
			t.Fatalf("%d points before the shift started", n)
		}
		time.Sleep(10 * time.Minute)
		points := h.fleet.points()
		if first := points[0]; first.Status != "idle" || !first.DeviceTime.Equal(start.Add(time.Minute).UTC()) {
			t.Errorf("first point = %+v, want idle at 10:00", first)
		}
		if last := points[len(points)-1]; last.Status != "offline" || !last.DeviceTime.Equal(start.Add(6*time.Minute).UTC()) {
			t.Errorf("last point = %+v, want offline at 10:05", last)
		}
		if testutil.ToFloat64(h.metrics.CouriersActive) != 0 {
			t.Error("the courier should no longer be active")
		}
		// The next shift is tomorrow at 10:00; look at 10:01.
		time.Sleep(time.Until(start.Add(24*time.Hour + 2*time.Minute)))
		if last := h.fleet.points(); last[len(last)-1].Status != "idle" {
			t.Errorf("the courier should be back on shift the next day")
		}
		stop()
	})
}

func TestDrainFinishesTheOrderInHand(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness()
		_, done := h.run(t.Context())
		h.offer("off_1", "ord_1", store)
		time.Sleep(10 * time.Second)
		close(h.drain)
		h.offer("off_2", "ord_2", store) // no new work while draining

		select {
		case <-done:
		case <-time.After(time.Hour):
			t.Fatal("the courier did not log off after draining")
		}
		if !equal(h.orders.delivers, []string{"ord_1"}) || !equal(h.dispatch.declined, []string{"off_2"}) {
			t.Errorf("delivered %v, declined %v", h.orders.delivers, h.dispatch.declined)
		}
		if points := h.fleet.points(); points[len(points)-1].Status != "offline" {
			t.Error("the last point should be offline")
		}
	})
}

func TestDrainBeforeTheShiftStops(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness()
		h.cfg.ShiftStart, h.cfg.ShiftEnd = 20*time.Hour, 21*time.Hour // starts in 8 h
		_, done := h.run(t.Context())
		close(h.drain)
		<-done
		if n := len(h.fleet.points()); n != 0 {
			t.Errorf("%d points sent by a courier who never started", n)
		}
	})
}

func TestRejectedTokenStopsTheCourier(t *testing.T) {
	t.Run("offer stream", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := newHarness()
			h.dispatch.followErr = clients.ErrUnauthorized
			_, done := h.run(t.Context())
			<-done
		})
	})
	t.Run("fleet-ingest", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := newHarness()
			h.fleet.respond = func(int) error { return &clients.APIError{Status: 401} }
			_, done := h.run(t.Context())
			<-done
			if n := len(h.fleet.batches); n != 1 {
				t.Errorf("%d batches, want 1 (no offline report with a rejected token)", n)
			}
		})
	})
}

func TestUnknownOrderUsesTheDefaultPrepTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness()
		h.set.Clock = clock.New(time.Now(), 12*time.Hour, 10) // 10× speed: 5 min of prep is 30 s
		stop, _ := h.run(t.Context())
		h.offer("off_9", "ord_unknown", store)
		time.Sleep(3 * time.Minute)
		stop()
		if !equal(h.orders.pickups, []string{"ord_unknown"}) {
			t.Errorf("pickups = %v", h.orders.pickups)
		}
	})
}
