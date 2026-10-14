package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/events"
	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/kit"
	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/state"
)

func newGuard(w *world, replay func(context.Context) error) (*Guard, *metrics) {
	m := newMetrics(prometheus.NewRegistry())
	return &Guard{store: NewStore(w.rdb, testStoreConfig()), replay: replay, interval: time.Hour, metrics: m,
		logger: discard(), now: w.clock.Now}, m
}

func (w *world) markFleetRebuilt(at time.Time) {
	w.t.Helper()
	if err := w.rdb.Set(context.Background(), state.RebuiltKey, at.UTC().Format(time.RFC3339), 0).Err(); err != nil {
		w.t.Fatal(err)
	}
}

func TestGuardWaitsForFleetStateThenReplays(t *testing.T) {
	w := newWorld(t)
	replays := 0
	var replayErr error
	g, m := newGuard(w, func(context.Context) error { replays++; return replayErr })
	ctx := context.Background()

	// First start on an empty Redis: wait for fleet-state's marker.
	g.step(ctx)
	if g.Dispatching() || replays != 0 || g.Ready(ctx) == nil || testutil.ToFloat64(m.paused) != 1 {
		t.Fatal("must not dispatch before fleet-state has rebuilt Redis")
	}
	w.markFleetRebuilt(t0.Add(-time.Hour))
	g.step(ctx)
	if !g.Dispatching() || replays != 1 || g.Ready(ctx) != nil || testutil.ToFloat64(m.recoveries) != 1 {
		t.Fatalf("dispatching=%v replays=%d", g.Dispatching(), replays)
	}
	w.clock.Advance(10 * time.Second)
	g.step(ctx) // sentinel present: nothing to do
	if replays != 1 {
		t.Fatal("replayed without a loss")
	}

	// Redis loses everything. fleet-state's marker comes back, but from before the loss.
	w.clock.Advance(10 * time.Second)
	w.mr.FlushAll()
	w.markFleetRebuilt(t0) // older than the last time the sentinel was seen
	g.step(ctx)
	if g.Dispatching() || replays != 1 {
		t.Fatal("must wait for a rebuild newer than the loss")
	}
	// The replay fails once, then works.
	w.markFleetRebuilt(w.clock.Now())
	replayErr = errors.New("kafka down")
	g.step(ctx)
	if g.Dispatching() || replays != 2 {
		t.Fatal("a failed replay must not resume dispatch")
	}
	replayErr = nil
	g.step(ctx)
	if !g.Dispatching() || replays != 3 || testutil.ToFloat64(m.paused) != 0 {
		t.Fatal("did not resume after the replay")
	}

	// Redis unreachable: keep the current state (readiness reports Redis separately).
	w.mr.SetError("LOADING")
	g.step(ctx)
	w.mr.SetError("")
	if !g.Dispatching() {
		t.Fatal("a Redis error is not a Redis loss")
	}
}

func TestGuardResumesWhenAnotherReplicaRecovered(t *testing.T) {
	w := newWorld(t)
	g, _ := newGuard(w, func(context.Context) error { t.Fatal("must not replay"); return nil })
	ctx := context.Background()
	g.step(ctx)
	if err := NewStore(w.rdb, testStoreConfig()).SetSentinel(ctx, t0); err != nil {
		t.Fatal(err)
	}
	g.step(ctx)
	if !g.Dispatching() {
		t.Fatal("another replica's recovery counts")
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- g.Run(runCtx) }()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func fakeKafka(t *testing.T) kit.KafkaConfig {
	t.Helper()
	c, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(3, events.TopicOrders, events.TopicDispatch))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return kit.KafkaConfig{Brokers: c.ListenAddrs(), ClientID: "test"}
}

// produce writes events keyed by order ID, stamped at the given time.
func produce(t *testing.T, cfg kit.KafkaConfig, at time.Time, evs ...any) {
	t.Helper()
	cl, err := cfg.NewClient()
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	for _, e := range evs {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		r := &kgo.Record{Topic: events.TopicOrders, Value: b, Timestamp: at}
		switch e := e.(type) {
		case events.OrderEvent:
			r.Key = []byte(e.OrderID)
		case events.DispatchEvent:
			r.Topic, r.Key = events.TopicDispatch, []byte(e.OrderID)
		case string:
			r.Topic, r.Value = events.TopicDispatch, []byte(e)
		}
		if err := cl.ProduceSync(context.Background(), r).FirstErr(); err != nil {
			t.Fatal(err)
		}
	}
}

func dispatchEvent(typ events.DispatchEventType, orderID, courierID string, at time.Time) events.DispatchEvent {
	e := newEvent(typ, orderID, "shibuya", at)
	if courierID != "" {
		offer := events.NewID(events.PrefixOffer)
		e.CourierID, e.OfferID = &courierID, &offer
	}
	return e
}

func TestReplayerRestoresDispatchStateFromKafka(t *testing.T) {
	cfg := fakeKafka(t)
	w := newWorld(t)
	r := w.replica("")
	now := time.Now()
	pending, assigned, cancelled, old := events.NewID(events.PrefixOrder), events.NewID(events.PrefixOrder),
		events.NewID(events.PrefixOrder), events.NewID(events.PrefixOrder)
	decliner, carrier := events.NewID(events.PrefixCourier), events.NewID(events.PrefixCourier)

	produce(t, cfg, now.Add(-2*time.Hour), orderEvent(events.OrderConfirmed, old, now.Add(-2*time.Hour)))
	recent := now.Add(-5 * time.Minute)
	// dispatch.events first: across topics the replay sees events in any order.
	produce(t, cfg, recent,
		dispatchEvent(events.OfferSent, pending, decliner, recent),
		dispatchEvent(events.OfferDeclined, pending, decliner, recent),
		dispatchEvent(events.NoCourierAvailable, pending, "", recent),
		dispatchEvent(events.CourierAssigned, assigned, carrier, recent),
		"not an event",
	)
	produce(t, cfg, recent,
		orderEvent(events.OrderConfirmed, pending, recent),
		orderEvent(events.OrderConfirmed, assigned, recent),
		orderEvent(events.OrderConfirmed, cancelled, recent),
		orderEvent(events.OrderCancelled, cancelled, recent),
	)

	rp := &Replayer{kafka: cfg, ordersTopic: events.TopicOrders, dispatchTopic: events.TopicDispatch,
		window: 30 * time.Minute, consumer: r.consumer, store: r.store, metrics: r.metrics, logger: discard(), now: time.Now}
	ctx := context.Background()
	if err := rp.Run(ctx); err != nil {
		t.Fatal(err)
	}

	if o := w.order(pending); o.Status != statusPending || o.Attempts != 1 {
		t.Fatalf("pending order: %+v", o)
	}
	if w.rdb.HGet(ctx, orderKey(pending), "sli").Val() != "1" {
		t.Fatal("the SLI was observed before the loss and must not be observed again")
	}
	if !w.rdb.SIsMember(ctx, triedKey(pending), decliner).Val() {
		t.Fatal("the courier who declined must not be asked again")
	}
	if o := w.order(assigned); o.Status != statusAssigned || o.CourierID != carrier {
		t.Fatalf("assigned order: %+v", o)
	}
	if w.rdb.Get(ctx, busyKey(carrier)).Val() != assigned {
		t.Fatal("the carrying courier must be busy again")
	}
	if o := w.order(cancelled); o.Status != statusDone {
		t.Fatalf("cancelled order: %+v", o)
	}
	if o := w.order(old); o.Status != "" {
		t.Fatalf("an order outside RECOVERY_WINDOW was restored: %+v", o)
	}
	queued, err := r.store.PendingOrders(ctx, []int32{0, 1, 2})
	if err != nil || len(queued) != 1 || queued[0].ID != pending {
		t.Fatalf("pending orders after the replay: %+v (%v)", queued, err)
	}
	if testutil.ToFloat64(r.metrics.invalid.WithLabelValues(events.TopicDispatch)) != 1 {
		t.Fatal("invalid dispatch record not counted")
	}
	// Replaying again changes nothing, and no courier was notified during the replay.
	if err := rp.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if o := w.order(pending); o.Attempts != 1 {
		t.Fatalf("a second replay applied events twice: %+v", o)
	}
}

func TestReplayerWithNothingToReplayOrKafkaDown(t *testing.T) {
	w := newWorld(t)
	r := w.replica("")
	rp := &Replayer{kafka: fakeKafka(t), ordersTopic: events.TopicOrders, dispatchTopic: events.TopicDispatch,
		window: time.Minute, consumer: r.consumer, store: r.store, metrics: r.metrics, logger: discard(), now: time.Now}
	if err := rp.Run(context.Background()); err != nil {
		t.Fatalf("empty topics: %v", err)
	}
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	rp.kafka = kit.KafkaConfig{Brokers: []string{l.Addr().String()}}
	_ = l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := rp.Run(ctx); err == nil {
		t.Fatal("want an error while Kafka is down")
	}
}
