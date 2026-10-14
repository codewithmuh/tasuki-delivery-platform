package tracking

import (
	"slices"
	"testing"
	"time"

	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/events"
)

var t0 = time.Date(2026, 10, 14, 3, 0, 0, 0, time.UTC)

func orderEvent(id string, typ events.OrderEventType, at time.Time) events.OrderEvent {
	return events.OrderEvent{
		Schema: events.SchemaOrderEvent, EventID: events.NewID(events.PrefixEvent), Type: typ, OrderID: id,
		MerchantID: events.NewID(events.PrefixMerchant), OccurredAt: at,
		Order: events.OrderDetails{
			Store:      events.Store{ID: events.NewID(events.PrefixStore), Lat: 35.66, Lng: 139.70, Ward: "shibuya"},
			Dropoff:    events.Place{Lat: 35.65, Lng: 139.75, Ward: "minato"},
			PromisedAt: t0.Add(40 * time.Minute), FeeYen: 660,
		},
	}
}

func assigned(id, courier string, at time.Time, pickupS, dropoffS int) events.DispatchEvent {
	return events.DispatchEvent{
		Schema: events.SchemaDispatchEvent, EventID: events.NewID(events.PrefixEvent), Type: events.CourierAssigned,
		OrderID: id, CourierID: &courier, OccurredAt: at, EtaPickupS: &pickupS, EtaDropoffS: &dropoffS,
	}
}

func TestOrderLifecycle(t *testing.T) {
	o := NewOrders()
	id, courier := events.NewID(events.PrefixOrder), events.NewID(events.PrefixCourier)

	v, changed := o.ApplyOrderEvent(orderEvent(id, events.OrderConfirmed, t0))
	if !changed || v.Status != StatusConfirmed || v.ETA == nil || !v.ETA.Equal(t0.Add(40*time.Minute)) {
		t.Fatalf("confirmed: %+v changed=%v", v, changed)
	}
	if _, changed := o.ApplyOrderEvent(orderEvent(id, events.OrderConfirmed, t0)); changed {
		t.Fatal("a duplicate event must not count as a change")
	}

	v, changed = o.ApplyDispatchEvent(assigned(id, courier, t0.Add(time.Minute), 300, 900))
	if !changed || v.Status != StatusAssigned || v.CourierID != courier || !v.ETA.Equal(t0.Add(40*time.Minute)) {
		t.Fatalf("assigned: %+v", v)
	}
	if !slices.Equal(o.OrdersOf(courier), []string{id}) {
		t.Fatal("courier index missing the order")
	}

	v, _ = o.ApplyOrderEvent(orderEvent(id, events.OrderPickedUp, t0.Add(10*time.Minute)))
	if v.Status != StatusPickedUp || !v.UpdatedAt.Equal(t0.Add(10*time.Minute)) {
		t.Fatalf("picked up: %+v", v)
	}

	v, _ = o.ApplyOrderEvent(orderEvent(id, events.OrderDelivered, t0.Add(24*time.Minute)))
	if v.Status != StatusDelivered || !v.Status.Terminal() {
		t.Fatalf("delivered: %+v", v)
	}
	if len(o.OrdersOf(courier)) != 0 {
		t.Fatal("a delivered order must leave the courier index")
	}
	// A late event never moves a finished order backwards, nor reassigns it.
	v, _ = o.ApplyOrderEvent(orderEvent(id, events.OrderConfirmed, t0))
	v2, _ := o.ApplyDispatchEvent(assigned(id, events.NewID(events.PrefixCourier), t0.Add(30*time.Minute), 1, 1))
	if v.Status != StatusDelivered || v2.CourierID != courier {
		t.Fatalf("late events changed a finished order: %+v %+v", v, v2)
	}
}

func TestEventsInAnyOrderGiveTheSameResult(t *testing.T) {
	id, courier := events.NewID(events.PrefixOrder), events.NewID(events.PrefixCourier)
	evs := []func(*Orders) (OrderView, bool){
		func(o *Orders) (OrderView, bool) { return o.ApplyOrderEvent(orderEvent(id, events.OrderConfirmed, t0)) },
		func(o *Orders) (OrderView, bool) {
			return o.ApplyDispatchEvent(assigned(id, courier, t0.Add(time.Minute), 60, 60))
		},
		func(o *Orders) (OrderView, bool) {
			return o.ApplyOrderEvent(orderEvent(id, events.OrderPickedUp, t0.Add(5*time.Minute)))
		},
	}
	for _, perm := range [][]int{{0, 1, 2}, {2, 1, 0}, {1, 2, 0}, {2, 0, 1}} {
		o := NewOrders()
		var v OrderView
		for _, i := range perm {
			v, _ = evs[i](o)
		}
		if v.Status != StatusPickedUp || v.CourierID != courier {
			t.Errorf("order %v: %+v", perm, v)
		}
	}
}

func TestReassignmentNewestWins(t *testing.T) {
	o := NewOrders()
	id := events.NewID(events.PrefixOrder)
	first, second := events.NewID(events.PrefixCourier), events.NewID(events.PrefixCourier)
	o.ApplyDispatchEvent(assigned(id, second, t0.Add(2*time.Minute), 60, 60))
	v, changed := o.ApplyDispatchEvent(assigned(id, first, t0.Add(time.Minute), 60, 60)) // older, arrives late
	if changed || v.CourierID != second {
		t.Fatalf("an older assignment won: %+v", v)
	}
	v, _ = o.ApplyDispatchEvent(assigned(id, first, t0.Add(3*time.Minute), 60, 60))
	if v.CourierID != first || len(o.OrdersOf(second)) != 0 || len(o.OrdersOf(first)) != 1 {
		t.Fatalf("reassignment: %+v", v)
	}
}

func TestETAIsThePromisedTime(t *testing.T) {
	o := NewOrders()
	id := events.NewID(events.PrefixOrder)
	v, _ := o.ApplyDispatchEvent(assigned(id, events.NewID(events.PrefixCourier), t0, 300, 600))
	if v.ETA != nil {
		t.Fatalf("ETA before any order event = %v, want null", v.ETA)
	}
	v, changed := o.ApplyOrderEvent(orderEvent(id, events.OrderConfirmed, t0))
	if !changed || v.ETA == nil || !v.ETA.Equal(t0.Add(40*time.Minute)) || v.ETA.Location() != time.UTC {
		t.Fatalf("ETA = %v", v.ETA)
	}
}

func TestPickupCarriesTheCourierWhenAssignmentIsLate(t *testing.T) {
	o := NewOrders()
	id, courier := events.NewID(events.PrefixOrder), events.NewID(events.PrefixCourier)
	e := orderEvent(id, events.OrderPickedUp, t0)
	e.Order.CourierID = &courier
	v, _ := o.ApplyOrderEvent(e)
	if v.CourierID != courier || v.Status != StatusPickedUp || len(o.OrdersOf(courier)) != 1 {
		t.Fatalf("got %+v", v)
	}
}

func TestOtherEventsOnlyMakeTheOrderKnown(t *testing.T) {
	o := NewOrders()
	id := events.NewID(events.PrefixOrder)
	v, _ := o.ApplyDispatchEvent(events.DispatchEvent{Schema: events.SchemaDispatchEvent, EventID: events.NewID(events.PrefixEvent),
		Type: events.OfferSent, OrderID: id, OccurredAt: t0})
	if v.Status != StatusConfirmed || v.ETA != nil || v.UpdatedAt != t0 {
		t.Fatalf("got %+v", v)
	}
	if _, ok := o.Get(id); !ok || o.Len() != 1 {
		t.Fatal("order not known")
	}
	if _, ok := o.Get(events.NewID(events.PrefixOrder)); ok {
		t.Fatal("unknown order found")
	}
	for _, typ := range []events.OrderEventType{events.OrderCancelled, events.OrderFailed} {
		v, _ := o.ApplyOrderEvent(orderEvent(events.NewID(events.PrefixOrder), typ, t0))
		if !v.Status.Terminal() {
			t.Errorf("%s is terminal", typ)
		}
	}
	if OrderStatus("weird").rank() != 0 {
		t.Error("unknown statuses rank lowest")
	}
}

func TestEvict(t *testing.T) {
	o := NewOrders()
	old, recent := events.NewID(events.PrefixOrder), events.NewID(events.PrefixOrder)
	courier := events.NewID(events.PrefixCourier)
	o.ApplyDispatchEvent(assigned(old, courier, t0, 1, 1))
	o.ApplyOrderEvent(orderEvent(recent, events.OrderConfirmed, t0.Add(time.Hour)))
	if n := o.Evict(t0.Add(30 * time.Minute)); n != 1 {
		t.Fatalf("evicted %d", n)
	}
	if _, ok := o.Get(old); ok || len(o.OrdersOf(courier)) != 0 {
		t.Fatal("old order still known")
	}
	if _, ok := o.Get(recent); !ok {
		t.Fatal("recent order evicted")
	}
}
