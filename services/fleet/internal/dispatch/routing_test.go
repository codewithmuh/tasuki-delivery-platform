package dispatch

import (
	"context"
	"math"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/events"
)

func TestFallbackEstimate(t *testing.T) {
	if d := HaversineMetres(Point{35, 139}, Point{36, 139}); math.Abs(d-111_195) > 100 {
		t.Fatalf("one degree of latitude = %.0f m", d)
	}
	// 1 km straight line x 1.35 at 15 km/h = 324 s, as orders computes it.
	if s := FallbackSeconds(1000); s != 324 {
		t.Fatalf("FallbackSeconds(1000) = %d", s)
	}
}

func newTestRouter(url string) (*Router, *metrics, *clock) {
	m := newMetrics(prometheus.NewRegistry())
	c := &clock{t: t0}
	r := NewRouter(url, m)
	r.now = c.Now
	return r, m, c
}

var twoPoints = []Point{shibuya, dropoff}

func TestRouterBreakerSkipsAFailingOSRM(t *testing.T) {
	var healthy atomic.Bool
	url, hits := osrmServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !healthy.Load() {
			http.Error(w, `{"code":"Error"}`, http.StatusBadGateway)
			return
		}
		durations([]any{10, 20})(w, r)
	})
	r, m, c := newTestRouter(url)
	ctx := context.Background()
	for range 3 {
		if _, src := r.Durations(ctx, twoPoints[:1], twoPoints, time.Second); src != sourceFallback {
			t.Fatal("want the fallback")
		}
	}
	r.Durations(ctx, twoPoints[:1], twoPoints, time.Second) // breaker open: OSRM not called
	if hits.Load() != 3 || testutil.ToFloat64(m.etaRequests.WithLabelValues(sourceFallback)) != 4 {
		t.Fatalf("hits=%d", hits.Load())
	}
	c.Advance(breakerOpenFor)
	r.Durations(ctx, twoPoints[:1], twoPoints, time.Second) // one trial call, fails, opens again
	r.Durations(ctx, twoPoints[:1], twoPoints, time.Second)
	if hits.Load() != 4 {
		t.Fatalf("hits=%d, want one trial call", hits.Load())
	}
	healthy.Store(true)
	c.Advance(breakerOpenFor)
	d, src := r.Durations(ctx, twoPoints[:1], twoPoints, time.Second)
	if src != sourceOSRM || d[0][0] != 10 || d[0][1] != 20 {
		t.Fatalf("got %v from %s", d, src)
	}
	r.Durations(ctx, twoPoints[:1], twoPoints, time.Second)
	if hits.Load() != 6 {
		t.Fatalf("a healthy OSRM must be called every time; hits=%d", hits.Load())
	}
}

func TestRouterFillsGapsAndRejectsBadAnswers(t *testing.T) {
	ctx := context.Background()
	fallbackToDropoff := FallbackSeconds(HaversineMetres(shibuya, dropoff))
	// A pair OSRM cannot route (null) gets the estimate; the call still counts as OSRM.
	url, _ := osrmServer(t, durations([]any{0, nil}))
	r, _, _ := newTestRouter(url)
	if d, src := r.Durations(ctx, twoPoints[:1], twoPoints, time.Second); src != sourceOSRM || d[0][1] != fallbackToDropoff {
		t.Fatalf("got %v from %s", d, src)
	}
	for name, h := range map[string]http.HandlerFunc{
		"no route":     func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"code":"NoTable"}`)) },
		"wrong rows":   durations([]any{1, 2}, []any{3, 4}),
		"wrong cols":   durations([]any{1}),
		"not json":     func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) },
		"server error": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) },
	} {
		url, _ := osrmServer(t, h)
		r, _, _ := newTestRouter(url)
		if d, src := r.Durations(ctx, twoPoints[:1], twoPoints, time.Second); src != sourceFallback || d[0][1] != fallbackToDropoff {
			t.Errorf("%s: got %v from %s", name, d, src)
		}
	}
	// Disabled, unreachable, or nothing to ask.
	for _, r := range []*Router{NewRouter("", newMetrics(prometheus.NewRegistry())), NewRouter("http://127.0.0.1:1", newMetrics(prometheus.NewRegistry()))} {
		if _, src := r.Durations(ctx, twoPoints[:1], twoPoints, time.Second); src != sourceFallback {
			t.Error("want the fallback")
		}
	}
	if d, src := NewRouter(url, newMetrics(prometheus.NewRegistry())).Durations(ctx, nil, twoPoints, time.Second); len(d) != 0 || src != sourceFallback {
		t.Error("no sources: nothing to ask")
	}
	bad := NewRouter("http://bad host", newMetrics(prometheus.NewRegistry()))
	if _, src := bad.Durations(ctx, twoPoints[:1], twoPoints, time.Second); src != sourceFallback {
		t.Error("an unusable URL falls back")
	}
}

func TestOutboxResendsWhatKafkaDidNotAcknowledge(t *testing.T) {
	w := newWorld(t)
	r := w.replica("")
	w.addCourier(north(shibuya, 300))
	w.confirm(r)
	w.prod.setFail(kgo.ErrRecordTimeout)
	r.matcher.Tick(context.Background()) // OfferSent is written to the outbox, but Kafka fails it
	if testutil.ToFloat64(r.metrics.publishFails) != 1 || w.rdb.ZCard(context.Background(), outboxKey).Val() != 1 {
		t.Fatal("the event must stay in the outbox")
	}
	w.prod.setFail(nil)
	r.outbox.relay(context.Background()) // within the lease: not yet
	if len(w.prod.of(events.OfferSent)) != 0 {
		t.Fatal("resent before the lease ran out")
	}
	// An orphaned entry without a body is cleaned up by the claim.
	w.rdb.ZAdd(context.Background(), outboxKey, redis.Z{Score: 0, Member: "evt_orphan"})
	w.clock.Advance(11 * time.Second)
	r.outbox.relay(context.Background())
	if sent := w.prod.of(events.OfferSent); len(sent) != 1 {
		t.Fatalf("relay sent %d events", len(sent))
	}
	if n := w.rdb.ZCard(context.Background(), outboxKey).Val(); n != 0 {
		t.Fatalf("%d entries left in the outbox", n)
	}

	ctx, cancel := context.WithCancel(context.Background())
	r.outbox.interval = time.Millisecond
	done := make(chan error, 1)
	go func() { done <- r.outbox.Run(ctx) }()
	time.Sleep(10 * time.Millisecond)
	w.mr.SetError("LOADING") // relay errors are logged, not fatal
	time.Sleep(10 * time.Millisecond)
	w.mr.SetError("")
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := r.store.Acked(context.Background()); err != nil {
		t.Fatal(err)
	}
	w.mr.SetError("LOADING")
	defer w.mr.SetError("")
	if r.store.Acked(context.Background(), "evt_x") == nil {
		t.Fatal("want an error from Redis")
	}
	if _, err := r.store.ClaimDue(context.Background(), w.clock.Now(), 10); err == nil {
		t.Fatal("want an error from Redis")
	}
}
