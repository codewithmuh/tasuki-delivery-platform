package viewer

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/clients"
	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/metrics"
)

// fakeTracker plays a fixed list of events, then blocks until release is closed (or returns err).
type fakeTracker struct {
	events  []clients.TrackEvent
	release chan struct{}
	err     error
	tokens  chan string
}

func (f *fakeTracker) Watch(ctx context.Context, token string, handle func(clients.TrackEvent)) error {
	f.tokens <- token
	for _, ev := range f.events {
		handle(ev)
	}
	select {
	case <-f.release:
		return f.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func newPool(t *testing.T, tracker *fakeTracker, maxViewers int) (*Pool, *metrics.Metrics, context.CancelFunc) {
	t.Helper()
	pool, m, _, cancel := newPoolWithRegistry(t, tracker, maxViewers)
	return pool, m, cancel
}

func newPoolWithRegistry(t *testing.T, tracker *fakeTracker, maxViewers int) (*Pool, *metrics.Metrics, *prometheus.Registry, context.CancelFunc) {
	t.Helper()
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	ctx, cancel := context.WithCancel(context.Background())
	return NewPool(ctx, tracker, maxViewers, m, nil), m, reg, cancel
}

func TestViewersRecordWhatTheySeeAndFinish(t *testing.T) {
	received := time.Date(2026, 10, 16, 3, 0, 0, 0, time.UTC)
	tracker := &fakeTracker{
		events: []clients.TrackEvent{
			{Status: &clients.TrackStatus{Status: "assigned"}},
			{Position: &clients.TrackPosition{CourierID: "cou_1", ReceivedAt: received}},
			{End: "delivered"},
		},
		release: make(chan struct{}),
		tokens:  make(chan string, 1),
	}
	pool, m, reg, cancel := newPoolWithRegistry(t, tracker, 10)
	defer cancel()
	pool.now = func() time.Time { return received.Add(1500 * time.Millisecond) }

	pool.Watch("ord_1", "http://localhost:8088/track/tok_1")
	if got := <-tracker.tokens; got != "tok_1" {
		t.Fatalf("watched token %q, want tok_1", got)
	}
	if got := testutil.ToFloat64(m.TrackingViewers); got != 1 {
		t.Errorf("viewers gauge = %v while watching, want 1", got)
	}
	close(tracker.release)
	pool.Wait()

	if got := testutil.ToFloat64(m.TrackingViewers); got != 0 {
		t.Errorf("viewers gauge = %v after the stream ended, want 0", got)
	}
	if got := testutil.ToFloat64(m.TrackingStreams.WithLabelValues("ended")); got != 1 {
		t.Errorf("ended streams = %v, want 1", got)
	}
	for _, event := range []string{"status", "position", "end"} {
		if got := testutil.ToFloat64(m.TrackingEvents.WithLabelValues(event)); got != 1 {
			t.Errorf("%s events = %v, want 1", event, got)
		}
	}
	// One position, received 1.5 s before the viewer got it.
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, mf := range families {
		if mf.GetName() != "simulator_tracking_position_age_seconds" {
			continue
		}
		h := mf.GetMetric()[0].GetHistogram()
		found = h.GetSampleCount() == 1 && h.GetSampleSum() == 1.5
		if !found {
			t.Errorf("position age: %d samples summing to %v, want 1 of 1.5 s", h.GetSampleCount(), h.GetSampleSum())
		}
	}
	if !found {
		t.Error("no position age recorded")
	}
}

func TestThePoolIsCappedAndSkipsBadURLs(t *testing.T) {
	tracker := &fakeTracker{release: make(chan struct{}), tokens: make(chan string, 4)}
	pool, m, cancel := newPool(t, tracker, 2)

	pool.Watch("ord_1", "http://localhost:8088/track/tok_1")
	pool.Watch("ord_2", "http://localhost:8088/track/tok_2")
	<-tracker.tokens
	<-tracker.tokens
	pool.Watch("ord_3", "http://localhost:8088/track/tok_3")  // full
	pool.Watch("ord_4", "http://localhost:8088/orders/ord_4") // no token

	if got := testutil.ToFloat64(m.TrackingStreams.WithLabelValues("capped")); got != 1 {
		t.Errorf("capped = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.TrackingStreams.WithLabelValues("bad_url")); got != 1 {
		t.Errorf("bad_url = %v, want 1", got)
	}
	cancel() // the simulation stops
	pool.Wait()
	if got := testutil.ToFloat64(m.TrackingStreams.WithLabelValues("stopped")); got != 2 {
		t.Errorf("stopped = %v, want 2", got)
	}
}

func TestFailedStreamsAreCountedByReason(t *testing.T) {
	tracker := &fakeTracker{
		release: make(chan struct{}), tokens: make(chan string, 1),
		err: &clients.APIError{Status: 401, Code: "invalid_token"},
	}
	close(tracker.release)
	pool, m, cancel := newPool(t, tracker, 1)
	defer cancel()
	pool.Watch("ord_1", "http://localhost:8088/track/tok_1")
	pool.Wait()
	if got := testutil.ToFloat64(m.TrackingStreams.WithLabelValues("http_401")); got != 1 {
		t.Errorf("http_401 = %v, want 1", got)
	}
}
