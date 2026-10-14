package dispatch

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/events"
	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/kit"
	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/state"
)

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

func waitLong(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRunEndToEnd(t *testing.T) {
	cfg := fakeKafka(t)
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer func() { _ = rdb.Close() }()
	ctx := context.Background()
	rdb.Set(ctx, state.RebuiltKey, time.Now().UTC().Format(time.RFC3339), 0) // fleet-state has run
	courier := events.NewID(events.PrefixCourier)
	near := north(shibuya, 300)
	rdb.GeoAdd(ctx, state.GeoAvailableKey, &redis.GeoLocation{Name: courier, Longitude: near.Lng, Latitude: near.Lat})

	port := freePort(t)
	env := map[string]string{
		"HTTP_PORT": port, "KAFKA_BROKERS": cfg.Brokers[0], "REDIS_URL": "redis://" + mr.Addr() + "/0",
		"COURIER_TOKEN_SECRET": secret, "ROUTING_URL": "none", "SHUTDOWN_TIMEOUT": "5s", "MATCH_INTERVAL": "50ms",
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- Run(runCtx, func(k string) string { return env[k] }, discard(), "test") }()
	url := "http://127.0.0.1:" + port
	waitLong(t, "readiness", func() bool { return kit.Probe(ctx, url+"/readyz") == nil })
	if mr.Exists(sentinelKey) == false {
		t.Fatal("the first start must set the sentinel after its replay")
	}

	_, ch := stream(t, url, token(t, courier))
	orderID := events.NewID(events.PrefixOrder)
	produce(t, cfg, time.Now(), orderEvent(events.OrderConfirmed, orderID, time.Now().UTC()))
	var offer offerData
	waitLong(t, "the offer", func() bool {
		select {
		case ev := <-ch:
			return ev.name == eventOffer && json.Unmarshal([]byte(ev.data), &offer) == nil
		default:
			return false
		}
	})
	if offer.OrderID != orderID {
		t.Fatalf("offer: %+v", offer)
	}
	if code, body := post(t, url+"/v1/offers/"+offer.OfferID+"/accept", token(t, courier)); code != http.StatusOK {
		t.Fatalf("accept: %d %v", code, body)
	}

	// CourierAssigned reaches dispatch.events in Kafka.
	cl, err := cfg.NewClient(kgo.ConsumeTopics(events.TopicDispatch), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	seen := map[events.DispatchEventType]bool{}
	waitLong(t, "CourierAssigned in Kafka", func() bool {
		pollCtx, stop := context.WithTimeout(ctx, 200*time.Millisecond)
		defer stop()
		cl.PollFetches(pollCtx).EachRecord(func(r *kgo.Record) {
			if e, err := events.DecodeDispatchEvent(r.Value); err == nil && e.OrderID == orderID && string(r.Key) == orderID {
				seen[e.Type] = true
			}
		})
		return seen[events.CourierAssigned] && seen[events.OfferSent] && seen[events.OfferAccepted]
	})
	metrics := get(t, url+"/metrics")
	if !strings.Contains(metrics, `dispatch_assignment_duration_seconds_count{outcome="assigned"} 1`) {
		t.Fatal("SLI histogram not exported")
	}

	http.DefaultClient.CloseIdleConnections()
	cancel()
	for range ch { // shutdown ends the open stream
	}
	if err := <-done; err != nil {
		t.Fatalf("Run returned %v", err)
	}
}

func get(t *testing.T, url string) string {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func TestRunRejectsBadConfiguration(t *testing.T) {
	good := map[string]string{"COURIER_TOKEN_SECRET": secret, "ROUTING_URL": "none"}
	with := func(kv ...string) map[string]string {
		m := map[string]string{}
		for k, v := range good {
			m[k] = v
		}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	for name, env := range map[string]map[string]string{
		"missing secret":      with("COURIER_TOKEN_SECRET", ""),
		"short secret":        with("COURIER_TOKEN_SECRET", "short"),
		"missing routing":     with("ROUTING_URL", ""),
		"bad routing":         with("ROUTING_URL", "ftp://osrm"),
		"zero offer ttl":      with("OFFER_TTL", "0s"),
		"radius order":        with("SEARCH_RADIUS_M", "5000", "SEARCH_MAX_RADIUS_M", "1000"),
		"eta budget":          with("ETA_ROUTING_TIMEOUT", "300ms", "ETA_BUDGET", "200ms"),
		"bad redis url":       with("REDIS_URL", "nope://x"),
		"bad attempts number": with("MAX_OFFER_ATTEMPTS", "zero"),
	} {
		t.Run(name, func(t *testing.T) {
			err := Run(context.Background(), func(k string) string { return env[k] }, discard(), "test")
			if err == nil || !strings.Contains(err.Error(), "configuration") {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	env := kit.NewEnv(func(k string) string { return map[string]string{"ROUTING_URL": "http://routing:5000"}[k] })
	cfg := LoadConfig(env)
	if env.Err() != nil || cfg.RoutingURL != "http://routing:5000" || cfg.Group != "dispatch" ||
		cfg.Match.OfferTTL != 30*time.Second || cfg.Match.MaxAttempts != 3 || cfg.API.ETABudget != 250*time.Millisecond ||
		cfg.Store.OfferTTL != cfg.Match.OfferTTL || cfg.RecoveryWindow != 30*time.Minute {
		t.Fatalf("got %+v (%v)", cfg, env.Err())
	}
}

func TestWithHTTPPort(t *testing.T) {
	both := WithHTTPPort(func(k string) string { return map[string]string{"HTTP_PORT": "9001", "PORT": "9002", "X": "y"}[k] })
	onlyPort := WithHTTPPort(func(k string) string { return map[string]string{"PORT": "9002"}[k] })
	if both("PORT") != "9001" || onlyPort("PORT") != "9002" || both("X") != "y" {
		t.Fatal("HTTP_PORT must win, PORT is the fallback, other keys pass through")
	}
}

func TestRunStopsCleanlyWhileDependenciesAreDown(t *testing.T) {
	dead := func() string {
		l, _ := net.Listen("tcp", "127.0.0.1:0")
		defer func() { _ = l.Close() }()
		return l.Addr().String()
	}
	port := freePort(t)
	env := map[string]string{"HTTP_PORT": port, "KAFKA_BROKERS": dead(), "REDIS_URL": "redis://" + dead() + "/0",
		"COURIER_TOKEN_SECRET": secret, "ROUTING_URL": "none", "SHUTDOWN_TIMEOUT": "20s"}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, func(k string) string { return env[k] }, discard(), "test") }()
	url := "http://127.0.0.1:" + port
	waitFor(t, "liveness", func() bool { return kit.Probe(context.Background(), url+"/healthz") == nil })
	if kit.Probe(context.Background(), url+"/readyz") == nil {
		t.Fatal("must not be ready without Redis")
	}
	time.Sleep(300 * time.Millisecond)
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

func TestSampleGauges(t *testing.T) {
	w := newWorld(t)
	w.addCourier(shibuya)
	m := newMetrics(prometheus.NewRegistry())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sampleGauges(ctx, NewStore(w.rdb, testStoreConfig()), m, time.Millisecond) }()
	waitFor(t, "the courier gauge", func() bool {
		return testutil.ToFloat64(m.couriers.WithLabelValues("available")) == 1 &&
			testutil.ToFloat64(m.couriers.WithLabelValues("online")) == 1
	})
	w.mr.SetError("LOADING")
	time.Sleep(5 * time.Millisecond)
	w.mr.SetError("")
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestHubDropsWhatItCannotDeliver(t *testing.T) {
	w := newWorld(t)
	r := w.replica("")
	s, err := r.hub.Register("cou_x")
	if err != nil {
		t.Fatal(err)
	}
	w.rdb.Publish(context.Background(), offersChannel, "not json")
	w.rdb.Publish(context.Background(), offersChannel, `{"event":"offer"}`) // no courier
	for range 20 {
		r.hub.Deliver(message{CourierID: "cou_x", Event: eventOffer, Data: []byte(`{}`)})
	}
	if len(s.ch) != cap(s.ch) {
		t.Fatalf("queue holds %d of %d", len(s.ch), cap(s.ch))
	}
	r.hub.Unregister(s)
	r.hub.Unregister(s) // twice is harmless
	if r.hub.streamCount() != 0 || testutil.ToFloat64(r.metrics.streams) != 0 {
		t.Fatal("stream not removed")
	}
	// Following a dead Redis gives up cleanly on cancel.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := NewHub(1, r.metrics).Follow(ctx, deadRedis(t), discard()); err != nil {
		t.Fatal(err)
	}
}
