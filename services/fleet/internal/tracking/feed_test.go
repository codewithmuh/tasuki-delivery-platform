package tracking

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/events"
	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/kit"
)

func fakeKafka(t *testing.T) kit.KafkaConfig {
	t.Helper()
	c, err := kfake.NewCluster(kfake.NumBrokers(1),
		kfake.SeedTopics(3, events.TopicCourierLocations, events.TopicOrders, events.TopicDispatch))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return kit.KafkaConfig{Brokers: c.ListenAddrs(), ClientID: "test"}
}

// produce publishes each value to topic, keyed the way the real producers key them.
func produce(t *testing.T, cfg kit.KafkaConfig, topic string, values ...any) {
	t.Helper()
	cl, err := cfg.NewClient()
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	for _, v := range values {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var key string
		switch e := v.(type) {
		case events.OrderEvent:
			key = e.OrderID
		case events.DispatchEvent:
			key = e.OrderID
		case events.CourierLocation:
			key = e.CourierID
		}
		if err := cl.ProduceSync(context.Background(), &kgo.Record{Topic: topic, Key: []byte(key), Value: b}).FirstErr(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOrderFeedReplaysThenFollows(t *testing.T) {
	cfg := fakeKafka(t)
	delivered, active := events.NewID(events.PrefixOrder), events.NewID(events.PrefixOrder)
	courier := events.NewID(events.PrefixCourier)
	produce(t, cfg, events.TopicOrders,
		orderEvent(delivered, events.OrderConfirmed, t0),
		orderEvent(delivered, events.OrderDelivered, t0.Add(30*time.Minute)),
		orderEvent(active, events.OrderConfirmed, t0),
		"not an event",
	)
	produce(t, cfg, events.TopicDispatch, assigned(active, courier, t0.Add(time.Minute), 60, 60), "garbage")

	h, orders, m := newTestHub(HubConfig{})
	feed := &OrderFeed{kafka: cfg, ordersTopic: events.TopicOrders, dispatchTopic: events.TopicDispatch,
		orders: orders, hub: h, metrics: m, logger: discard()}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- feed.Run(ctx) }()
	waitFor(t, "the replay to catch up", feed.Ready)

	if v, _ := orders.Get(delivered); v.Status != StatusDelivered {
		t.Fatalf("delivered order: %+v", v)
	}
	if v, _ := orders.Get(active); v.Status != StatusAssigned || v.CourierID != courier {
		t.Fatalf("active order: %+v", v)
	}
	if testutil.ToFloat64(m.invalid.WithLabelValues(events.TopicOrders)) != 1 ||
		testutil.ToFloat64(m.invalid.WithLabelValues(events.TopicDispatch)) != 1 {
		t.Fatal("invalid records not counted")
	}

	// Live: a viewer of the active order sees its pickup.
	v, _ := h.SubscribeOrder(active)
	produce(t, cfg, events.TopicOrders, orderEvent(active, events.OrderPickedUp, t0.Add(10*time.Minute)))
	waitFor(t, "the live status", func() bool { return len(v.ch) > 0 })
	if m := <-v.ch; !strings.Contains(string(m.frame), `"picked_up"`) {
		t.Fatalf("got %s", m.frame)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestOrderFeedWithMissingTopicsIsReadyAtOnce(t *testing.T) {
	c, err := kfake.NewCluster(kfake.NumBrokers(1))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	h, orders, m := newTestHub(HubConfig{})
	feed := &OrderFeed{kafka: kit.KafkaConfig{Brokers: c.ListenAddrs()}, ordersTopic: "nope.orders",
		dispatchTopic: "nope.dispatch", orders: orders, hub: h, metrics: m, logger: discard()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = feed.Run(ctx) }()
	waitFor(t, "readiness", feed.Ready)
}

func TestOrderFeedKeepsRetryingWhileKafkaIsDown(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	_ = l.Close()
	h, orders, m := newTestHub(HubConfig{})
	feed := &OrderFeed{kafka: kit.KafkaConfig{Brokers: []string{addr}}, ordersTopic: events.TopicOrders,
		dispatchTopic: events.TopicDispatch, orders: orders, hub: h, metrics: m, logger: discard()}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := feed.Run(ctx); err != nil || feed.Ready() {
		t.Fatalf("err=%v ready=%v", err, feed.Ready())
	}
}

func TestFollowLocations(t *testing.T) {
	cfg := fakeKafka(t)
	h, _, m := newTestHub(HubConfig{})
	c := events.NewID(events.PrefixCourier)
	recent := location(c, shibuya, time.Now().UTC())
	produce(t, cfg, events.TopicCourierLocations, recent, "nonsense")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- followLocations(ctx, cfg, events.TopicCourierLocations, time.Minute, h, m, discard()) }()
	waitFor(t, "the courier", func() bool { return h.Couriers() == 1 })
	waitFor(t, "the invalid record", func() bool {
		return testutil.ToFloat64(m.invalid.WithLabelValues(events.TopicCourierLocations)) == 1
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestMaintainSweepsAndEvicts(t *testing.T) {
	h, orders, m := newTestHub(HubConfig{})
	h.OnLocation(location(events.NewID(events.PrefixCourier), shibuya, time.Now().Add(-time.Hour)))
	orders.ApplyOrderEvent(orderEvent(events.NewID(events.PrefixOrder), events.OrderConfirmed, time.Now().Add(-72*time.Hour)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- maintain(ctx, h, orders, m, time.Minute, 48*time.Hour, 10*time.Millisecond) }()
	waitFor(t, "sweep and eviction", func() bool { return h.Couriers() == 0 && orders.Len() == 0 })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// --- wiring ---

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

func TestRunEndToEnd(t *testing.T) {
	cfg := fakeKafka(t)
	port := freePort(t)
	env := map[string]string{
		"PORT": port, "KAFKA_BROKERS": cfg.Brokers[0], "TRACKING_TOKEN_SECRET": secret,
		"FLEET_STREAM_PUBLIC": "true", "SHUTDOWN_TIMEOUT": "5s",
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, func(k string) string { return env[k] }, discard(), "test") }()
	url := "http://127.0.0.1:" + port
	waitFor(t, "readiness", func() bool { return kit.Probe(context.Background(), url+"/readyz") == nil })

	_, ch := open(t, url+"/v1/fleet/stream", nil)
	c := events.NewID(events.PrefixCourier)
	produce(t, cfg, events.TopicCourierLocations, location(c, shibuya, time.Now().UTC()))
	if ev := next(t, ch, true); ev.name != "courier" || !strings.Contains(ev.data, c) {
		t.Fatalf("got %+v", ev)
	}

	http.DefaultClient.CloseIdleConnections()
	cancel()
	expectClosed(t, ch) // shutdown ends open streams instead of waiting for them
	if err := <-done; err != nil {
		t.Fatalf("Run returned %v", err)
	}
}

func TestRunRejectsBadConfiguration(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"missing secret": {},
		"short secret":   {"TRACKING_TOKEN_SECRET": "short"},
		"zero heartbeat": {"TRACKING_TOKEN_SECRET": secret, "TRACKING_HEARTBEAT": "0s"},
	} {
		t.Run(name, func(t *testing.T) {
			err := Run(context.Background(), func(k string) string { return env[k] }, discard(), "test")
			if err == nil || !strings.Contains(err.Error(), "configuration") {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestRunStopsCleanlyWhileKafkaIsDown(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := l.Addr().String()
	_ = l.Close()
	port := freePort(t)
	env := map[string]string{"PORT": port, "KAFKA_BROKERS": dead, "TRACKING_TOKEN_SECRET": secret, "SHUTDOWN_TIMEOUT": "20s"}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, func(k string) string { return env[k] }, discard(), "test") }()
	url := "http://127.0.0.1:" + port
	waitFor(t, "liveness", func() bool { return kit.Probe(context.Background(), url+"/healthz") == nil })
	if kit.Probe(context.Background(), url+"/readyz") == nil {
		t.Fatal("must not be ready before the order replay")
	}
	time.Sleep(500 * time.Millisecond)
	http.DefaultClient.CloseIdleConnections()
	start := time.Now()
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("shutdown took %v", took)
	}
}
