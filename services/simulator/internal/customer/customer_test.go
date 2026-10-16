package customer

import (
	"context"
	"fmt"
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

var (
	centre = geo.Point{Lat: 35.6640, Lng: 139.6982}
	box    = geo.BBox{MinLat: 35.52, MaxLat: 35.82, MinLng: 139.56, MaxLng: 139.92}
)

type placed struct {
	at      time.Time
	storeID string
	dropoff clients.Dropoff
	key     string
}

type fakeOrders struct {
	mu       sync.Mutex
	quotes   int
	orders   []placed
	pending  map[string]placed // quote ID → what was quoted
	quoteErr error
	orderErr error
	replayed bool
	block    chan struct{} // when set, CreateOrder waits for it
}

func (f *fakeOrders) Quote(_ context.Context, apiKey, storeID string, d clients.Dropoff) (clients.Quote, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.quoteErr != nil {
		return clients.Quote{}, f.quoteErr
	}
	f.quotes++
	id := fmt.Sprintf("quo_%s_%d", apiKey, f.quotes)
	if f.pending == nil {
		f.pending = map[string]placed{}
	}
	f.pending[id] = placed{at: time.Now(), storeID: storeID, dropoff: d}
	return clients.Quote{ID: id, ETASeconds: 1800}, nil
}

func (f *fakeOrders) CreateOrder(_ context.Context, _, quoteID, key string) (clients.Order, error) {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.orderErr != nil {
		return clients.Order{}, f.orderErr
	}
	p := f.pending[quoteID]
	p.key = key
	f.orders = append(f.orders, p)
	return clients.Order{ID: "ord_" + key, Replayed: f.replayed, TrackingURL: "http://localhost:8088/track/tok_" + key}, nil
}

type fakeKitchen struct {
	mu    sync.Mutex
	ready map[string]time.Time
}

func (k *fakeKitchen) Prepare(orderID string, at time.Time) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.ready == nil {
		k.ready = map[string]time.Time{}
	}
	k.ready[orderID] = at
}

// flat returns a curve with the same rate all day.
func flat(t *testing.T, perHour float64) demand.Curve {
	t.Helper()
	c, err := demand.NewCurve([]demand.Point{{At: 0, PerHour: perHour}})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

type fakeViewers struct {
	mu      sync.Mutex
	watched map[string]string // order ID → tracking URL
}

func (v *fakeViewers) Watch(orderID, trackingURL string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.watched == nil {
		v.watched = map[string]string{}
	}
	v.watched[orderID] = trackingURL
}

type setup struct {
	orders  *fakeOrders
	kitchen *fakeKitchen
	viewers *fakeViewers
	metrics *metrics.Metrics
	agent   *Agent
}

func newSetup(t *testing.T, perHour float64, seed uint64, inFlight int) *setup {
	return newWatchedSetup(t, perHour, seed, inFlight, 0)
}

func newWatchedSetup(t *testing.T, perHour float64, seed uint64, inFlight int, watchShare float64) *setup {
	s := &setup{
		orders: &fakeOrders{}, kitchen: &fakeKitchen{}, viewers: &fakeViewers{},
		metrics: metrics.New(prometheus.NewRegistry()),
	}
	s.agent = New(Config{
		WardID: "shibuya", WardName: "Shibuya", Centre: centre, RadiusM: 1500, BBox: box, Share: 0.5,
		Stores:  []Store{{ID: "str_1", APIKey: "key_a"}, {ID: "str_2", APIKey: "key_b"}},
		Curve:   flat(t, perHour),
		Clock:   clock.New(time.Now(), 12*time.Hour, 10),
		PrepMin: 5 * time.Minute, PrepMax: 15 * time.Minute,
		WatchShare: watchShare,
	}, Deps{
		Orders: s.orders, Kitchen: s.kitchen, Viewers: s.viewers, Metrics: s.metrics,
		InFlight: make(chan struct{}, inFlight),
	}, rand.New(rand.NewPCG(seed, 1)))
	return s
}

func (s *setup) runFor(d time.Duration) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.agent.Run(ctx)
		close(done)
	}()
	time.Sleep(d)
	cancel()
	<-done
}

func TestOrdersFollowTheRateAndAreWellFormed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// 7,200/h for the scenario, half of it in this ward: one order per second on average.
		s := newSetup(t, 7200, 42, 100)
		start := time.Now()
		s.runFor(10 * time.Minute)

		n := len(s.orders.orders)
		if n < 540 || n > 660 { // 600 expected; ±2.5 standard deviations
			t.Fatalf("%d orders in 10 minutes, want about 600", n)
		}
		keys := map[string]bool{}
		for _, o := range s.orders.orders {
			d := o.dropoff
			if dist := geo.Distance(centre, geo.Point{Lat: d.Lat, Lng: d.Lng}); dist > 1501 {
				t.Errorf("drop-off %.0f m from the ward centre", dist)
			}
			if d.Ward != "shibuya" || d.Address == "" || d.CustomerName == "" || len(d.CustomerPhone) != 13 {
				t.Errorf("bad drop-off %+v", d)
			}
			if keys[o.key] || len(o.key) < 26 {
				t.Errorf("idempotency key %q is reused or too short", o.key)
			}
			keys[o.key] = true
		}
		// Food is ready 5–15 minutes after the order, divided by the 10× speed-up.
		for id, ready := range s.kitchen.ready {
			if wait := ready.Sub(start); wait < 30*time.Second || wait > 10*time.Minute+90*time.Second {
				t.Errorf("order %s ready after %v", id, wait)
			}
		}
		m := s.metrics
		if got := testutil.ToFloat64(m.OrdersCreated.WithLabelValues("shibuya")); int(got) != n {
			t.Errorf("created metric = %v, want %d", got, n)
		}
		if got := testutil.ToFloat64(m.DemandRate.WithLabelValues("shibuya")); got != 3600 {
			t.Errorf("demand gauge = %v, want 3600", got)
		}
	})
}

func TestSameSeedSameOrders(t *testing.T) {
	run := func(seed uint64) []placed {
		var out []placed
		synctest.Test(t, func(t *testing.T) {
			s := newSetup(t, 3600, seed, 100)
			start := time.Now()
			s.runFor(5 * time.Minute)
			for _, o := range s.orders.orders {
				o.at = time.Time{}.Add(o.at.Sub(start)) // compare times relative to the start
				o.key = ""                              // keys are random on purpose
				out = append(out, o)
			}
		})
		return out
	}
	a, b, c := run(7), run(7), run(8)
	if len(a) == 0 || len(a) != len(b) {
		t.Fatalf("runs gave %d and %d orders", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("order %d differs between runs with the same seed:\n%+v\n%+v", i, a[i], b[i])
		}
	}
	if len(c) == len(a) && c[0] == a[0] {
		t.Error("a different seed gave the same orders")
	}
}

func TestFailuresAreCountedByStageAndReason(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newSetup(t, 7200, 1, 10)
		s.orders.quoteErr = &clients.APIError{Status: 503}
		s.runFor(time.Minute)
		if testutil.ToFloat64(s.metrics.OrdersFailed.WithLabelValues("quote", "http_503")) == 0 {
			t.Error("quote failures not counted")
		}

		s = newSetup(t, 7200, 1, 10)
		s.orders.orderErr = &clients.APIError{Status: 409, Code: "quote_expired"}
		s.runFor(time.Minute)
		if testutil.ToFloat64(s.metrics.OrdersFailed.WithLabelValues("order", "http_409")) == 0 {
			t.Error("order failures not counted")
		}
		if testutil.ToFloat64(s.metrics.OrdersCreated.WithLabelValues("shibuya")) != 0 {
			t.Error("failed orders counted as created")
		}
	})
}

func TestReplayedOrdersAreCounted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newSetup(t, 7200, 1, 10)
		s.orders.replayed = true
		s.runFor(time.Minute)
		created := testutil.ToFloat64(s.metrics.OrdersCreated.WithLabelValues("shibuya"))
		if created == 0 || testutil.ToFloat64(s.metrics.OrdersReplayed) != created {
			t.Errorf("replayed %v of %v", testutil.ToFloat64(s.metrics.OrdersReplayed), created)
		}
	})
}

// When the orders service stalls, at most InFlight orders wait for it; the rest fail fast
// with "backpressure" instead of piling up goroutines. Stopping waits for the stalled ones.
func TestBackpressureAndShutdownWaitForInFlightOrders(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newSetup(t, 7200, 1, 2)
		s.orders.block = make(chan struct{})

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			s.agent.Run(ctx)
			close(done)
		}()
		time.Sleep(time.Minute)
		cancel()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("Run returned while orders were still in flight")
		default:
		}
		if got := testutil.ToFloat64(s.metrics.OrdersFailed.WithLabelValues("order", "backpressure")); got < 10 {
			t.Errorf("backpressure failures = %v, want most of the minute's orders", got)
		}
		close(s.orders.block)
		<-done
		if got := len(s.orders.orders); got != 2 {
			t.Errorf("%d orders completed, want the 2 that were in flight", got)
		}
	})
}

func TestNoDemandNoOrders(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newSetup(t, 0, 1, 10)
		s.runFor(time.Hour)
		if s.orders.quotes != 0 {
			t.Errorf("%d quotes with zero demand", s.orders.quotes)
		}
	})
}

func TestAShareOfCustomersWatchTheirDelivery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newWatchedSetup(t, 7200, 7, 100, 0.6)
		s.runFor(10 * time.Minute)

		n := len(s.orders.orders)
		watched := len(s.viewers.watched)
		if share := float64(watched) / float64(n); share < 0.5 || share > 0.7 {
			t.Fatalf("%d of %d customers watched (%.2f), want about 0.6", watched, n, share)
		}
		for id, url := range s.viewers.watched {
			if url != "http://localhost:8088/track/tok_"+id[len("ord_"):] {
				t.Errorf("order %s watched with tracking URL %q", id, url)
			}
		}
	})
}

func TestNobodyWatchesWithAZeroShare(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newSetup(t, 3600, 11, 100)
		s.runFor(5 * time.Minute)
		if len(s.orders.orders) == 0 || len(s.viewers.watched) != 0 {
			t.Fatalf("%d orders, %d watched: want orders and nobody watching", len(s.orders.orders), len(s.viewers.watched))
		}
	})
}
