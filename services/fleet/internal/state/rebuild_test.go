package state

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/twmb/franz-go/pkg/kfake"

	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/events"
	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/history"
	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/kit"
)

func fakeKafka(t *testing.T) kit.KafkaConfig {
	t.Helper()
	c, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(3, events.TopicCourierLocations))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return kit.KafkaConfig{Brokers: c.ListenAddrs(), ClientID: "test"}
}

// produceAt writes points with the given Kafka timestamps (fleet-ingest uses received_at).
func produceAt(t *testing.T, cfg kit.KafkaConfig, points ...events.CourierLocation) {
	t.Helper()
	cl, err := cfg.NewClient()
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	for _, p := range points {
		if err := cl.ProduceSync(context.Background(), record(t, p)).FirstErr(); err != nil {
			t.Fatal(err)
		}
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func newRebuilder(cfg kit.KafkaConfig, s *Store, window time.Duration) *Rebuilder {
	m := newMetrics(prometheus.NewRegistry())
	return &Rebuilder{
		kafka: cfg, topic: events.TopicCourierLocations, window: window, store: s, logger: discard(), metrics: m,
		now:     time.Now,
		proc:    &Processor{store: s, sink: history.Discard{}, logger: discard(), metrics: m, now: time.Now},
		trigger: make(chan struct{}, 1),
	}
}

func TestRebuildReplaysOnlyTheWindow(t *testing.T) {
	cfg := fakeKafka(t)
	mr, _, s := newStore(t)
	now := time.Now().UTC()
	old := loc(events.NewID(events.PrefixCourier), now.Add(-10*time.Minute), events.CourierIdle)
	old.ReceivedAt = now.Add(-10 * time.Minute)
	recent := make([]events.CourierLocation, 5)
	for i := range recent {
		recent[i] = loc(events.NewID(events.PrefixCourier), now.Add(-time.Minute), events.CourierIdle)
		recent[i].ReceivedAt = now.Add(-time.Minute)
	}
	produceAt(t, cfg, append([]events.CourierLocation{old}, recent...)...)

	r := newRebuilder(cfg, s, 5*time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	waitFor(t, "first rebuild", r.Ready)
	if mr.Exists(CourierKeyPrefix + old.CourierID) {
		t.Fatal("a point older than the window was replayed")
	}
	for _, p := range recent {
		if !mr.Exists(CourierKeyPrefix + p.CourierID) {
			t.Fatalf("courier %s missing after rebuild", p.CourierID)
		}
	}
	if !mr.Exists(RebuiltKey) {
		t.Fatal("rebuild marker not set")
	}

	// Redis loses everything; a trigger rebuilds it.
	mr.FlushAll()
	r.Trigger()
	r.Trigger() // a second request while one is pending is absorbed
	waitFor(t, "second rebuild", func() bool { return r.Ready() && mr.Exists(RebuiltKey) })
	if !mr.Exists(CourierKeyPrefix + recent[0].CourierID) {
		t.Fatal("the second rebuild did not restore couriers")
	}
	if got := testutil.ToFloat64(r.metrics.rebuilds); got < 2 {
		t.Fatalf("rebuilds = %v", got)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRebuildWithAnEmptyTopicOrNoWindow(t *testing.T) {
	cfg := fakeKafka(t)
	for _, window := range []time.Duration{time.Minute, 0} {
		mr, _, s := newStore(t)
		if err := newRebuilder(cfg, s, window).rebuild(context.Background()); err != nil {
			t.Fatalf("window %v: %v", window, err)
		}
		if !mr.Exists(RebuiltKey) {
			t.Fatalf("window %v: marker not set", window)
		}
	}
}

func TestRebuildFailsWhileKafkaIsDown(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	_ = l.Close()
	_, _, s := newStore(t)
	r := newRebuilder(kit.KafkaConfig{Brokers: []string{addr}}, s, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := r.rebuild(ctx); err == nil {
		t.Fatal("expected an error")
	}
	if err := r.Run(ctx); err != nil || r.Ready() {
		t.Fatalf("Run must keep retrying and return nil on cancel: %v ready=%v", err, r.Ready())
	}
}

func TestSweeperTriggersARebuildWhenRedisWasEmptied(t *testing.T) {
	mr, _, s := newStore(t)
	r := newRebuilder(kit.KafkaConfig{}, s, 0)
	sw := &Sweeper{store: s, rebuilder: r, interval: time.Hour, seenWindow: time.Minute, logger: discard(),
		metrics: r.metrics, now: func() time.Time { return t0.Add(2 * time.Minute) }}
	mustApply(t, s, loc(events.NewID(events.PrefixCourier), t0, events.CourierIdle))

	sw.tick(context.Background()) // rebuilder not ready yet: sweep only
	if len(r.trigger) != 0 || testutil.ToFloat64(r.metrics.swept) != 1 {
		t.Fatalf("trigger=%d swept=%v", len(r.trigger), testutil.ToFloat64(r.metrics.swept))
	}
	r.ready.Store(true)
	sw.tick(context.Background()) // ready, but the marker is missing
	if len(r.trigger) != 1 {
		t.Fatal("expected a rebuild request")
	}
	mr.Close()
	sw.tick(context.Background()) // Redis down: logged, no panic
}

func TestSweeperRunStopsOnCancel(t *testing.T) {
	_, _, s := newStore(t)
	r := newRebuilder(kit.KafkaConfig{}, s, 0)
	sw := &Sweeper{store: s, rebuilder: r, interval: 10 * time.Millisecond, seenWindow: time.Minute,
		logger: discard(), metrics: r.metrics, now: time.Now}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := sw.Run(ctx); err != nil {
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
	return itoa(l.Addr().(*net.TCPAddr).Port)
}

func TestRunEndToEnd(t *testing.T) {
	cfg := fakeKafka(t)
	mr, _, _ := newStore(t)
	histDir := t.TempDir()
	port := freePort(t)
	env := map[string]string{
		"PORT": port, "KAFKA_BROKERS": cfg.Brokers[0], "REDIS_URL": "redis://" + mr.Addr() + "/0",
		"HISTORY_DIR": histDir, "SWEEP_INTERVAL": "50ms", "SHUTDOWN_TIMEOUT": "5s",
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, func(k string) string { return env[k] }, discard(), "test") }()

	waitFor(t, "readiness", func() bool { return kit.Probe(context.Background(), "http://127.0.0.1:"+port+"/readyz") == nil })
	now := time.Now().UTC()
	p := loc(events.NewID(events.PrefixCourier), now, events.CourierIdle)
	p.ReceivedAt = now
	produceAt(t, cfg, p)
	waitFor(t, "the point in Redis", func() bool { return mr.Exists(CourierKeyPrefix + p.CourierID) })
	if !slices.Contains(members(t, mr, GeoAvailableKey), p.CourierID) {
		t.Fatal("courier not in the available geo set")
	}

	http.DefaultClient.CloseIdleConnections()
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned %v", err)
	}
	files, _ := filepath.Glob(filepath.Join(histDir, "locations", "*", "*", "part-*.ndjson.gz"))
	if len(files) != 1 {
		t.Fatalf("history files after shutdown: %v", files)
	}
}

func TestRunRejectsBadConfiguration(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string]map[string]string{
		"bad redis url":     {"REDIS_URL": "http://nope"},
		"zero sweep":        {"SWEEP_INTERVAL": "0s"},
		"bad duration":      {"REBUILD_WINDOW": "a while"},
		"history on a file": {"HISTORY_DIR": filepath.Join(blocker, "history")},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			if err := Run(context.Background(), func(k string) string { return env[k] }, discard(), "test"); err == nil {
				t.Fatal("expected an error")
			} else if name != "history on a file" && !strings.Contains(err.Error(), "configuration") {
				t.Fatalf("got %v", err)
			}
		})
	}
}

// With Kafka and Redis both down the service still starts, answers /healthz, stays unready, and
// a SIGTERM still exits cleanly and quickly (the CI smoke test relies on this).
func TestRunStopsCleanlyWhileDependenciesAreDown(t *testing.T) {
	dead := func() string {
		l, _ := net.Listen("tcp", "127.0.0.1:0")
		defer func() { _ = l.Close() }()
		return l.Addr().String()
	}
	port := freePort(t)
	env := map[string]string{"PORT": port, "KAFKA_BROKERS": dead(), "REDIS_URL": "redis://" + dead() + "/0", "SHUTDOWN_TIMEOUT": "20s"}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, func(k string) string { return env[k] }, discard(), "test") }()
	url := "http://127.0.0.1:" + port
	waitFor(t, "liveness", func() bool { return kit.Probe(context.Background(), url+"/healthz") == nil })
	if kit.Probe(context.Background(), url+"/readyz") == nil {
		t.Fatal("must not be ready without Kafka and Redis")
	}
	time.Sleep(500 * time.Millisecond) // let the workers start retrying
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
