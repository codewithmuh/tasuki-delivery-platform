package tracking

import (
	"sync"
	"time"

	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/events"
)

// OrderStatus is the status the tracking page shows (contracts/README.md, track stream "status").
type OrderStatus string

// Order statuses, in lifecycle order.
const (
	StatusConfirmed OrderStatus = "confirmed"
	StatusAssigned  OrderStatus = "assigned"
	StatusPickedUp  OrderStatus = "picked_up"
	StatusDelivered OrderStatus = "delivered"
	StatusCancelled OrderStatus = "cancelled"
	StatusFailed    OrderStatus = "failed"
)

// rank places a status on the lifecycle. Events come from two topics with no order between them,
// so a status only ever moves forward: a late OrderConfirmed cannot undo a pickup. That makes
// applying events order-independent and idempotent, which is what at-least-once delivery needs.
func (s OrderStatus) rank() int {
	switch s {
	case StatusConfirmed:
		return 1
	case StatusAssigned:
		return 2
	case StatusPickedUp:
		return 3
	case StatusDelivered, StatusCancelled, StatusFailed:
		return 4
	}
	return 0
}

// Terminal reports whether the order is finished; its stream ends with an "end" event.
func (s OrderStatus) Terminal() bool { return s.rank() == 4 }

// OrderView is what a tracking viewer sees of one order.
type OrderView struct {
	OrderID   string
	Status    OrderStatus
	CourierID string     // "" until a courier is assigned
	ETA       *time.Time // the promised delivery time; nil until an order event has been seen
	UpdatedAt time.Time  // occurred_at of the event behind the last visible change
}

func (v OrderView) same(o OrderView) bool {
	sameETA := (v.ETA == nil && o.ETA == nil) || (v.ETA != nil && o.ETA != nil && v.ETA.Equal(*o.ETA))
	return v.Status == o.Status && v.CourierID == o.CourierID && sameETA
}

type order struct {
	view        OrderView
	promisedAt  time.Time
	assignedAt  time.Time // occurred_at of the assignment in effect: the newest one wins
	lastEventAt time.Time // drives eviction
}

// Orders maps each order to its status and courier, folded from orders.events and dispatch.events.
type Orders struct {
	mu        sync.RWMutex
	orders    map[string]*order
	byCourier map[string]map[string]struct{} // courier ID -> its unfinished orders
}

// NewOrders returns an empty view.
func NewOrders() *Orders {
	return &Orders{orders: map[string]*order{}, byCourier: map[string]map[string]struct{}{}}
}

// ApplyOrderEvent folds in an orders.events message. It returns the order's view and whether
// anything a viewer sees changed.
func (o *Orders) ApplyOrderEvent(e events.OrderEvent) (OrderView, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	ord := o.get(e.OrderID)
	before := ord.view
	if !e.Order.PromisedAt.IsZero() {
		ord.promisedAt = e.Order.PromisedAt
	}
	switch e.Type {
	case events.OrderConfirmed:
		o.advance(ord, StatusConfirmed)
	case events.OrderPickedUp:
		if ord.view.CourierID == "" && e.Order.CourierID != nil {
			o.assign(ord, *e.Order.CourierID, e.OccurredAt) // the assignment event may still be on its way
		}
		o.advance(ord, StatusPickedUp)
	case events.OrderDelivered:
		o.advance(ord, StatusDelivered)
	case events.OrderCancelled:
		o.advance(ord, StatusCancelled)
	case events.OrderFailed:
		o.advance(ord, StatusFailed)
	}
	return o.finish(ord, before, e.OccurredAt)
}

// ApplyDispatchEvent folds in a dispatch.events message. Only CourierAssigned changes what a
// viewer sees; other types just make the order known.
func (o *Orders) ApplyDispatchEvent(e events.DispatchEvent) (OrderView, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	ord := o.get(e.OrderID)
	before := ord.view
	// A reassignment produces a second CourierAssigned; the newest one wins, whatever the
	// arrival order. A finished order is never reassigned.
	if e.Type == events.CourierAssigned && e.CourierID != nil && !ord.view.Status.Terminal() &&
		!e.OccurredAt.Before(ord.assignedAt) {
		o.assign(ord, *e.CourierID, e.OccurredAt)
		o.advance(ord, StatusAssigned)
	}
	return o.finish(ord, before, e.OccurredAt)
}

// get returns the order, creating it as "confirmed": any order that has events is at least that.
func (o *Orders) get(id string) *order {
	ord, ok := o.orders[id]
	if !ok {
		ord = &order{view: OrderView{OrderID: id, Status: StatusConfirmed}}
		o.orders[id] = ord
	}
	return ord
}

func (o *Orders) advance(ord *order, s OrderStatus) {
	if s.rank() > ord.view.Status.rank() {
		ord.view.Status = s
	}
	if ord.view.Status.Terminal() {
		o.unindex(ord) // no more positions for a finished order
	}
}

func (o *Orders) assign(ord *order, courierID string, at time.Time) {
	o.unindex(ord)
	ord.view.CourierID, ord.assignedAt = courierID, at
	if !ord.view.Status.Terminal() {
		if o.byCourier[courierID] == nil {
			o.byCourier[courierID] = map[string]struct{}{}
		}
		o.byCourier[courierID][ord.view.OrderID] = struct{}{}
	}
}

func (o *Orders) unindex(ord *order) {
	if set := o.byCourier[ord.view.CourierID]; set != nil {
		delete(set, ord.view.OrderID)
		if len(set) == 0 {
			delete(o.byCourier, ord.view.CourierID)
		}
	}
}

func (o *Orders) finish(ord *order, before OrderView, at time.Time) (OrderView, bool) {
	if at.After(ord.lastEventAt) {
		ord.lastEventAt = at
	}
	ord.view.ETA = ord.eta()
	changed := !before.same(ord.view)
	if changed && at.After(ord.view.UpdatedAt) {
		ord.view.UpdatedAt = at
	}
	if ord.view.UpdatedAt.IsZero() {
		ord.view.UpdatedAt = at
	}
	return ord.view, changed
}

// eta is the delivery time promised at confirmation (contracts/README.md: "eta" is an RFC 3339
// time or null). A live estimate from the courier's position can replace it later.
func (ord *order) eta() *time.Time {
	if ord.promisedAt.IsZero() {
		return nil // only dispatch events seen so far
	}
	t := ord.promisedAt.UTC()
	return &t
}

// Get returns the view of one order.
func (o *Orders) Get(id string) (OrderView, bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	ord, ok := o.orders[id]
	if !ok {
		return OrderView{}, false
	}
	return ord.view, true
}

// OrdersOf returns the unfinished orders a courier carries (several when orders are stacked).
func (o *Orders) OrdersOf(courierID string) []string {
	o.mu.RLock()
	defer o.mu.RUnlock()
	ids := make([]string, 0, len(o.byCourier[courierID]))
	for id := range o.byCourier[courierID] {
		ids = append(ids, id)
	}
	return ids
}

// Evict forgets orders whose last event happened before cutoff, so memory stays bounded by
// ORDER_STATE_TTL rather than by topic retention. It returns how many it removed.
func (o *Orders) Evict(cutoff time.Time) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := 0
	for id, ord := range o.orders {
		if ord.lastEventAt.Before(cutoff) {
			o.unindex(ord)
			delete(o.orders, id)
			n++
		}
	}
	return n
}

// Len returns how many orders are known.
func (o *Orders) Len() int {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return len(o.orders)
}
