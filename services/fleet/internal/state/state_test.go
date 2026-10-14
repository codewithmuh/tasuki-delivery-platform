package state

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/events"
	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/history"
)

var t0 = time.Date(2026, 10, 14, 3, 0, 0, 0, time.UTC)

func discard() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func newStore(t *testing.T) (*miniredis.Miniredis, *redis.Client, *Store) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, rdb, NewStore(rdb, 10*time.Minute)
}

func loc(courier string, deviceTime time.Time, status events.CourierStatus) events.CourierLocation {
	return events.CourierLocation{
		Schema: events.SchemaCourierLocation, EventID: events.NewID(events.PrefixEvent), CourierID: courier,
		Lat: 35.6640, Lng: 139.6982, DeviceTime: deviceTime, ReceivedAt: deviceTime.Add(500 * time.Millisecond),
		Status: status,
	}
}

func members(t *testing.T, mr *miniredis.Miniredis, key string) []string {
	t.Helper()
	m, err := mr.ZMembers(key)
	if err != nil && !mr.Exists(key) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func mustApply(t *testing.T, s *Store, points ...events.CourierLocation) []Result {
	t.Helper()
	res, err := s.Apply(context.Background(), points)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestApplyKeepsTheNewestPoint(t *testing.T) {
	mr, _, s := newStore(t)
	c := events.NewID(events.PrefixCourier)
	newer := loc(c, t0.Add(10*time.Second), events.CourierToStore)
	newer.Lat = 35.70
	older := loc(c, t0, events.CourierIdle)
	order := events.NewID(events.PrefixOrder)
	newer.OrderID = &order

	if got := mustApply(t, s, newer, older, newer); !slices.Equal(got, []Result{Applied, Stale, Stale}) {
		t.Fatalf("results = %v", got)
	}
	key := CourierKeyPrefix + c
	if mr.HGet(key, "lat") != "35.7" || mr.HGet(key, "status") != "to_store" || mr.HGet(key, "order_id") != order ||
		mr.HGet(key, "device_time") != newer.DeviceTime.Format(time.RFC3339Nano) ||
		mr.HGet(key, "received_at") != newer.ReceivedAt.Format(time.RFC3339Nano) {
		t.Fatalf("hash holds the wrong point: lat=%s status=%s", mr.HGet(key, "lat"), mr.HGet(key, "status"))
	}
	if ttl := mr.TTL(key); ttl != 10*time.Minute {
		t.Fatalf("TTL = %v", ttl)
	}
}

func TestApplyMaintainsGeoSets(t *testing.T) {
	mr, _, s := newStore(t)
	c := events.NewID(events.PrefixCourier)
	step := func(i int, status events.CourierStatus) {
		mustApply(t, s, loc(c, t0.Add(time.Duration(i)*time.Second), status))
	}
	step(1, events.CourierIdle)
	if !slices.Contains(members(t, mr, GeoAllKey), c) || !slices.Contains(members(t, mr, GeoAvailableKey), c) {
		t.Fatal("an idle courier belongs in both geo sets")
	}
	step(2, events.CourierToStore)
	if !slices.Contains(members(t, mr, GeoAllKey), c) || slices.Contains(members(t, mr, GeoAvailableKey), c) {
		t.Fatal("a busy courier is not available")
	}
	if score, _ := mr.ZScore(LastSeenKey, c); int64(score) != t0.Add(2500*time.Millisecond).UnixMilli() {
		t.Fatalf("last seen = %v", score)
	}
	step(3, events.CourierOffline)
	if slices.Contains(members(t, mr, GeoAllKey), c) || slices.Contains(members(t, mr, LastSeenKey), c) {
		t.Fatal("an offline courier leaves the geo sets")
	}
	if mr.HGet(CourierKeyPrefix+c, "status") != "offline" {
		t.Fatal("the hash still records the offline status")
	}
}

func TestApplySkipsGeoForPolarPoints(t *testing.T) {
	mr, _, s := newStore(t)
	c := events.NewID(events.PrefixCourier)
	p := loc(c, t0, events.CourierIdle)
	p.Lat = 89 // valid in the schema, outside what GEOADD accepts
	if got := mustApply(t, s, p); got[0] != Applied {
		t.Fatalf("result = %v", got)
	}
	if slices.Contains(members(t, mr, GeoAllKey), c) {
		t.Fatal("a polar point must not be geo-indexed")
	}
}

func TestApplyReportsPerPointRedisErrors(t *testing.T) {
	mr, _, s := newStore(t)
	broken, fine := events.NewID(events.PrefixCourier), events.NewID(events.PrefixCourier)
	if err := mr.Set(CourierKeyPrefix+broken, "not a hash"); err != nil { // WRONGTYPE inside the script
		t.Fatal(err)
	}
	got := mustApply(t, s, loc(broken, t0, events.CourierIdle), loc(fine, t0, events.CourierIdle))
	if !slices.Equal(got, []Result{Failed, Applied}) {
		t.Fatalf("results = %v", got)
	}
}

func TestApplyReloadsTheScriptAfterRedisForgetsIt(t *testing.T) {
	_, rdb, s := newStore(t)
	c := events.NewID(events.PrefixCourier)
	mustApply(t, s, loc(c, t0, events.CourierIdle))
	if err := rdb.ScriptFlush(context.Background()).Err(); err != nil { // what a Redis restart does
		t.Fatal(err)
	}
	if got := mustApply(t, s, loc(c, t0.Add(time.Second), events.CourierIdle)); got[0] != Applied {
		t.Fatalf("result after script flush = %v", got)
	}
}

func TestApplyFailsWhenRedisIsDown(t *testing.T) {
	mr, _, s := newStore(t)
	mr.Close()
	if _, err := s.Apply(context.Background(), []events.CourierLocation{loc(events.NewID(events.PrefixCourier), t0, events.CourierIdle)}); err == nil {
		t.Fatal("expected an error")
	}
	if s.Ping(context.Background()) == nil {
		t.Fatal("Ping must fail")
	}
	if _, err := s.Sweep(context.Background(), t0); err == nil {
		t.Fatal("Sweep must fail")
	}
	if _, err := s.Rebuilt(context.Background()); err == nil {
		t.Fatal("Rebuilt must fail")
	}
	if s.BeginRebuild(context.Background(), "tok") == nil {
		t.Fatal("BeginRebuild must fail")
	}
	if s.MarkRebuilt(context.Background(), "tok", t0) == nil {
		t.Fatal("MarkRebuilt must fail")
	}
}

func TestSweepRemovesQuietCouriersInBatches(t *testing.T) {
	mr, _, s := newStore(t)
	var quiet []events.CourierLocation
	for range 1005 { // more than one sweep batch
		quiet = append(quiet, loc(events.NewID(events.PrefixCourier), t0, events.CourierIdle))
	}
	mustApply(t, s, quiet...)
	active := events.NewID(events.PrefixCourier)
	mustApply(t, s, loc(active, t0.Add(time.Minute), events.CourierIdle))

	n, err := s.Sweep(context.Background(), t0.Add(30*time.Second))
	if err != nil || n != 1005 {
		t.Fatalf("swept %d, %v", n, err)
	}
	if got := members(t, mr, GeoAvailableKey); !slices.Equal(got, []string{active}) {
		t.Fatalf("available = %d couriers", len(got))
	}
	if !mr.Exists(CourierKeyPrefix + quiet[0].CourierID) {
		t.Fatal("the hash stays until its TTL")
	}
}

func TestRebuildMarker(t *testing.T) {
	mr, _, s := newStore(t)
	ctx := context.Background()
	if ok, err := s.Rebuilt(ctx); ok || err != nil {
		t.Fatalf("fresh Redis: %v %v", ok, err)
	}
	if err := s.BeginRebuild(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkRebuilt(ctx, "a", t0); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.Rebuilt(ctx); !ok {
		t.Fatal("marker not found")
	}
	if mr.Exists(RebuildingKeyPrefix + "a") {
		t.Fatal("the in-flight key must be removed once the rebuild is marked")
	}
	mr.FlushAll()
	if ok, _ := s.Rebuilt(ctx); ok {
		t.Fatal("marker survived a flush")
	}
}

func TestRebuildMarkerRefusesWhenRedisWasFlushedDuringTheRebuild(t *testing.T) {
	mr, _, s := newStore(t)
	ctx := context.Background()
	if err := s.BeginRebuild(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	mr.FlushAll() // Redis loses its data while the replay runs
	if err := s.MarkRebuilt(ctx, "a", t0); !errors.Is(err, ErrFlushedDuringRebuild) {
		t.Fatalf("got %v, want ErrFlushedDuringRebuild", err)
	}
	if ok, _ := s.Rebuilt(ctx); ok {
		t.Fatal("a half-empty Redis must not be marked as rebuilt")
	}
}

func TestRebuildMarkerNeverMovesBackwards(t *testing.T) {
	mr, _, s := newStore(t)
	ctx := context.Background()
	for _, tok := range []string{"early", "late"} {
		if err := s.BeginRebuild(ctx, tok); err != nil {
			t.Fatal(err)
		}
	}
	// The rebuild that started later finishes first; the earlier one must not overwrite it.
	if err := s.MarkRebuilt(ctx, "late", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkRebuilt(ctx, "early", t0); err != nil {
		t.Fatal(err)
	}
	if got, _ := mr.Get(RebuiltKey); got != t0.Add(time.Minute).UTC().Format(time.RFC3339) {
		t.Fatalf("marker = %s", got)
	}
}

// --- Processor ---

type recordingSink struct {
	mu   sync.Mutex
	got  int
	fail bool
}

func (r *recordingSink) Write(_ context.Context, p []events.CourierLocation) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail {
		return errors.New("disk full")
	}
	r.got += len(p)
	return nil
}
func (r *recordingSink) Close() error { return nil }

func record(t *testing.T, l events.CourierLocation) *kgo.Record {
	t.Helper()
	b, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	return &kgo.Record{Topic: events.TopicCourierLocations, Key: []byte(l.CourierID), Value: b, Timestamp: l.ReceivedAt}
}

func newProcessor(s *Store, sink history.Sink, live bool) (*Processor, *metrics) {
	m := newMetrics(prometheus.NewRegistry())
	return &Processor{store: s, sink: sink, logger: discard(), metrics: m, now: func() time.Time { return t0.Add(2 * time.Second) }, live: live}, m
}

func TestProcessorAppliesCountsAndWritesHistory(t *testing.T) {
	_, _, s := newStore(t)
	sink := &recordingSink{}
	p, m := newProcessor(s, sink, true)
	c := events.NewID(events.PrefixCourier)
	recs := []*kgo.Record{
		record(t, loc(c, t0.Add(time.Second), events.CourierIdle)),
		record(t, loc(c, t0, events.CourierIdle)), // arrives late: stale
		{Value: []byte("garbage")},
	}
	if err := p.Handle(context.Background(), recs); err != nil {
		t.Fatal(err)
	}
	for result, want := range map[string]float64{"applied": 1, "stale": 1, "invalid": 1} {
		if got := testutil.ToFloat64(m.points.WithLabelValues(result)); got != want {
			t.Errorf("points{%s} = %v, want %v", result, got, want)
		}
	}
	if n := testutil.CollectAndCount(m.freshness); n != 1 || sink.got != 2 {
		t.Fatalf("freshness series %d, history points %d", n, sink.got)
	}
	if err := p.Handle(context.Background(), []*kgo.Record{{Value: []byte("x")}}); err != nil {
		t.Fatal("a batch of only invalid records is not an error")
	}
}

func TestProcessorToleratesHistoryFailures(t *testing.T) {
	_, _, s := newStore(t)
	p, m := newProcessor(s, &recordingSink{fail: true}, true)
	if err := p.Handle(context.Background(), []*kgo.Record{record(t, loc(events.NewID(events.PrefixCourier), t0, events.CourierIdle))}); err != nil {
		t.Fatal(err)
	}
	if testutil.ToFloat64(m.historyErrors) != 1 || testutil.ToFloat64(m.points.WithLabelValues("applied")) != 1 {
		t.Fatal("a history failure must be counted and must not stop Redis updates")
	}
}

func TestProcessorRetriesUntilRedisReturns(t *testing.T) {
	mr, _, s := newStore(t)
	p, m := newProcessor(s, history.Discard{}, false)
	mr.Close()
	go func() { time.Sleep(300 * time.Millisecond); _ = mr.Restart() }()
	if err := p.Handle(context.Background(), []*kgo.Record{record(t, loc(events.NewID(events.PrefixCourier), t0, events.CourierIdle))}); err != nil {
		t.Fatal(err)
	}
	if testutil.ToFloat64(m.points.WithLabelValues("applied")) != 1 {
		t.Fatal("the point was not applied after Redis came back")
	}

	mr.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := p.Handle(ctx, []*kgo.Record{record(t, loc(events.NewID(events.PrefixCourier), t0, events.CourierIdle))}); err == nil {
		t.Fatal("Handle must give up when its context ends")
	}
}

func TestProcessorCountsFailedPoints(t *testing.T) {
	mr, _, s := newStore(t)
	p, m := newProcessor(s, history.Discard{}, true)
	c := events.NewID(events.PrefixCourier)
	_ = mr.Set(CourierKeyPrefix+c, "x")
	if err := p.Handle(context.Background(), []*kgo.Record{record(t, loc(c, t0, events.CourierIdle))}); err != nil {
		t.Fatal(err)
	}
	if testutil.ToFloat64(m.points.WithLabelValues("failed")) != 1 {
		t.Fatal("failed point not counted")
	}
}

func itoa(i int) string { return strconv.Itoa(i) }
