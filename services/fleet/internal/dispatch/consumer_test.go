package dispatch

import (
	"context"
	"math"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/events"
)

func TestConsumerDedupesByEventID(t *testing.T) {
	w := newWorld(t)
	r := w.replica("")
	e := orderEvent(events.OrderConfirmed, events.NewID(events.PrefixOrder), t0)
	r.apply(t, e)
	r.apply(t, e) // redelivered after a rebalance
	again := e
	again.EventID = events.NewID(events.PrefixEvent) // the same order under a new event ID
	r.apply(t, again)
	if testutil.ToFloat64(r.metrics.orderEvents.WithLabelValues("OrderConfirmed", "applied")) != 1 ||
		testutil.ToFloat64(r.metrics.orderEvents.WithLabelValues("OrderConfirmed", "duplicate")) != 1 ||
		testutil.ToFloat64(r.metrics.orderEvents.WithLabelValues("OrderConfirmed", "ignored")) != 1 {
		t.Fatal("want one applied, one duplicate, one ignored")
	}
	o := w.order(e.OrderID)
	if o.Status != statusPending || o.Ward != "shibuya" || o.Store != shibuya || o.Dropoff != dropoff ||
		!o.ConfirmedAt.Equal(t0) || o.ReadyAt == nil || o.StoreID != e.Order.Store.ID {
		t.Fatalf("order record: %+v", o)
	}
}

func TestConsumerSkipsUnusableRecords(t *testing.T) {
	w := newWorld(t)
	r := w.replica("")
	bad := orderEvent(events.OrderConfirmed, events.NewID(events.PrefixOrder), t0)
	bad.Order.Store.Lat = math.NaN()
	noStore := orderEvent(events.OrderConfirmed, events.NewID(events.PrefixOrder), t0)
	noStore.Order.Store.ID = ""
	unknown := orderEvent("OrderTeleported", events.NewID(events.PrefixOrder), t0)
	records := []*kgo.Record{
		{Topic: events.TopicOrders, Value: []byte("{not json")},
		record(t, noStore), record(t, unknown),
	}
	// NaN cannot be marshalled to JSON, so build that record by hand.
	records = append(records, &kgo.Record{Topic: events.TopicOrders,
		Value: []byte(`{"schema":"order.event.v1","event_id":"` + bad.EventID + `","type":"OrderConfirmed","order_id":"` +
			bad.OrderID + `","merchant_id":"` + bad.MerchantID + `","occurred_at":"2026-10-14T03:00:00Z",` +
			`"order":{"store":{"id":"str_01JABCDEFGHJKMNPQRSTVWXYZ0","lat":95,"lng":139.7,"ward":"shibuya"},` +
			`"dropoff":{"lat":35.6,"lng":139.7,"ward":"shibuya"},"promised_at":"2026-10-14T03:40:00Z","fee_yen":660}}`)})
	if err := r.consumer.Handle(context.Background(), records); err != nil {
		t.Fatal(err)
	}
	if n := testutil.ToFloat64(r.metrics.invalid.WithLabelValues(events.TopicOrders)); n != 3 {
		t.Fatalf("invalid records counted: %v", n)
	}
	if n := w.rdb.SCard(context.Background(), pendingKey(0)).Val(); n != 0 {
		t.Fatalf("%d unusable orders queued", n)
	}
}

func TestOrderLifecycleFreesTheCourier(t *testing.T) {
	w := newWorld(t)
	r := w.replica("")
	c := w.addCourier(north(shibuya, 300))
	id := w.confirm(r)
	r.matcher.Tick(context.Background())
	offerID := *w.prod.of(events.OfferSent)[0].OfferID
	if code, _ := post(t, r.url+"/v1/offers/"+offerID+"/accept", token(t, c)); code != http.StatusOK {
		t.Fatalf("accept: %d", code)
	}
	picked := orderEvent(events.OrderPickedUp, id, t0.Add(10*time.Minute))
	picked.Order.CourierID = &c
	r.apply(t, picked)
	if o := w.order(id); o.Status != statusPickedUp || o.CourierID != c {
		t.Fatalf("after pickup: %+v", o)
	}
	if w.rdb.Get(context.Background(), busyKey(c)).Val() != id {
		t.Fatal("courier must stay busy until delivery")
	}
	r.apply(t, orderEvent(events.OrderDelivered, id, t0.Add(30*time.Minute)))
	if o := w.order(id); o.Status != statusDone {
		t.Fatalf("after delivery: %+v", o)
	}
	if w.rdb.Exists(context.Background(), busyKey(c)).Val() != 0 {
		t.Fatal("a delivered order must free its courier")
	}
	// A late pickup (redelivered with a new event ID) does not move the order back.
	late := picked
	late.EventID = events.NewID(events.PrefixEvent)
	r.apply(t, late)
	if o := w.order(id); o.Status != statusDone {
		t.Fatalf("status moved backwards: %+v", o)
	}
}

func TestCancelBeforeConfirmLeavesATombstone(t *testing.T) {
	w := newWorld(t)
	r := w.replica("")
	id := events.NewID(events.PrefixOrder)
	r.apply(t, orderEvent(events.OrderCancelled, id, t0.Add(time.Minute)))
	r.apply(t, orderEvent(events.OrderConfirmed, id, t0)) // a replay delivers it late
	if o := w.order(id); o.Status != statusDone {
		t.Fatalf("a cancelled order came back: %+v", o)
	}
	if w.rdb.SCard(context.Background(), pendingKey(0)).Val() != 0 {
		t.Fatal("a cancelled order is queued")
	}
	// Pickup of an order dispatch never saw records who carries it.
	unseen, c := events.NewID(events.PrefixOrder), events.NewID(events.PrefixCourier)
	picked := orderEvent(events.OrderPickedUp, unseen, t0)
	picked.Order.CourierID = &c
	r.apply(t, picked)
	if w.rdb.Get(context.Background(), busyKey(c)).Val() != unseen {
		t.Fatal("courier not marked busy")
	}
}

func TestConsumerRetriesWhileRedisIsDown(t *testing.T) {
	w := newWorld(t)
	r := w.replica("")
	e := orderEvent(events.OrderConfirmed, events.NewID(events.PrefixOrder), t0)
	w.mr.SetError("LOADING")
	go func() {
		time.Sleep(300 * time.Millisecond)
		w.mr.SetError("")
	}()
	if err := r.consumer.Handle(context.Background(), []*kgo.Record{record(t, e)}); err != nil {
		t.Fatal(err)
	}
	if o := w.order(e.OrderID); o.Status != statusPending {
		t.Fatalf("not applied after Redis came back: %+v", o)
	}
	// Cancelled while Redis is down: Handle gives up without an error.
	w.mr.SetError("LOADING")
	defer w.mr.SetError("")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := r.consumer.Handle(ctx, []*kgo.Record{record(t, e)}); err == nil {
		t.Fatal("want the context error")
	}
}

func TestOwnership(t *testing.T) {
	o := NewOwnership()
	o.Assign([]int32{5, 1, 3})
	o.Revoke([]int32{3, 7})
	if got := o.Partitions(); !slices.Equal(got, []int32{1, 5}) {
		t.Fatalf("got %v", got)
	}
}
