package tracking

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/auth"
	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/events"
	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/kit"
)

const secret = "test-secret-that-is-at-least-32-bytes-long"

func discard() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

type fixture struct {
	url      string
	hub      *Hub
	orders   *Orders
	metrics  *metrics
	streams  *Streams
	stopping chan struct{}
}

func newFixture(t *testing.T, cfg StreamConfig, hubCfg HubConfig) *fixture {
	t.Helper()
	if cfg.Heartbeat == 0 {
		cfg.Heartbeat = time.Hour
	}
	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = time.Second
	}
	if cfg.UnknownOrderWait == 0 {
		cfg.UnknownOrderWait = 50 * time.Millisecond
	}
	h, orders, m := newTestHub(hubCfg)
	v, err := auth.NewVerifier(secret, auth.RoleTracking, events.PrefixOrder)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{hub: h, orders: orders, metrics: m, stopping: make(chan struct{})}
	f.streams = &Streams{cfg: cfg, hub: h, orders: orders, verifier: v, stopping: f.stopping,
		logger: discard(), metrics: m, now: time.Now}
	srv := kit.New(kit.Config{}, discard())
	f.streams.Register(srv)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	f.url = ts.URL
	return f
}

type sseEvent struct {
	name, data string
	comment    bool
}

// streamResp is the part of the response the tests look at; open owns (and closes) the body.
type streamResp struct {
	StatusCode int
	Header     http.Header
}

// open starts a stream and returns its parsed events; the channel closes when the stream ends.
func open(t *testing.T, url string, header map[string]string) (streamResp, <-chan sseEvent) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	ch := make(chan sseEvent, 1000)
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

// next returns the next event (comments included unless skipComments).
func next(t *testing.T, ch <-chan sseEvent, skipComments bool) sseEvent {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("stream closed")
			}
			if ev.comment && skipComments {
				continue
			}
			return ev
		case <-timeout:
			t.Fatal("timed out waiting for an event")
		}
	}
}

func expectClosed(t *testing.T, ch <-chan sseEvent) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if !ev.comment {
				t.Fatalf("unexpected event %+v before close", ev)
			}
		case <-timeout:
			t.Fatal("stream did not close")
		}
	}
}

func trackingToken(t *testing.T, order string, exp time.Time) string {
	t.Helper()
	tok, err := auth.Issue(secret, auth.RoleTracking, order, exp)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestTrackStreamFromStatusToEnd(t *testing.T) {
	f := newFixture(t, StreamConfig{}, HubConfig{})
	id, courier := events.NewID(events.PrefixOrder), events.NewID(events.PrefixCourier)
	f.orders.ApplyOrderEvent(orderEvent(id, events.OrderConfirmed, t0))
	f.orders.ApplyDispatchEvent(assigned(id, courier, t0, 60, 60))
	f.hub.OnLocation(location(courier, shibuya, time.Now().Add(-time.Second)))

	resp, ch := open(t, f.url+"/v1/track/"+trackingToken(t, id, time.Now().Add(time.Hour))+"/stream", nil)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("got %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	status := next(t, ch, true)
	var s map[string]any
	_ = json.Unmarshal([]byte(status.data), &s)
	if status.name != "status" || s["status"] != "assigned" || s["order_id"] != id || s["updated_at"] == nil ||
		s["eta"] != t0.Add(40*time.Minute).Format(time.RFC3339) {
		t.Fatalf("first event %+v", status)
	}
	if ev := next(t, ch, true); ev.name != "position" || !strings.Contains(ev.data, courier) {
		t.Fatalf("second event %+v", ev)
	}

	f.hub.OnLocation(location(courier, minato, time.Now()))
	pos := next(t, ch, true)
	var p map[string]any
	_ = json.Unmarshal([]byte(pos.data), &p)
	if pos.name != "position" || len(p) != 4 || p["lat"] != minato[0] {
		t.Fatalf("live position %+v", pos) // exactly courier_id, lat, lng, received_at
	}
	if testutil.CollectAndCount(f.metrics.eventAge) != 1 {
		t.Fatal("event age not observed for the live position")
	}

	view, _ := f.orders.ApplyOrderEvent(orderEvent(id, events.OrderDelivered, t0.Add(time.Minute)))
	f.hub.OnOrderUpdate(view)
	if ev := next(t, ch, true); ev.name != "status" || !strings.Contains(ev.data, `"delivered"`) {
		t.Fatalf("got %+v", ev)
	}
	if ev := next(t, ch, true); ev.name != "end" || ev.data != `{"reason":"delivered"}` {
		t.Fatalf("got %+v", ev)
	}
	expectClosed(t, ch)
}

func TestTrackStreamErrors(t *testing.T) {
	f := newFixture(t, StreamConfig{}, HubConfig{MaxViewers: 1})
	unknown := trackingToken(t, events.NewID(events.PrefixOrder), time.Now().Add(time.Hour))
	expired := trackingToken(t, events.NewID(events.PrefixOrder), time.Now().Add(-time.Minute))
	for name, tc := range map[string]struct {
		path string
		want int
	}{
		"bad token":     {"/v1/track/nope/stream", http.StatusUnauthorized},
		"expired token": {"/v1/track/" + expired + "/stream", http.StatusUnauthorized},
		"unknown order": {"/v1/track/" + unknown + "/stream", http.StatusNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			resp, err := http.Get(f.url + tc.path)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("got %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}

	id := events.NewID(events.PrefixOrder)
	f.orders.ApplyOrderEvent(orderEvent(id, events.OrderConfirmed, t0))
	if _, err := f.hub.SubscribeFleet(""); err != nil { // fill the only slot
		t.Fatal(err)
	}
	resp, err := http.Get(f.url + "/v1/track/" + trackingToken(t, id, time.Now().Add(time.Hour)) + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("got %d", resp.StatusCode)
	}
}

func TestTrackStreamWaitsBrieflyForANewOrder(t *testing.T) {
	f := newFixture(t, StreamConfig{UnknownOrderWait: 3 * time.Second}, HubConfig{})
	id := events.NewID(events.PrefixOrder)
	go func() {
		time.Sleep(200 * time.Millisecond)
		f.orders.ApplyOrderEvent(orderEvent(id, events.OrderConfirmed, t0))
	}()
	resp, ch := open(t, f.url+"/v1/track/"+trackingToken(t, id, time.Now().Add(time.Hour))+"/stream", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d", resp.StatusCode)
	}
	if ev := next(t, ch, true); ev.name != "status" || !strings.Contains(ev.data, `"confirmed"`) {
		t.Fatalf("got %+v", ev)
	}
}

func TestTrackStreamForAFinishedOrderEndsAtOnce(t *testing.T) {
	f := newFixture(t, StreamConfig{}, HubConfig{})
	id := events.NewID(events.PrefixOrder)
	f.orders.ApplyOrderEvent(orderEvent(id, events.OrderCancelled, t0))
	_, ch := open(t, f.url+"/v1/track/"+trackingToken(t, id, time.Now().Add(time.Hour))+"/stream", nil)
	if ev := next(t, ch, true); ev.name != "status" {
		t.Fatalf("got %+v", ev)
	}
	if ev := next(t, ch, true); ev.name != "end" || ev.data != `{"reason":"cancelled"}` {
		t.Fatalf("got %+v", ev)
	}
	expectClosed(t, ch)
}

func TestTrackStreamEndsWhenTheTokenExpires(t *testing.T) {
	f := newFixture(t, StreamConfig{}, HubConfig{})
	id := events.NewID(events.PrefixOrder)
	f.orders.ApplyOrderEvent(orderEvent(id, events.OrderConfirmed, t0))
	// JWT expiry has one-second precision, so this token lives 1-2 s.
	_, ch := open(t, f.url+"/v1/track/"+trackingToken(t, id, time.Now().Add(2*time.Second))+"/stream", nil)
	next(t, ch, true) // status
	if ev := next(t, ch, true); ev.name != "end" || ev.data != `{"reason":"expired"}` {
		t.Fatalf("got %+v", ev)
	}
	expectClosed(t, ch)
}

func TestHeartbeatPings(t *testing.T) {
	f := newFixture(t, StreamConfig{Heartbeat: 30 * time.Millisecond, FleetPublic: true}, HubConfig{})
	_, ch := open(t, f.url+"/v1/fleet/stream", nil)
	for range 2 {
		if ev := next(t, ch, false); !ev.comment || ev.data != ": ping" {
			t.Fatalf("got %+v", ev)
		}
	}
}

func TestFleetStreamAccess(t *testing.T) {
	disabled := newFixture(t, StreamConfig{}, HubConfig{})
	resp, _ := http.Get(disabled.url + "/v1/fleet/stream")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("no token, not public: got %d", resp.StatusCode)
	}

	f := newFixture(t, StreamConfig{FleetToken: "console-token"}, HubConfig{})
	for name, tc := range map[string]struct {
		path   string
		header map[string]string
		want   int
	}{
		"missing token": {"/v1/fleet/stream", nil, http.StatusUnauthorized},
		"wrong token":   {"/v1/fleet/stream?token=nope", nil, http.StatusUnauthorized},
		"unknown ward":  {"/v1/fleet/stream?token=console-token&ward=atlantis", nil, http.StatusBadRequest},
		"query token":   {"/v1/fleet/stream?token=console-token", nil, http.StatusOK},
		"bearer token":  {"/v1/fleet/stream?ward=shibuya", map[string]string{"Authorization": "Bearer console-token"}, http.StatusOK},
	} {
		t.Run(name, func(t *testing.T) {
			resp, _ := open(t, f.url+tc.path, tc.header)
			if resp.StatusCode != tc.want {
				t.Fatalf("got %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

func TestFleetStreamSnapshotLiveAndGone(t *testing.T) {
	f := newFixture(t, StreamConfig{FleetPublic: true, CORSOrigins: []string{"http://localhost:5173"}}, HubConfig{})
	a, b := events.NewID(events.PrefixCourier), events.NewID(events.PrefixCourier)
	f.hub.OnLocation(location(a, shibuya, time.Now().Add(-2*time.Minute)))
	f.hub.OnLocation(location(b, minato, time.Now()))

	resp, ch := open(t, f.url+"/v1/fleet/stream?ward=shibuya", map[string]string{"Origin": "http://localhost:5173"})
	if resp.Header.Get("Access-Control-Allow-Origin") != "http://localhost:5173" {
		t.Fatal("CORS header missing for an allowed origin")
	}
	ev := next(t, ch, true)
	var c map[string]any
	_ = json.Unmarshal([]byte(ev.data), &c)
	if ev.name != "courier" || c["courier_id"] != a || c["ward"] != "shibuya" {
		t.Fatalf("snapshot %+v", ev)
	}
	for _, field := range []string{"lat", "lng", "status", "order_id", "device_time", "received_at"} {
		if _, ok := c[field]; !ok {
			t.Errorf("courier event is missing %q", field)
		}
	}

	f.hub.OnLocation(location(a, shibuya, time.Now().Add(-90*time.Second)))
	if ev := next(t, ch, true); ev.name != "courier" {
		t.Fatalf("live %+v", ev)
	}
	f.hub.SweepGone(time.Now().Add(-time.Minute))
	if ev := next(t, ch, true); ev.name != "courier_gone" || ev.data != `{"courier_id":"`+a+`"}` {
		t.Fatalf("gone %+v", ev)
	}

	other, _ := open(t, f.url+"/v1/fleet/stream", map[string]string{"Origin": "https://evil.example"})
	if other.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("CORS header set for an unknown origin")
	}
}

func TestStreamsEndOnShutdown(t *testing.T) {
	f := newFixture(t, StreamConfig{FleetPublic: true}, HubConfig{})
	_, ch := open(t, f.url+"/v1/fleet/stream", nil)
	waitFor(t, "the stream to open", func() bool {
		return testutil.ToFloat64(f.metrics.connections.WithLabelValues(streamFleet)) == 1
	})
	close(f.stopping)
	expectClosed(t, ch)
	waitFor(t, "the connection gauge to drop", func() bool {
		return testutil.ToFloat64(f.metrics.connections.WithLabelValues(streamFleet)) == 0
	})
}

// A client that stops reading must cost it events, never stall the Kafka consumer.
func TestSlowHTTPClientGetsDropsNotBackpressure(t *testing.T) {
	f := newFixture(t, StreamConfig{FleetPublic: true, WriteTimeout: 10 * time.Second}, HubConfig{ViewerBuffer: 8})
	resp, err := http.Get(f.url + "/v1/fleet/stream") // opened and never read
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	waitFor(t, "the stream to open", func() bool {
		return testutil.ToFloat64(f.metrics.connections.WithLabelValues(streamFleet)) == 1
	})

	start := time.Now()
	for i := range 20000 { // ~5 MB of events: far more than the socket buffers hold
		f.hub.OnLocation(location(events.NewID(events.PrefixCourier), shibuya, t0.Add(time.Duration(i)*time.Millisecond)))
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the consumer was slowed to %v by a client that does not read", took)
	}
	if testutil.ToFloat64(f.metrics.dropped.WithLabelValues(streamFleet)) == 0 {
		t.Fatal("expected dropped events for the slow client")
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// syncBuffer is a log sink that the server goroutines and the test can share.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// Tracking tokens are bearer credentials in the URL path: they must never reach the logs.
func TestTrackingTokensAreNeverLogged(t *testing.T) {
	f := newFixture(t, StreamConfig{}, HubConfig{})
	var logs syncBuffer
	srv := kit.New(kit.Config{}, slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	f.streams.Register(srv)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	id := events.NewID(events.PrefixOrder)
	f.orders.ApplyOrderEvent(orderEvent(id, events.OrderDelivered, t0)) // the stream ends at once
	good := trackingToken(t, id, time.Now().Add(time.Hour))
	for _, tok := range []string{good, "not-a-valid-token-but-still-secret"} {
		resp, err := http.Get(ts.URL + "/v1/track/" + tok + "/stream")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	waitFor(t, "two access-log lines", func() bool { return strings.Count(logs.String(), `"msg":"request"`) == 2 })
	out := logs.String()
	if strings.Contains(out, good) || strings.Contains(out, "not-a-valid-token-but-still-secret") {
		t.Fatalf("a tracking token reached the logs:\n%s", out)
	}
	if !strings.Contains(out, `"route":"GET /v1/track/{token}/stream"`) {
		t.Fatalf("expected the route pattern in the access log:\n%s", out)
	}
}
