package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/auth"
	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/events"
	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/kit"
)

const secret = "test-secret-that-is-at-least-32-bytes-long"

var now = time.Date(2026, 10, 14, 3, 0, 0, 0, time.UTC)

func discard() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// fakePublisher records what it was asked to publish and returns err.
type fakePublisher struct {
	mu   sync.Mutex
	got  []events.CourierLocation
	err  error
	wait bool // block until ctx ends, like a Kafka that never answers
}

func (f *fakePublisher) Publish(ctx context.Context, locs []events.CourierLocation) error {
	if f.wait {
		<-ctx.Done()
		return ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, locs...)
	return f.err
}

func testConfig() Config {
	return LoadConfig(kit.NewEnv(func(string) string { return "" }))
}

func newHandler(t *testing.T, pub Publisher) *Handler {
	t.Helper()
	v, err := auth.NewVerifier(secret, auth.RoleCourier, events.PrefixCourier)
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.PublishTimeout = 50 * time.Millisecond
	h := NewHandler(cfg, v, pub, discard(), prometheus.NewRegistry())
	h.now = func() time.Time { return now }
	return h
}

func courierToken(t *testing.T, courier string) string {
	t.Helper()
	tok, err := auth.Issue(secret, auth.RoleCourier, courier, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func post(t *testing.T, h http.Handler, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/locations", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func pointJSON(deviceTime time.Time, extra string) string {
	return fmt.Sprintf(`{"lat":35.664,"lng":139.698,"accuracy_m":5,"speed_mps":4.2,"heading_deg":90,`+
		`"device_time":%q,"status":"to_store","order_id":null%s}`, deviceTime.Format(time.RFC3339Nano), extra)
}

func batchJSON(points ...string) string { return `{"points":[` + strings.Join(points, ",") + `]}` }

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct{ Code string } `json:"error"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return body.Error.Code
}

func TestAcceptsABatchAndStampsIt(t *testing.T) {
	pub := &fakePublisher{}
	h := newHandler(t, pub)
	courier := events.NewID(events.PrefixCourier)
	order := events.NewID(events.PrefixOrder)
	rec := post(t, h, courierToken(t, courier), batchJSON(
		pointJSON(now.Add(-8*time.Second), ""),
		pointJSON(now.Add(-4*time.Second), `,"courier_id":"cou_SPOOFED"`), // body cannot pick the courier
		strings.Replace(pointJSON(now, ""), `"order_id":null`, `"order_id":"`+order+`"`, 1),
	))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
	var resp map[string]int
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	if resp["accepted"] != 3 || resp["dropped"] != 0 {
		t.Fatalf("response %v", resp)
	}
	if len(pub.got) != 3 {
		t.Fatalf("published %d events", len(pub.got))
	}
	for _, e := range pub.got {
		if e.CourierID != courier || !e.ReceivedAt.Equal(now) || e.Validate() != nil {
			t.Errorf("bad event %+v (validate: %v)", e, e.Validate())
		}
	}
	if pub.got[2].OrderID == nil || *pub.got[2].OrderID != order {
		t.Error("order_id was not carried over")
	}
	if got := testutil.ToFloat64(h.points.WithLabelValues(resultAccepted)); got != 3 {
		t.Errorf("accepted metric = %v", got)
	}
}

func TestDropsBadFutureAndDuplicatePointsOneByOne(t *testing.T) {
	pub := &fakePublisher{}
	h := newHandler(t, pub)
	good := pointJSON(now.Add(-time.Second), "")
	rec := post(t, h, courierToken(t, events.NewID(events.PrefixCourier)), batchJSON(
		good,
		good,                                   // duplicate device_time
		pointJSON(now.Add(31*time.Second), ""), // too far in the future
		pointJSON(now.Add(29*time.Second), ""), // within the 30 s allowance
		`{"lng":139.7,"device_time":"2026-10-14T03:00:00Z","status":"idle"}`, // no lat
		`{"lat":91,"lng":139.7,"device_time":"2026-10-14T03:00:00Z","status":"idle"}`,
		`{"lat":35,"lng":139.7,"device_time":"yesterday","status":"idle"}`,
		`"not an object"`,
	))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
	var resp map[string]int
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	if resp["accepted"] != 2 || resp["dropped"] != 6 {
		t.Fatalf("response %v", resp)
	}
	for result, want := range map[string]float64{resultInvalid: 4, resultFuture: 1, resultDuplicate: 1, resultAccepted: 2} {
		if got := testutil.ToFloat64(h.points.WithLabelValues(result)); got != want {
			t.Errorf("points{result=%s} = %v, want %v", result, got, want)
		}
	}
}

func TestOnlyFuturePointsPublishNothing(t *testing.T) {
	pub := &fakePublisher{err: errors.New("must not be called")}
	rec := post(t, newHandler(t, pub), courierToken(t, events.NewID(events.PrefixCourier)),
		batchJSON(pointJSON(now.Add(time.Hour), "")))
	if rec.Code != http.StatusAccepted || len(pub.got) != 0 {
		t.Fatalf("got %d, published %d", rec.Code, len(pub.got))
	}
}

func TestRejectsBadRequests(t *testing.T) {
	h := newHandler(t, &fakePublisher{})
	token := courierToken(t, events.NewID(events.PrefixCourier))
	trackingToken, _ := auth.Issue(secret, auth.RoleTracking, events.NewID(events.PrefixOrder), time.Now().Add(time.Hour))
	tooMany := make([]string, 51)
	for i := range tooMany {
		tooMany[i] = pointJSON(now.Add(-time.Duration(i)*time.Second), "")
	}
	cases := []struct {
		name, token, body string
		status            int
		code              string
	}{
		{"no token", "", batchJSON(pointJSON(now, "")), http.StatusUnauthorized, "unauthorized"},
		{"tracking token", trackingToken, batchJSON(pointJSON(now, "")), http.StatusUnauthorized, "invalid_token"},
		{"not json", token, "{", http.StatusBadRequest, "invalid_json"},
		{"too large", token, `{"points":[` + strings.Repeat(" ", 70<<10) + `]}`, http.StatusRequestEntityTooLarge, "body_too_large"},
		{"empty batch", token, `{"points":[]}`, http.StatusUnprocessableEntity, "invalid_batch"},
		{"51 points", token, batchJSON(tooMany...), http.StatusUnprocessableEntity, "invalid_batch"},
		{"all invalid", token, batchJSON(`{"lat":1}`, `{}`), http.StatusUnprocessableEntity, "invalid_points"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := post(t, h, tc.token, tc.body)
			if rec.Code != tc.status || errorCode(t, rec) != tc.code {
				t.Fatalf("got %d", rec.Code)
			}
		})
	}
}

func TestShedsLoadWithRetryAfter(t *testing.T) {
	cases := map[string]struct {
		pub    *fakePublisher
		reason string
	}{
		"buffer full": {&fakePublisher{err: fmt.Errorf("%w: test", ErrOverloaded)}, "buffer_full"},
		"timeout":     {&fakePublisher{wait: true}, "timeout"},
		"kafka error": {&fakePublisher{err: errors.New("broker said no")}, "kafka_error"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHandler(t, tc.pub)
			rec := post(t, h, courierToken(t, events.NewID(events.PrefixCourier)), batchJSON(pointJSON(now, "")))
			if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "5" || errorCode(t, rec) != "overloaded" {
				t.Fatalf("got %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
			}
			if testutil.ToFloat64(h.shed.WithLabelValues(tc.reason)) != 1 || testutil.ToFloat64(h.points.WithLabelValues(resultShed)) != 1 {
				t.Fatal("shed metrics not recorded")
			}
		})
	}
}

// --- Kafka publisher, against an in-memory Kafka (kfake) ---

func fakeKafka(t *testing.T) (*kfake.Cluster, kit.KafkaConfig) {
	t.Helper()
	c, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(3, events.TopicCourierLocations))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c, kit.KafkaConfig{Brokers: c.ListenAddrs(), ClientID: "test"}
}

func sampleLocations(n int) []events.CourierLocation {
	courier := events.NewID(events.PrefixCourier)
	out := make([]events.CourierLocation, n)
	for i := range out {
		out[i] = events.CourierLocation{
			Schema: events.SchemaCourierLocation, EventID: events.NewID(events.PrefixEvent), CourierID: courier,
			Lat: 35.66, Lng: 139.70, DeviceTime: now.Add(time.Duration(i) * time.Second), ReceivedAt: now,
			Status: events.CourierIdle,
		}
	}
	return out
}

func TestKafkaPublisherProducesKeyedRecords(t *testing.T) {
	_, cfg := fakeKafka(t)
	producer, err := cfg.NewClient()
	if err != nil {
		t.Fatal(err)
	}
	defer producer.Close()
	locs := sampleLocations(3)
	if err := NewKafkaPublisher(producer, events.TopicCourierLocations).Publish(context.Background(), locs); err != nil {
		t.Fatal(err)
	}

	consumer, err := cfg.NewClient(kgo.ConsumeTopics(events.TopicCourierLocations), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		t.Fatal(err)
	}
	defer consumer.Close()
	var got []*kgo.Record
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for len(got) < 3 {
		fetches := consumer.PollFetches(ctx)
		if ctx.Err() != nil {
			t.Fatalf("got %d of 3 records", len(got))
		}
		got = append(got, fetches.Records()...)
	}
	for i, r := range got {
		loc, err := events.DecodeCourierLocation(r.Value)
		if err != nil {
			t.Fatal(err)
		}
		if string(r.Key) != loc.CourierID || !r.Timestamp.Equal(now) || got[0].Partition != r.Partition {
			t.Errorf("record %d: key %q, timestamp %v, partition %d", i, r.Key, r.Timestamp, r.Partition)
		}
	}
}

func TestKafkaPublisherShedsWhenKafkaIsDown(t *testing.T) {
	// Nothing listens here, so records stay buffered until they time out.
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	_ = l.Close()
	cl, err := (kit.KafkaConfig{Brokers: []string{addr}}).NewClient(
		kgo.MaxBufferedRecords(1),
		kgo.RecordDeliveryTimeout(2*time.Second), // safety net: nothing in this test may wait forever
	)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	pub := NewKafkaPublisher(cl, events.TopicCourierLocations)

	// Two records, room for one: the second is refused at once instead of waiting.
	if err := pub.Publish(context.Background(), sampleLocations(2)); !errors.Is(err, ErrOverloaded) {
		t.Fatalf("got %v, want ErrOverloaded", err)
	}
}

func TestKafkaPublisherGivesUpAtTheDeadline(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	_ = l.Close()
	cl, err := (kit.KafkaConfig{Brokers: []string{addr}}).NewClient(kgo.RecordDeliveryTimeout(2 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := NewKafkaPublisher(cl, events.TopicCourierLocations).Publish(ctx, sampleLocations(1)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want a timeout", err)
	}
}

func TestKafkaPublisherReportsProduceErrors(t *testing.T) {
	_, cfg := fakeKafka(t)
	cl, err := cfg.NewClient(kgo.UnknownTopicRetries(0))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = NewKafkaPublisher(cl, "no.such.topic").Publish(ctx, sampleLocations(1))
	if err == nil || errors.Is(err, ErrOverloaded) {
		t.Fatalf("got %v, want a produce error", err)
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

func TestRunServesAndShedsEndToEnd(t *testing.T) {
	_, cfg := fakeKafka(t)
	port := freePort(t)
	env := map[string]string{
		"PORT": port, "KAFKA_BROKERS": cfg.Brokers[0], "COURIER_TOKEN_SECRET": secret, "SHUTDOWN_TIMEOUT": "5s",
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, func(k string) string { return env[k] }, discard(), "test") }()

	url := "http://127.0.0.1:" + port
	deadline := time.Now().Add(5 * time.Second)
	for kit.Probe(context.Background(), url+"/readyz") != nil {
		if time.Now().After(deadline) {
			t.Fatal("fleet-ingest never became ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	body := batchJSON(pointJSON(time.Now().UTC(), ""))
	req, _ := http.NewRequest(http.MethodPost, url+"/v1/locations", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+courierToken(t, events.NewID(events.PrefixCourier)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /v1/locations = %d", resp.StatusCode)
	}
	http.DefaultClient.CloseIdleConnections()
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned %v", err)
	}
}

func TestRunRejectsBadConfiguration(t *testing.T) {
	cases := map[string]map[string]string{
		"missing secret": {},
		"short secret":   {"COURIER_TOKEN_SECRET": "short"},
		"bad duration":   {"COURIER_TOKEN_SECRET": secret, "INGEST_PUBLISH_TIMEOUT": "soon"},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			err := Run(context.Background(), func(k string) string { return env[k] }, discard(), "test")
			if err == nil || !strings.Contains(err.Error(), "configuration") {
				t.Fatalf("got %v", err)
			}
		})
	}
}

// Kafka down: fleet-ingest is live and ready, sheds batches with 503 + Retry-After, and still
// exits cleanly on SIGTERM. The CI smoke test checks the same against the container.
func TestRunWithKafkaDown(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := l.Addr().String()
	_ = l.Close()
	port := freePort(t)
	env := map[string]string{"PORT": port, "KAFKA_BROKERS": dead, "COURIER_TOKEN_SECRET": secret,
		"INGEST_PUBLISH_TIMEOUT": "300ms", "SHUTDOWN_TIMEOUT": "20s"}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, func(k string) string { return env[k] }, discard(), "test") }()
	url := "http://127.0.0.1:" + port
	deadline := time.Now().Add(5 * time.Second)
	for kit.Probe(context.Background(), url+"/readyz") != nil {
		if time.Now().After(deadline) {
			t.Fatal("fleet-ingest must be ready even while Kafka is down")
		}
		time.Sleep(20 * time.Millisecond)
	}
	req, _ := http.NewRequest(http.MethodPost, url+"/v1/locations", strings.NewReader(batchJSON(pointJSON(time.Now().UTC(), ""))))
	req.Header.Set("Authorization", "Bearer "+courierToken(t, events.NewID(events.PrefixCourier)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "5" {
		t.Fatalf("got %d, Retry-After %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
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
