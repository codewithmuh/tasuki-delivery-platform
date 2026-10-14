package dispatch

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/auth"
	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/events"
	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/kit"
	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/state"
)

const secret = "test-secret-that-is-at-least-32-bytes-long" // gitleaks:allow (test fixture)

var (
	t0      = time.Date(2026, 10, 14, 3, 0, 0, 0, time.UTC)
	shibuya = Point{Lat: 35.6580, Lng: 139.7016} // the store in most tests
	dropoff = Point{Lat: 35.6450, Lng: 139.7100} // about 1.6 km south-east
)

func discard() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// north returns a point metres north of p.
func north(p Point, metres float64) Point { return Point{Lat: p.Lat + metres/111_195, Lng: p.Lng} }

// clock is a settable time source shared by a test's replicas.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// dispatchSchema compiles contracts/events/dispatch-event.v1.schema.json with format checks on.
func dispatchSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	path := filepath.Join("..", "..", "..", "..", "contracts", "events", "dispatch-event.v1.schema.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	if err := c.AddResource("dispatch.json", doc); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile("dispatch.json")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// fakeProducer stands in for Kafka. It validates every event against the contract schema, so any
// test that emits an event also checks it, and acknowledges it (or fails it with fail).
type fakeProducer struct {
	t      *testing.T
	schema *jsonschema.Schema
	mu     sync.Mutex
	sent   []events.DispatchEvent
	fail   error
}

func (p *fakeProducer) TryProduce(_ context.Context, r *kgo.Record, promise func(*kgo.Record, error)) {
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(r.Value))
	if err != nil {
		p.t.Errorf("event is not JSON: %v", err)
	} else if err := p.schema.Validate(inst); err != nil {
		p.t.Errorf("event does not match dispatch-event.v1: %v\n%s", err, r.Value)
	}
	var e events.DispatchEvent
	_ = json.Unmarshal(r.Value, &e)
	if string(r.Key) != e.OrderID || r.Topic != events.TopicDispatch {
		p.t.Errorf("record key %q topic %q, want order_id %q on %s", r.Key, r.Topic, e.OrderID, events.TopicDispatch)
	}
	p.mu.Lock()
	fail := p.fail
	if fail == nil {
		p.sent = append(p.sent, e)
	}
	p.mu.Unlock()
	promise(r, fail)
}

func (p *fakeProducer) of(typ events.DispatchEventType) []events.DispatchEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []events.DispatchEvent
	for _, e := range p.sent {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func (p *fakeProducer) setFail(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fail = err
}

type open struct{}

func (open) Dispatching() bool { return true }

// world is one Redis, one Kafka stand-in and one clock, shared by the replicas of a test.
type world struct {
	t     *testing.T
	mr    *miniredis.Miniredis
	rdb   *redis.Client
	clock *clock
	prod  *fakeProducer
}

func newWorld(t *testing.T) *world {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return &world{t: t, mr: mr, rdb: rdb, clock: &clock{t: t0}, prod: &fakeProducer{t: t, schema: dispatchSchema(t)}}
}

func testStoreConfig() StoreConfig {
	return StoreConfig{OfferTTL: 30 * time.Second, OfferKeep: 10 * time.Minute, OrderTTL: 24 * time.Hour,
		BusyTTL: 3 * time.Hour, OutboxLease: 10 * time.Second}
}

func testMatchConfig() MatchConfig {
	return MatchConfig{Interval: time.Hour, OfferTTL: 30 * time.Second, SearchRadiusM: 1000, SearchMaxRadiusM: 4000,
		MaxCandidates: 10, MaxAttempts: 3, NoCourierAfter: time.Minute, ReportInterval: time.Minute,
		RetryInterval: 5 * time.Second, RoutingTimeout: 500 * time.Millisecond}
}

// replica is one dispatch replica: its own objects and HTTP server, the world's Redis.
type replica struct {
	reg      *prometheus.Registry
	metrics  *metrics
	store    *Store
	owner    *Ownership
	router   *Router
	outbox   *Outbox
	notify   *Notifier
	matcher  *Matcher
	consumer *Consumer
	hub      *Hub
	api      *API
	url      string
	stopping chan struct{}
}

// replica starts a replica that owns partition 0. routingURL "" means no OSRM.
func (w *world) replica(routingURL string) *replica {
	w.t.Helper()
	r := &replica{reg: prometheus.NewRegistry(), stopping: make(chan struct{})}
	r.metrics = newMetrics(r.reg)
	r.store = NewStore(w.rdb, testStoreConfig())
	r.owner = NewOwnership()
	r.owner.Assign([]int32{0})
	r.router = NewRouter(routingURL, r.metrics)
	r.notify = &Notifier{rdb: w.rdb, logger: discard()}
	r.outbox = &Outbox{store: r.store, producer: w.prod, topic: events.TopicDispatch, interval: time.Hour, batch: 100,
		logger: discard(), metrics: r.metrics, now: w.clock.Now}
	r.matcher = &Matcher{cfg: testMatchConfig(), store: r.store, router: r.router, outbox: r.outbox, notify: r.notify,
		owner: r.owner, gate: open{}, metrics: r.metrics, logger: discard(), now: w.clock.Now, wake: make(chan struct{}, 1)}
	r.consumer = &Consumer{store: r.store, notify: r.notify, wake: r.matcher.Wake, metrics: r.metrics, logger: discard()}
	r.hub = NewHub(100, r.metrics)
	verifier, err := auth.NewVerifier(secret, auth.RoleCourier, events.PrefixCourier)
	if err != nil {
		w.t.Fatal(err)
	}
	r.api = &API{cfg: APIConfig{Heartbeat: time.Hour, WriteTimeout: time.Second, SearchMaxRadiusM: 4000,
		ETABudget: 250 * time.Millisecond, ETARoutingTimeout: 100 * time.Millisecond, SupplyLowBelow: 3},
		store: r.store, hub: r.hub, outbox: r.outbox, router: r.router, verifier: verifier, stopping: r.stopping,
		metrics: r.metrics, logger: discard(), now: w.clock.Now}
	srv := kit.New(kit.Config{}, discard())
	r.api.Register(srv)
	ts := httptest.NewServer(srv.Handler())
	w.t.Cleanup(ts.Close)
	r.url = ts.URL

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = r.hub.Follow(ctx, w.rdb, discard()); close(done) }()
	w.t.Cleanup(func() { cancel(); <-done })
	waitFor(w.t, "the offer subscription", r.hub.Subscribed)
	return r
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// addCourier puts an idle courier into fleet-state's geo sets.
func (w *world) addCourier(p Point) string {
	w.t.Helper()
	id := events.NewID(events.PrefixCourier)
	for _, key := range []string{state.GeoAvailableKey, state.GeoAllKey} {
		if err := w.rdb.GeoAdd(context.Background(), key, &redis.GeoLocation{Name: id, Longitude: p.Lng, Latitude: p.Lat}).Err(); err != nil {
			w.t.Fatal(err)
		}
	}
	return id
}

func orderEvent(typ events.OrderEventType, orderID string, at time.Time) events.OrderEvent {
	ready := at.Add(10 * time.Minute)
	return events.OrderEvent{
		Schema: events.SchemaOrderEvent, EventID: events.NewID(events.PrefixEvent), Type: typ, OrderID: orderID,
		MerchantID: events.NewID(events.PrefixMerchant), OccurredAt: at,
		Order: events.OrderDetails{
			Store:      events.Store{ID: "str_01JABCDEFGHJKMNPQRSTVWXYZ0", Lat: shibuya.Lat, Lng: shibuya.Lng, Ward: "shibuya"},
			Dropoff:    events.Place{Lat: dropoff.Lat, Lng: dropoff.Lng, Ward: "shibuya"},
			ReadyAt:    &ready,
			PromisedAt: at.Add(40 * time.Minute),
			FeeYen:     660,
		},
	}
}

func record(t *testing.T, e any) *kgo.Record {
	t.Helper()
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	topic := events.TopicOrders
	if _, ok := e.(events.DispatchEvent); ok {
		topic = events.TopicDispatch
	}
	return &kgo.Record{Topic: topic, Partition: 0, Value: b}
}

// apply feeds one orders.events event to the replica's consumer.
func (r *replica) apply(t *testing.T, e events.OrderEvent) {
	t.Helper()
	if err := r.consumer.Handle(context.Background(), []*kgo.Record{record(t, e)}); err != nil {
		t.Fatal(err)
	}
}

// confirm creates a new order, confirmed at the world's current time.
func (w *world) confirm(r *replica) string {
	w.t.Helper()
	id := events.NewID(events.PrefixOrder)
	r.apply(w.t, orderEvent(events.OrderConfirmed, id, w.clock.Now()))
	return id
}

func (w *world) order(id string) Order {
	w.t.Helper()
	o, err := NewStore(w.rdb, testStoreConfig()).Order(context.Background(), id)
	if err != nil {
		w.t.Fatal(err)
	}
	return o
}

func token(t *testing.T, courierID string) string {
	t.Helper()
	tok, err := auth.Issue(secret, auth.RoleCourier, courierID, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// post answers an offer and returns the status code and decoded body.
func post(t *testing.T, url, tok string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, url, nil)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
}

func errorCode(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

type sseEvent struct {
	name, data string
	comment    bool
}

// streamResp is the part of a stream response the tests look at; stream owns (and closes) the body.
type streamResp struct {
	StatusCode int
	Header     http.Header
}

// stream opens a courier offer stream and returns the response and its parsed events; the
// channel closes when the stream ends.
func stream(t *testing.T, url, tok string) (streamResp, <-chan sseEvent) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url+"/v1/couriers/me/offers/stream", nil)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	ch := make(chan sseEvent, 100)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(resp.Body)
		var ev sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, ":"):
				ch <- sseEvent{data: line, comment: true}
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.data = strings.TrimPrefix(line, "data: ")
			case line == "" && ev.name != "":
				ch <- ev
				ev = sseEvent{}
			}
		}
	}()
	return streamResp{StatusCode: resp.StatusCode, Header: resp.Header}, ch
}

// next returns the next named event, skipping comments.
func next(t *testing.T, ch <-chan sseEvent) sseEvent {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("stream closed")
			}
			if !ev.comment {
				return ev
			}
		case <-timeout:
			t.Fatal("timed out waiting for an event")
		}
	}
}

// histCount returns how many observations the histogram has for the label value.
func histCount(t *testing.T, reg *prometheus.Registry, name, label string) uint64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetValue() == label {
					return m.GetHistogram().GetSampleCount()
				}
			}
		}
	}
	return 0
}

// openOffers counts open offers by courier and by order.
func (w *world) openOffers() (byCourier, byOrder map[string]int) {
	w.t.Helper()
	ctx := context.Background()
	byCourier, byOrder = map[string]int{}, map[string]int{}
	keys, err := w.rdb.Keys(ctx, offerKeyPrefix+"*").Result()
	if err != nil {
		w.t.Fatal(err)
	}
	for _, k := range keys {
		f := w.rdb.HGetAll(ctx, k).Val()
		if f["status"] == offerOpen {
			byCourier[f["courier_id"]]++
			byOrder[f["order_id"]]++
		}
	}
	return byCourier, byOrder
}

// deadRedis is a client for an address where nothing listens.
func deadRedis(t *testing.T) *redis.Client {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	rdb := redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1, DialTimeout: 100 * time.Millisecond})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func tokenWithRole(t *testing.T, role, subject string) (string, error) {
	t.Helper()
	return auth.Issue(secret, role, subject, time.Now().Add(time.Hour))
}

func (h *Hub) streamCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n
}
