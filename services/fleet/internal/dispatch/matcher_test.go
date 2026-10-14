package dispatch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/events"
)

// osrmServer is a fake OSRM. It counts the calls it gets.
func osrmServer(t *testing.T, h http.HandlerFunc) (string, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		h(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts.URL, &hits
}

func durations(rows ...[]any) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "Ok", "durations": rows})
	}
}

func TestOffersTheNearestCourierByRoad(t *testing.T) {
	var query string
	url, hits := osrmServer(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Path + "?" + r.URL.RawQuery
		// Rows: couriers A, B, C (nearest first in a straight line), then the store.
		// Columns: to the store, to the drop-off. A is closest on the map but across a river.
		durations([]any{900, 0}, []any{200, 0}, []any{500, 0}, []any{0, 1300})(w, r)
	})
	w := newWorld(t)
	r := w.replica(url)
	w.addCourier(north(shibuya, 300))
	b := w.addCourier(north(shibuya, 600))
	w.addCourier(north(shibuya, 900))
	id := w.confirm(r)

	r.matcher.Tick(context.Background())

	sent := w.prod.of(events.OfferSent)
	if len(sent) != 1 || *sent[0].CourierID != b || *sent[0].EtaPickupS != 200 || *sent[0].EtaDropoffS != 1300 {
		t.Fatalf("offer went to the wrong courier: %+v", sent)
	}
	if hits.Load() != 1 {
		t.Fatalf("OSRM called %d times, want one table call per order", hits.Load())
	}
	if !strings.HasPrefix(query, "/table/v1/bicycle/") || !strings.Contains(query, "sources=0;1;2;3&destinations=4;5") {
		t.Fatalf("unexpected OSRM request %s", query)
	}
	if o := w.order(id); o.Status != statusOffered || o.CourierID != b || o.Attempts != 1 {
		t.Fatalf("order after the offer: %+v", o)
	}
	if testutil.ToFloat64(r.metrics.etaRequests.WithLabelValues(sourceOSRM)) != 1 ||
		testutil.ToFloat64(r.metrics.offers.WithLabelValues("sent")) != 1 {
		t.Fatal("metrics not counted")
	}
}

func TestFallsBackToStraightLineWhenOSRMFails(t *testing.T) {
	url, _ := osrmServer(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	w := newWorld(t)
	r := w.replica(url)
	a := w.addCourier(north(shibuya, 300))
	w.addCourier(north(shibuya, 600))
	w.confirm(r)

	r.matcher.Tick(context.Background())

	sent := w.prod.of(events.OfferSent)
	want := FallbackSeconds(HaversineMetres(north(shibuya, 300), shibuya))
	if len(sent) != 1 || *sent[0].CourierID != a || abs(*sent[0].EtaPickupS-want) > 1 {
		t.Fatalf("want the straight-line nearest with ~%d s, got %+v", want, sent)
	}
	if testutil.ToFloat64(r.metrics.etaRequests.WithLabelValues(sourceFallback)) != 1 {
		t.Fatal("fallback not counted")
	}
}

func abs(n int) int { return max(n, -n) }

func TestSearchWidensUpToTheMaximumRadius(t *testing.T) {
	w := newWorld(t)
	r := w.replica("")
	far := w.addCourier(north(shibuya, 2500)) // outside the first 1 km, inside the 4 km maximum
	w.addCourier(north(shibuya, 6000))        // beyond the maximum: never offered
	w.confirm(r)
	r.matcher.Tick(context.Background())
	if sent := w.prod.of(events.OfferSent); len(sent) != 1 || *sent[0].CourierID != far {
		t.Fatalf("got %+v", sent)
	}
}

func TestTwoReplicasNeverOfferOneCourierTwice(t *testing.T) {
	w := newWorld(t)
	// Both replicas own partition 0: the overlap a rebalance can cause, at its worst.
	a, b := w.replica(""), w.replica("")
	couriers := map[string]bool{}
	for i := range 5 {
		couriers[w.addCourier(north(shibuya, float64(100+150*i)))] = true
	}
	for range 12 {
		w.confirm(a)
	}
	for round := range 3 {
		var wg sync.WaitGroup
		for _, r := range []*replica{a, b, a, b} {
			wg.Go(func() { r.matcher.Tick(context.Background()) })
		}
		wg.Wait()
		byCourier, byOrder := w.openOffers()
		for c, n := range byCourier {
			if n > 1 {
				t.Fatalf("round %d: courier %s holds %d open offers", round, c, n)
			}
		}
		for o, n := range byOrder {
			if n > 1 {
				t.Fatalf("round %d: order %s has %d open offers", round, o, n)
			}
		}
		if len(byCourier) != len(couriers) {
			t.Fatalf("round %d: %d of %d couriers hold an offer", round, len(byCourier), len(couriers))
		}
	}
	if n := len(w.prod.of(events.OfferSent)); n != 5 {
		t.Fatalf("%d offers sent, want one per courier", n)
	}
}

func TestAcceptAssignsTheCourier(t *testing.T) {
	w := newWorld(t)
	r := w.replica("")
	c := w.addCourier(north(shibuya, 300))
	id := w.confirm(r)
	w.clock.Advance(20 * time.Second)
	r.matcher.Tick(context.Background())
	offerID := *w.prod.of(events.OfferSent)[0].OfferID
	accept := r.url + "/v1/offers/" + offerID + "/accept"

	if code, body := post(t, accept, token(t, events.NewID(events.PrefixCourier))); code != http.StatusConflict ||
		errorCode(body) != "offer_not_yours" {
		t.Fatalf("another courier: %d %v", code, body)
	}
	code, body := post(t, accept, token(t, c))
	if code != http.StatusOK || body["status"] != "accepted" || body["order_id"] != id || body["offer_id"] != offerID {
		t.Fatalf("accept: %d %v", code, body)
	}
	assigned := w.prod.of(events.CourierAssigned)
	if len(assigned) != 1 || *assigned[0].CourierID != c || assigned[0].EtaPickupS == nil || assigned[0].EtaDropoffS == nil ||
		len(w.prod.of(events.OfferAccepted)) != 1 {
		t.Fatalf("events: %+v", assigned)
	}
	if o := w.order(id); o.Status != statusAssigned || o.CourierID != c {
		t.Fatalf("order: %+v", o)
	}
	if got := w.rdb.Get(context.Background(), busyKey(c)).Val(); got != id {
		t.Fatalf("courier not marked busy: %q", got)
	}
	if w.rdb.Exists(context.Background(), reservedKey(c)).Val() != 0 || w.rdb.SIsMember(context.Background(), pendingKey(0), id).Val() {
		t.Fatal("reservation or pending entry left behind")
	}
	if histCount(t, r.reg, "dispatch_assignment_duration_seconds", outcomeAssigned) != 1 {
		t.Fatal("SLI not observed")
	}
	// The outbox is empty once Kafka acknowledged the events.
	if n := w.rdb.ZCard(context.Background(), outboxKey).Val(); n != 0 {
		t.Fatalf("%d events left in the outbox", n)
	}

	// A retried accept is answered the same way and changes nothing.
	if code, _ := post(t, accept, token(t, c)); code != http.StatusOK || len(w.prod.of(events.CourierAssigned)) != 1 {
		t.Fatalf("retried accept: %d", code)
	}
	if code, body := post(t, r.url+"/v1/offers/"+offerID+"/decline", token(t, c)); code != http.StatusConflict ||
		errorCode(body) != "offer_closed" {
		t.Fatalf("decline after accept: %d %v", code, body)
	}
	// The busy courier gets no new offer.
	w.confirm(r)
	r.matcher.Tick(context.Background())
	if n := len(w.prod.of(events.OfferSent)); n != 1 {
		t.Fatalf("a busy courier was offered another order (%d offers)", n)
	}
}

func TestAnswerRejectsBadRequests(t *testing.T) {
	w := newWorld(t)
	r := w.replica("")
	unknown := r.url + "/v1/offers/" + events.NewID(events.PrefixOffer) + "/accept"
	cases := []struct {
		name, url, tok, code string
		status               int
	}{
		{"no token", unknown, "", "unauthorized", http.StatusUnauthorized},
		{"bad token", unknown, "nonsense", "invalid_token", http.StatusUnauthorized},
		{"malformed offer", r.url + "/v1/offers/off_x/accept", token(t, events.NewID(events.PrefixCourier)), "offer_not_found", http.StatusNotFound},
		{"malformed decline", r.url + "/v1/offers/off_x/decline", token(t, events.NewID(events.PrefixCourier)), "offer_not_found", http.StatusNotFound},
		{"unknown offer", unknown, token(t, events.NewID(events.PrefixCourier)), "offer_not_found", http.StatusNotFound},
	}
	for _, tc := range cases {
		if status, body := post(t, tc.url, tc.tok); status != tc.status || errorCode(body) != tc.code {
			t.Errorf("%s: %d %v", tc.name, status, body)
		}
	}
	// Redis down: 503, so the courier app retries.
	w.mr.SetError("LOADING")
	defer w.mr.SetError("")
	if status, body := post(t, unknown, token(t, events.NewID(events.PrefixCourier))); status != http.StatusServiceUnavailable {
		t.Errorf("redis down: %d %v", status, body)
	}
}

func TestDeclineOffersTheNextCourier(t *testing.T) {
	w := newWorld(t)
	r := w.replica("")
	first := w.addCourier(north(shibuya, 300))
	second := w.addCourier(north(shibuya, 600))
	id := w.confirm(r)
	r.matcher.Tick(context.Background())
	offerID := *w.prod.of(events.OfferSent)[0].OfferID
	decline := r.url + "/v1/offers/" + offerID + "/decline"

	code, body := post(t, decline, token(t, first))
	if code != http.StatusOK || body["status"] != "declined" {
		t.Fatalf("decline: %d %v", code, body)
	}
	if code, _ := post(t, decline, token(t, first)); code != http.StatusOK {
		t.Fatalf("retried decline: %d", code)
	}
	if code, body := post(t, r.url+"/v1/offers/"+offerID+"/accept", token(t, first)); code != http.StatusConflict ||
		errorCode(body) != "offer_closed" {
		t.Fatalf("accept after decline: %d %v", code, body)
	}
	if len(w.prod.of(events.OfferDeclined)) != 1 {
		t.Fatal("OfferDeclined not emitted")
	}
	r.matcher.Tick(context.Background())
	sent := w.prod.of(events.OfferSent)
	if len(sent) != 2 || *sent[1].CourierID != second {
		t.Fatalf("the order should go to the next courier, got %+v", sent)
	}
	if o := w.order(id); o.Attempts != 2 || o.CourierID != second {
		t.Fatalf("order: %+v", o)
	}
}

func TestExpiredOfferGoesToTheNextCourier(t *testing.T) {
	w := newWorld(t)
	r := w.replica("")
	first := w.addCourier(north(shibuya, 300))
	second := w.addCourier(north(shibuya, 600))
	w.confirm(r)
	_, events1 := stream(t, r.url, token(t, first))
	r.matcher.Tick(context.Background())
	offer := next(t, events1)
	offerID := *w.prod.of(events.OfferSent)[0].OfferID
	if offer.name != eventOffer || !strings.Contains(offer.data, offerID) {
		t.Fatalf("got %+v", offer)
	}

	w.clock.Advance(31 * time.Second)
	if code, body := post(t, r.url+"/v1/offers/"+offerID+"/accept", token(t, first)); code != http.StatusConflict ||
		errorCode(body) != "offer_expired" {
		t.Fatalf("late accept: %d %v", code, body)
	}
	if code, body := post(t, r.url+"/v1/offers/"+offerID+"/decline", token(t, first)); code != http.StatusConflict ||
		errorCode(body) != "offer_expired" {
		t.Fatalf("late decline: %d %v", code, body)
	}
	r.matcher.Tick(context.Background())
	if ev := next(t, events1); ev.name != eventOfferCancelled || !strings.Contains(ev.data, `"reason":"expired"`) {
		t.Fatalf("got %+v", ev)
	}
	if len(w.prod.of(events.OfferExpired)) != 1 {
		t.Fatal("OfferExpired not emitted")
	}
	sent := w.prod.of(events.OfferSent)
	if len(sent) != 2 || *sent[1].CourierID != second {
		t.Fatalf("want a new offer to the second courier, got %+v", sent)
	}
	if w.rdb.Exists(context.Background(), reservedKey(first)).Val() != 0 {
		t.Fatal("the first courier is still reserved")
	}
	if testutil.ToFloat64(r.metrics.offers.WithLabelValues("expired")) != 1 {
		t.Fatal("expiry not counted")
	}
}

func TestNoCourierAvailableIsReportedRepeatedlyButObservedOnce(t *testing.T) {
	w := newWorld(t)
	r := w.replica("")
	id := w.confirm(r)
	ctx := context.Background()

	r.matcher.Tick(ctx) // nobody around, but it has only just been confirmed
	if n := len(w.prod.of(events.NoCourierAvailable)); n != 0 {
		t.Fatalf("reported too early (%d)", n)
	}
	w.clock.Advance(61 * time.Second)
	r.matcher.Tick(ctx)
	if n := len(w.prod.of(events.NoCourierAvailable)); n != 1 {
		t.Fatalf("want one report after NO_COURIER_AFTER, got %d", n)
	}
	if histCount(t, r.reg, "dispatch_assignment_duration_seconds", outcomeNoCourier) != 1 {
		t.Fatal("the SLI must count an order without a courier as a bad event")
	}
	w.clock.Advance(10 * time.Second) // retried, but reported at most once a minute
	r.matcher.Tick(ctx)
	if n := len(w.prod.of(events.NoCourierAvailable)); n != 1 {
		t.Fatalf("reported again too soon (%d)", n)
	}
	w.clock.Advance(time.Minute)
	r.matcher.Tick(ctx)
	if n := len(w.prod.of(events.NoCourierAvailable)); n != 2 {
		t.Fatalf("orders needs repeated reports to cancel at 15 min; got %d", n)
	}

	// A courier comes online: the order is still offered, and the SLI is not observed again.
	c := w.addCourier(north(shibuya, 200))
	w.clock.Advance(5 * time.Second)
	r.matcher.Tick(ctx)
	sent := w.prod.of(events.OfferSent)
	if len(sent) != 1 || *sent[0].CourierID != c {
		t.Fatalf("want an offer once a courier appears, got %+v", sent)
	}
	if code, _ := post(t, r.url+"/v1/offers/"+*sent[0].OfferID+"/accept", token(t, c)); code != http.StatusOK {
		t.Fatalf("accept: %d", code)
	}
	if histCount(t, r.reg, "dispatch_assignment_duration_seconds", outcomeAssigned) != 0 ||
		histCount(t, r.reg, "dispatch_assignment_duration_seconds", outcomeNoCourier) != 1 {
		t.Fatal("the SLI must be observed exactly once per order")
	}
	if o := w.order(id); o.Status != statusAssigned {
		t.Fatalf("order: %+v", o)
	}
}

func TestMaxOfferAttemptsReportsNoCourierButKeepsOffering(t *testing.T) {
	w := newWorld(t)
	r := w.replica("")
	var couriers []string
	for i := range 4 {
		couriers = append(couriers, w.addCourier(north(shibuya, float64(200*(i+1)))))
	}
	w.confirm(r)
	ctx := context.Background()
	for i := range 3 {
		r.matcher.Tick(ctx)
		sent := w.prod.of(events.OfferSent)
		if len(sent) != i+1 {
			t.Fatalf("attempt %d: %d offers", i+1, len(sent))
		}
		if code, _ := post(t, r.url+"/v1/offers/"+*sent[i].OfferID+"/decline", token(t, *sent[i].CourierID)); code != http.StatusOK {
			t.Fatalf("decline %d: %d", i+1, code)
		}
	}
	r.matcher.Tick(ctx) // three offers declined in seconds: report now, and offer the fourth courier
	if n := len(w.prod.of(events.NoCourierAvailable)); n != 1 {
		t.Fatalf("want NoCourierAvailable after MAX_OFFER_ATTEMPTS, got %d", n)
	}
	if sent := w.prod.of(events.OfferSent); len(sent) != 4 || *sent[3].CourierID != couriers[3] {
		t.Fatalf("the order must still be offered: %+v", sent)
	}
	if histCount(t, r.reg, "dispatch_assignment_duration_seconds", outcomeNoCourier) != 1 {
		t.Fatal("no_courier not observed")
	}
}

func TestEveryEmittedEventTypeMatchesTheSchema(t *testing.T) {
	// The fake producer validates each event against contracts/events/dispatch-event.v1; this
	// test makes sure every type goes through it at least once.
	w := newWorld(t)
	r := w.replica("")
	a := w.addCourier(north(shibuya, 200))
	b := w.addCourier(north(shibuya, 400))
	w.confirm(r)
	ctx := context.Background()
	r.matcher.Tick(ctx) // OfferSent to a
	post(t, r.url+"/v1/offers/"+*w.prod.of(events.OfferSent)[0].OfferID+"/decline", token(t, a))
	r.matcher.Tick(ctx) // OfferSent to b
	w.clock.Advance(31 * time.Second)
	r.matcher.Tick(ctx) // OfferExpired; nobody left; reported once the order is a minute old
	w.clock.Advance(time.Minute)
	r.matcher.Tick(ctx) // NoCourierAvailable
	c := w.addCourier(north(shibuya, 300))
	w.clock.Advance(5 * time.Second)
	r.matcher.Tick(ctx)
	sent := w.prod.of(events.OfferSent)
	post(t, r.url+"/v1/offers/"+*sent[len(sent)-1].OfferID+"/accept", token(t, c)) // OfferAccepted, CourierAssigned
	_ = b
	for _, typ := range []events.DispatchEventType{events.OfferSent, events.OfferAccepted, events.OfferDeclined,
		events.OfferExpired, events.CourierAssigned, events.NoCourierAvailable} {
		if len(w.prod.of(typ)) == 0 {
			t.Errorf("no %s was emitted", typ)
		}
	}
}

func TestMatcherSkipsWhileTheGateIsClosed(t *testing.T) {
	w := newWorld(t)
	r := w.replica("")
	w.addCourier(north(shibuya, 200))
	w.confirm(r)
	r.matcher.gate = closedGate{}
	r.matcher.Tick(context.Background())
	if len(w.prod.of(events.OfferSent)) != 0 {
		t.Fatal("matched while recovering")
	}
}

type closedGate struct{}

func (closedGate) Dispatching() bool { return false }

func TestMatcherRunTicksAndWakes(t *testing.T) {
	w := newWorld(t)
	r := w.replica("")
	w.addCourier(north(shibuya, 200))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.matcher.Run(ctx) }()
	w.confirm(r) // the consumer wakes the matcher
	waitFor(t, "an offer", func() bool { return len(w.prod.of(events.OfferSent)) == 1 })
	if testutil.ToFloat64(r.metrics.pending.WithLabelValues("shibuya")) != 1 {
		t.Fatal("pending gauge not set")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestMatcherSurvivesRedisErrors(t *testing.T) {
	w := newWorld(t)
	r := w.replica("")
	w.confirm(r)
	w.mr.SetError("LOADING")
	r.matcher.Tick(context.Background()) // logs and gives up until the next tick
	w.mr.SetError("")
	o := w.order(w.confirm(r))
	w.mr.SetError("LOADING")
	r.matcher.attempt(context.Background(), &o, w.clock.Now())
	r.matcher.noOffer(context.Background(), &o, w.clock.Now(), true)
	if r.matcher.expire(context.Background(), &o, w.clock.Now()) {
		t.Fatal("expire cannot succeed while Redis is down")
	}
	w.mr.SetError("")
}
