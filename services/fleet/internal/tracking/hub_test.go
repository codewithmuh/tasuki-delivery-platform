package tracking

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/events"
)

// Ward centres from contracts/data/wards.json.
var (
	shibuya = [2]float64{35.6640, 139.6982}
	minato  = [2]float64{35.6581, 139.7516}
)

func newTestHub(cfg HubConfig) (*Hub, *Orders, *metrics) {
	if cfg.ViewerBuffer == 0 {
		cfg.ViewerBuffer = 16
	}
	if cfg.MaxViewers == 0 {
		cfg.MaxViewers = 100
	}
	m := newMetrics(prometheus.NewRegistry())
	o := NewOrders()
	return NewHub(cfg, o, m), o, m
}

func location(courier string, at [2]float64, deviceTime time.Time) events.CourierLocation {
	return events.CourierLocation{
		Schema: events.SchemaCourierLocation, EventID: events.NewID(events.PrefixEvent), CourierID: courier,
		Lat: at[0], Lng: at[1], DeviceTime: deviceTime, ReceivedAt: deviceTime.Add(time.Second), Status: events.CourierIdle,
	}
}

// drain returns the event names queued for a viewer, without blocking.
func drain(v *viewer) []string {
	var names []string
	for {
		select {
		case m := <-v.ch:
			line, _, _ := bytes.Cut(m.frame, []byte("\n"))
			names = append(names, strings.TrimPrefix(string(line), "event: "))
		default:
			return names
		}
	}
}

func TestOnLocationFansOutByWardAndKeepsTheNewest(t *testing.T) {
	h, _, _ := newTestHub(HubConfig{})
	all, _ := h.SubscribeFleet("")
	inShibuya, _ := h.SubscribeFleet("shibuya")
	inMinato, _ := h.SubscribeFleet("minato")
	c := events.NewID(events.PrefixCourier)

	h.OnLocation(location(c, shibuya, t0.Add(2*time.Second)))
	h.OnLocation(location(c, minato, t0)) // older: ignored, the courier stays in Shibuya
	if got := drain(all); len(got) != 1 || got[0] != "courier" {
		t.Fatalf("all = %v", got)
	}
	if len(drain(inShibuya)) != 1 || len(drain(inMinato)) != 0 {
		t.Fatal("ward filter not applied")
	}
	snap := h.FleetSnapshot("shibuya")
	if len(snap) != 1 || !bytes.Contains(snap[0].frame, []byte(`"ward":"shibuya"`)) || len(h.FleetSnapshot("minato")) != 0 {
		t.Fatalf("snapshot = %d events", len(snap))
	}
	if h.Couriers() != 1 {
		t.Fatal("expected one courier")
	}
}

func TestOutsideTokyoHasANullWard(t *testing.T) {
	h, _, _ := newTestHub(HubConfig{})
	h.OnLocation(location(events.NewID(events.PrefixCourier), [2]float64{34.69, 135.50}, t0)) // Osaka
	snap := h.FleetSnapshot("")
	if len(snap) != 1 || !bytes.Contains(snap[0].frame, []byte(`"ward":null`)) {
		t.Fatalf("snapshot = %d events", len(snap))
	}
}

func TestTrackViewersFollowTheAssignedCourier(t *testing.T) {
	h, orders, _ := newTestHub(HubConfig{})
	id, courier, other := events.NewID(events.PrefixOrder), events.NewID(events.PrefixCourier), events.NewID(events.PrefixCourier)
	v, _ := h.SubscribeOrder(id)

	h.OnLocation(location(courier, shibuya, t0)) // not assigned yet
	if got := drain(v); len(got) != 0 {
		t.Fatalf("got %v before assignment", got)
	}
	view, _ := orders.ApplyDispatchEvent(assigned(id, courier, t0, 60, 60))
	h.OnOrderUpdate(view)
	if got := drain(v); strings.Join(got, ",") != "status,position" {
		t.Fatalf("on assignment got %v", got)
	}
	h.OnLocation(location(courier, shibuya, t0.Add(4*time.Second)))
	h.OnLocation(location(other, shibuya, t0.Add(4*time.Second)))
	if got := drain(v); strings.Join(got, ",") != "position" {
		t.Fatalf("got %v", got)
	}
	snap := h.OrderSnapshot(id)
	if len(snap) != 2 || snap[1].receivedAt.IsZero() {
		t.Fatalf("snapshot has %d events", len(snap))
	}

	view, _ = orders.ApplyOrderEvent(orderEvent(id, events.OrderDelivered, t0.Add(time.Minute)))
	h.OnOrderUpdate(view)
	got := drain(v)
	if strings.Join(got, ",") != "status,end" {
		t.Fatalf("on delivery got %v", got)
	}
	h.OnLocation(location(courier, shibuya, t0.Add(8*time.Second)))
	if len(drain(v)) != 0 {
		t.Fatal("no positions after the order finished")
	}
	if snap := h.OrderSnapshot(id); len(snap) != 2 || !snap[1].final {
		t.Fatal("a finished order's snapshot ends the stream")
	}
	if h.OrderSnapshot(events.NewID(events.PrefixOrder)) != nil {
		t.Fatal("unknown order has no snapshot")
	}
	h.Unsubscribe(v) // already released by the terminal update: must be harmless
	h.Unsubscribe(v)
}

func TestSlowViewerDropsOldestInsteadOfBlocking(t *testing.T) {
	h, _, m := newTestHub(HubConfig{ViewerBuffer: 2})
	v, _ := h.SubscribeFleet("")
	c := events.NewID(events.PrefixCourier)
	done := make(chan struct{})
	go func() {
		for i := range 100 {
			h.OnLocation(location(c, shibuya, t0.Add(time.Duration(i)*time.Second)))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a viewer that never reads blocked the consumer")
	}
	if got := testutil.ToFloat64(m.dropped.WithLabelValues(streamFleet)); got != 98 {
		t.Fatalf("dropped = %v, want 98", got)
	}
	// What is left is the newest data.
	<-v.ch
	last := <-v.ch
	if !bytes.Contains(last.frame, []byte(t0.Add(99*time.Second).Format(time.RFC3339))) {
		t.Fatalf("kept %s, want the newest point", last.frame)
	}
}

func TestBrownoutThrottlesPerCourier(t *testing.T) {
	h, _, m := newTestHub(HubConfig{MinInterval: 10 * time.Second})
	clock := t0
	h.now = func() time.Time { return clock }
	v, _ := h.SubscribeFleet("")
	a, b := events.NewID(events.PrefixCourier), events.NewID(events.PrefixCourier)
	h.OnLocation(location(a, shibuya, t0))
	h.OnLocation(location(b, shibuya, t0)) // another courier is not throttled by a
	clock = clock.Add(4 * time.Second)
	h.OnLocation(location(a, shibuya, t0.Add(4*time.Second))) // too soon: skipped
	clock = clock.Add(8 * time.Second)
	h.OnLocation(location(a, shibuya, t0.Add(12*time.Second)))
	if got := len(drain(v)); got != 3 {
		t.Fatalf("delivered %d positions, want 3", got)
	}
	if testutil.ToFloat64(m.throttled.WithLabelValues(streamFleet)) != 1 {
		t.Fatal("throttled event not counted")
	}
}

func TestSweepGoneAnnouncesQuietCouriers(t *testing.T) {
	h, _, _ := newTestHub(HubConfig{MinInterval: time.Hour})
	all, _ := h.SubscribeFleet("")
	inMinato, _ := h.SubscribeFleet("minato")
	inShibuya, _ := h.SubscribeFleet("shibuya")
	quiet, active := events.NewID(events.PrefixCourier), events.NewID(events.PrefixCourier)
	h.OnLocation(location(quiet, shibuya, t0))
	h.OnLocation(location(active, shibuya, t0.Add(time.Minute)))
	drain(all)
	drain(inShibuya)
	if n := h.SweepGone(t0.Add(30 * time.Second)); n != 1 {
		t.Fatalf("swept %d", n)
	}
	if got := drain(all); len(got) != 1 || got[0] != "courier_gone" {
		t.Fatalf("got %v", got)
	}
	if got := drain(inShibuya); len(got) != 1 || got[0] != "courier_gone" {
		t.Fatalf("Shibuya viewer got %v", got)
	}
	if len(drain(inMinato)) != 0 {
		t.Fatal("a Shibuya courier is not announced to a Minato viewer")
	}
	// The brownout memory is cleared, so a returning courier shows up at once.
	h.OnLocation(location(quiet, shibuya, t0.Add(2*time.Minute)))
	if len(drain(all)) != 1 {
		t.Fatal("returning courier was throttled")
	}
}

func TestViewerLimit(t *testing.T) {
	h, _, _ := newTestHub(HubConfig{MaxViewers: 2})
	a, _ := h.SubscribeFleet("")
	if _, err := h.SubscribeOrder(events.NewID(events.PrefixOrder)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.SubscribeFleet(""); !errors.Is(err, ErrTooManyViewers) {
		t.Fatalf("got %v", err)
	}
	if _, err := h.SubscribeOrder("x"); !errors.Is(err, ErrTooManyViewers) {
		t.Fatalf("got %v", err)
	}
	h.Unsubscribe(a)
	h.Unsubscribe(a)
	if _, err := h.SubscribeFleet(""); err != nil {
		t.Fatalf("a slot should be free: %v", err)
	}
}

func TestUnencodableEventsBecomeComments(t *testing.T) {
	m := newMessage("x", func() {})
	if !bytes.HasPrefix(m.frame, []byte(": ")) {
		t.Fatalf("frame = %q", m.frame)
	}
}

func TestWardChangeTellsTheOldWard(t *testing.T) {
	h, _, _ := newTestHub(HubConfig{})
	inShibuya, _ := h.SubscribeFleet("shibuya")
	inMinato, _ := h.SubscribeFleet("minato")
	all, _ := h.SubscribeFleet("")
	c := events.NewID(events.PrefixCourier)
	h.OnLocation(location(c, shibuya, t0))
	drain(inShibuya)
	drain(all)

	h.OnLocation(location(c, minato, t0.Add(4*time.Second)))
	if got := drain(inShibuya); len(got) != 1 || got[0] != "courier_gone" {
		t.Fatalf("old ward got %v", got)
	}
	if got := drain(inMinato); len(got) != 1 || got[0] != "courier" {
		t.Fatalf("new ward got %v", got)
	}
	if got := drain(all); len(got) != 1 || got[0] != "courier" {
		t.Fatalf("all-wards viewer got %v", got)
	}
	h.OnLocation(location(c, minato, t0.Add(8*time.Second))) // same ward: no courier_gone
	if got := drain(inShibuya); len(got) != 0 {
		t.Fatalf("old ward got %v after the courier stayed in Minato", got)
	}
}
