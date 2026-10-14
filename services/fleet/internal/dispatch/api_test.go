package dispatch

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/events"
)

func TestOfferStreamSendsTheOpenOfferOnConnectAndPings(t *testing.T) {
	w := newWorld(t)
	r := w.replica("")
	r.api.cfg.Heartbeat = 20 * time.Millisecond
	c := w.addCourier(north(shibuya, 300))
	id := w.confirm(r)
	r.matcher.Tick(context.Background()) // the offer is made before the courier connects

	resp, ch := stream(t, r.url, token(t, c))
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Accel-Buffering") != "no" ||
		resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d, headers %v", resp.StatusCode, resp.Header)
	}
	ev := next(t, ch)
	var data map[string]any
	if err := json.Unmarshal([]byte(ev.data), &data); err != nil || ev.name != eventOffer {
		t.Fatalf("got %+v (%v)", ev, err)
	}
	for _, field := range []string{"offer_id", "order_id", "store", "dropoff", "ready_at", "expires_at", "eta_pickup_s", "eta_dropoff_s"} {
		if _, ok := data[field]; !ok {
			t.Errorf("offer has no %q: %s", field, ev.data)
		}
	}
	if data["order_id"] != id {
		t.Fatalf("offer for the wrong order: %s", ev.data)
	}
	// Then only pings: the heartbeat re-read finds the same offer and does not repeat it.
	deadline := time.After(200 * time.Millisecond)
	pings := 0
	for pings < 3 {
		select {
		case e := <-ch:
			if !e.comment {
				t.Fatalf("unexpected event %+v", e)
			}
			pings++
		case <-deadline:
			t.Fatalf("only %d pings", pings)
		}
	}
}

func TestOfferStreamRecoversALostMessageAtTheHeartbeat(t *testing.T) {
	w := newWorld(t)
	r := w.replica("")
	r.api.cfg.Heartbeat = 20 * time.Millisecond
	c := w.addCourier(north(shibuya, 300))
	_, ch := stream(t, r.url, token(t, c))
	w.confirm(r)
	// Make the offer without publishing it, as if the pub/sub message had been lost.
	r.matcher.notify = &Notifier{rdb: deadRedis(t), logger: discard()}
	r.matcher.Tick(context.Background())
	if ev := next(t, ch); ev.name != eventOffer {
		t.Fatalf("got %+v", ev)
	}
}

func TestOfferStreamRejectsAndLimits(t *testing.T) {
	w := newWorld(t)
	r := w.replica("")
	if resp, _ := stream(t, r.url, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
	tracking, _ := tokenWithRole(t, "tracking", events.NewID(events.PrefixOrder))
	if resp, _ := stream(t, r.url, tracking); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("tracking token: %d", resp.StatusCode)
	}
	r.hub.max = 1
	c := events.NewID(events.PrefixCourier)
	stream(t, r.url, token(t, c))
	if resp, _ := stream(t, r.url, token(t, c)); resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("over the limit: %d", resp.StatusCode)
	}
}

func TestOfferStreamEndsOnShutdown(t *testing.T) {
	w := newWorld(t)
	r := w.replica("")
	_, ch := stream(t, r.url, token(t, events.NewID(events.PrefixCourier)))
	waitFor(t, "the stream", func() bool { return r.hub.streamCount() == 1 })
	close(r.stopping)
	for range ch { // drains until the server ends the stream
	}
	waitFor(t, "the stream to unregister", func() bool { return r.hub.streamCount() == 0 })
}

func TestCancelWithdrawsTheOffer(t *testing.T) {
	w := newWorld(t)
	owner, other := w.replica(""), w.replica("")
	c := w.addCourier(north(shibuya, 300))
	id := w.confirm(owner)
	_, ch := stream(t, other.url, token(t, c)) // the courier is connected to the other replica
	owner.matcher.Tick(context.Background())
	offer := next(t, ch)
	if offer.name != eventOffer {
		t.Fatalf("got %+v", offer)
	}
	offerID := *w.prod.of(events.OfferSent)[0].OfferID

	owner.apply(t, orderEvent(events.OrderCancelled, id, w.clock.Now()))

	ev := next(t, ch)
	if ev.name != eventOfferCancelled || !strings.Contains(ev.data, offerID) || !strings.Contains(ev.data, reasonOrderCancelled) {
		t.Fatalf("got %+v", ev)
	}
	if code, body := post(t, other.url+"/v1/offers/"+offerID+"/accept", token(t, c)); code != http.StatusConflict {
		t.Fatalf("accept after cancel: %d %v", code, body)
	}
	if w.rdb.Exists(context.Background(), reservedKey(c)).Val() != 0 {
		t.Fatal("the courier is still reserved")
	}
	if o := w.order(id); o.Status != statusDone {
		t.Fatalf("order: %+v", o)
	}
}

func TestAcceptOnAnotherReplica(t *testing.T) {
	w := newWorld(t)
	a, b, c := w.replica(""), w.replica(""), w.replica("")
	courier := w.addCourier(north(shibuya, 300))
	id := w.confirm(a)
	_, ch := stream(t, b.url, token(t, courier)) // stream on b
	a.matcher.Tick(context.Background())         // offer made on a
	ev := next(t, ch)
	var offer offerData
	if err := json.Unmarshal([]byte(ev.data), &offer); err != nil {
		t.Fatal(err)
	}
	code, body := post(t, c.url+"/v1/offers/"+offer.OfferID+"/accept", token(t, courier)) // answer on c
	if code != http.StatusOK || body["status"] != "accepted" {
		t.Fatalf("accept on another replica: %d %v", code, body)
	}
	if as := w.prod.of(events.CourierAssigned); len(as) != 1 || as[0].OrderID != id {
		t.Fatalf("CourierAssigned: %+v", as)
	}

	// The cancellation of an assigned order reaches the courier too.
	a.apply(t, orderEvent(events.OrderCancelled, id, w.clock.Now()))
	if ev := next(t, ch); ev.name != eventOfferCancelled {
		t.Fatalf("got %+v", ev)
	}
	if w.rdb.Exists(context.Background(), busyKey(courier)).Val() != 0 {
		t.Fatal("a cancelled order must free its courier")
	}
}

func getETA(t *testing.T, url string) (int, map[string]any, time.Duration) {
	t.Helper()
	start := time.Now()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body, time.Since(start)
}

const etaQuery = "/v1/eta?ward=shibuya&store_lat=35.658&store_lng=139.7016&dropoff_lat=35.645&dropoff_lng=139.71"

func TestETAAnswersInTimeWhenOSRMIsSlow(t *testing.T) {
	url, _ := osrmServer(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	})
	w := newWorld(t)
	r := w.replica(url)
	for i := range 3 {
		w.addCourier(north(shibuya, float64(300*(i+1))))
	}
	code, body, took := getETA(t, r.url+etaQuery)
	if code != http.StatusOK || took > 300*time.Millisecond {
		t.Fatalf("status %d after %v (orders gives up at 300 ms): %v", code, took, body)
	}
	pickup := FallbackSeconds(HaversineMetres(north(shibuya, 300), shibuya))
	travel := FallbackSeconds(HaversineMetres(shibuya, dropoff))
	if int(body["pickup_s"].(float64)) != pickup || int(body["travel_s"].(float64)) != travel ||
		int(body["eta_seconds"].(float64)) != pickup+travel || body["supply"] != supplyOK {
		t.Fatalf("got %v, want pickup %d travel %d", body, pickup, travel)
	}
}

func TestETAUsesRoadTimesAndReportsSupply(t *testing.T) {
	url, _ := osrmServer(t, durations([]any{120, 0}, []any{0, 700}))
	w := newWorld(t)
	r := w.replica(url)
	w.addCourier(north(shibuya, 300))
	code, body, _ := getETA(t, r.url+etaQuery)
	if code != http.StatusOK || body["pickup_s"] != 120.0 || body["travel_s"] != 700.0 || body["eta_seconds"] != 820.0 ||
		body["supply"] != supplyLow {
		t.Fatalf("%d %v", code, body)
	}
}

func TestETAWithoutCouriers(t *testing.T) {
	w := newWorld(t)
	r := w.replica("")
	code, body, _ := getETA(t, r.url+etaQuery)
	if code != http.StatusOK || body["supply"] != supplyNone || int(body["pickup_s"].(float64)) != FallbackSeconds(4000) {
		t.Fatalf("%d %v", code, body)
	}
}

func TestETARejectsBadQueries(t *testing.T) {
	w := newWorld(t)
	r := w.replica("")
	for q, want := range map[string]string{
		"/v1/eta?ward=atlantis&store_lat=35&store_lng=139&dropoff_lat=35&dropoff_lng=139": "unknown_ward",
		"/v1/eta?store_lat=x&store_lng=139&dropoff_lat=35&dropoff_lng=139":                "invalid_query",
		"/v1/eta?store_lat=35&store_lng=139":                                              "invalid_query",
		"/v1/eta?store_lat=89&store_lng=139&dropoff_lat=35&dropoff_lng=139":               "invalid_query",
		"/v1/eta?store_lat=35&store_lng=Inf&dropoff_lat=35&dropoff_lng=139":               "invalid_query",
	} {
		if code, body, _ := getETA(t, r.url+q); code != http.StatusBadRequest || errorCode(body) != want {
			t.Errorf("%s: %d %v", q, code, body)
		}
	}
	w.mr.SetError("LOADING")
	defer w.mr.SetError("")
	if code, _, _ := getETA(t, r.url+etaQuery); code != http.StatusServiceUnavailable {
		t.Fatalf("redis down: %d", code)
	}
}
