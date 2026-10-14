package tracking

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/events"
	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/wards"
)

// Stream names, used as the "stream" metric label.
const (
	streamTrack = "track"
	streamFleet = "fleet"
)

// ErrTooManyViewers is returned when this replica already holds TRACKING_MAX_VIEWERS streams.
var ErrTooManyViewers = errors.New("too many viewers")

// message is one server-sent event, encoded once and shared by every viewer that gets it.
type message struct {
	frame      []byte    // "event: ...\ndata: ...\n\n"
	courierID  string    // set on position events: brownout throttling is per courier
	receivedAt time.Time // set on position events: the tracking freshness SLI measures from it
	final      bool      // the stream closes after this event
}

func newMessage(event string, data any) message {
	b, err := json.Marshal(data)
	if err != nil {
		// Only plain structs are encoded here; the one theoretical failure is a time beyond year
		// 9999. Send an SSE comment, which clients ignore, rather than crash the consumer.
		return message{frame: fmt.Appendf(nil, ": dropped unencodable %s event\n\n", event)}
	}
	return message{frame: fmt.Appendf(nil, "event: %s\ndata: %s\n\n", event, b)}
}

// Event payloads (contracts/README.md, "Server-sent event streams").
type courierData struct {
	CourierID  string               `json:"courier_id"`
	Lat        float64              `json:"lat"`
	Lng        float64              `json:"lng"`
	Status     events.CourierStatus `json:"status"`
	OrderID    *string              `json:"order_id"`
	Ward       *string              `json:"ward"` // nearest ward centre; null outside the 23 wards
	DeviceTime time.Time            `json:"device_time"`
	ReceivedAt time.Time            `json:"received_at"`
}

type courierGoneData struct {
	CourierID string `json:"courier_id"`
}

type statusData struct {
	OrderID   string      `json:"order_id"`
	Status    OrderStatus `json:"status"`
	ETA       *time.Time  `json:"eta"`
	UpdatedAt time.Time   `json:"updated_at"`
}

type positionData struct {
	CourierID  string    `json:"courier_id"`
	Lat        float64   `json:"lat"`
	Lng        float64   `json:"lng"`
	ReceivedAt time.Time `json:"received_at"`
}

type endData struct {
	Reason string `json:"reason"` // delivered, cancelled, failed, expired
}

func courierMessage(p position) message {
	var ward *string
	if p.ward != "" {
		ward = &p.ward
	}
	m := newMessage("courier", courierData{
		CourierID: p.loc.CourierID, Lat: p.loc.Lat, Lng: p.loc.Lng, Status: p.loc.Status,
		OrderID: p.loc.OrderID, Ward: ward, DeviceTime: p.loc.DeviceTime.UTC(), ReceivedAt: p.loc.ReceivedAt.UTC(),
	})
	m.courierID, m.receivedAt = p.loc.CourierID, p.loc.ReceivedAt
	return m
}

func positionMessage(p position) message {
	m := newMessage("position", positionData{
		CourierID: p.loc.CourierID, Lat: p.loc.Lat, Lng: p.loc.Lng, ReceivedAt: p.loc.ReceivedAt.UTC(),
	})
	m.courierID, m.receivedAt = p.loc.CourierID, p.loc.ReceivedAt
	return m
}

func statusMessage(v OrderView) message {
	return newMessage("status", statusData{OrderID: v.OrderID, Status: v.Status, ETA: v.ETA, UpdatedAt: v.UpdatedAt.UTC()})
}

func endMessage(reason string) message {
	m := newMessage("end", endData{Reason: reason})
	m.final = true
	return m
}

// position is the newest known point of a courier and the ward it is in.
type position struct {
	loc  events.CourierLocation
	ward string
}

// viewer is one open stream. The consumer side pushes into ch without ever blocking; the
// connection's own goroutine drains it at whatever speed the client reads.
type viewer struct {
	stream  string
	orderID string // track streams
	ward    string // fleet streams; "" = all wards
	ch      chan message

	minInterval time.Duration
	mu          sync.Mutex
	lastSent    map[string]time.Time // courier -> last position pushed (brownout)

	dropped   prometheus.Counter
	throttled prometheus.Counter
}

// push queues m. When the buffer is full the oldest queued event is dropped: for live positions
// the newest one is the one worth keeping, and a slow client must never stall the consumer.
func (v *viewer) push(m message) {
	for {
		select {
		case v.ch <- m:
			return
		default:
		}
		select {
		case <-v.ch:
			v.dropped.Inc()
		default:
		}
	}
}

// pushPosition applies the brownout knob first: at most one position per courier per minInterval.
func (v *viewer) pushPosition(m message, now time.Time) {
	if v.minInterval > 0 {
		v.mu.Lock()
		if now.Sub(v.lastSent[m.courierID]) < v.minInterval {
			v.mu.Unlock()
			v.throttled.Inc()
			return
		}
		v.lastSent[m.courierID] = now
		v.mu.Unlock()
	}
	v.push(m)
}

func (v *viewer) forget(courierID string) {
	v.mu.Lock()
	delete(v.lastSent, courierID)
	v.mu.Unlock()
}

// HubConfig tunes the fan-out.
type HubConfig struct {
	ViewerBuffer int           // TRACKING_VIEWER_BUFFER: events queued per stream before dropping
	MaxViewers   int           // TRACKING_MAX_VIEWERS: streams per replica
	MinInterval  time.Duration // TRACKING_MIN_INTERVAL: brownout; 0 = every position
}

// Hub holds the newest position of every courier and fans events out to open streams.
type Hub struct {
	cfg     HubConfig
	orders  *Orders
	metrics *metrics
	now     func() time.Time

	mu        sync.Mutex
	positions map[string]position
	byOrder   map[string]map[*viewer]struct{}
	fleet     map[*viewer]struct{}
	viewers   int
}

// NewHub returns an empty hub.
func NewHub(cfg HubConfig, orders *Orders, m *metrics) *Hub {
	return &Hub{
		cfg: cfg, orders: orders, metrics: m, now: time.Now,
		positions: map[string]position{},
		byOrder:   map[string]map[*viewer]struct{}{},
		fleet:     map[*viewer]struct{}{},
	}
}

func (h *Hub) newViewer(stream string) *viewer {
	return &viewer{
		stream: stream, ch: make(chan message, h.cfg.ViewerBuffer), minInterval: h.cfg.MinInterval,
		lastSent: map[string]time.Time{},
		dropped:  h.metrics.dropped.WithLabelValues(stream), throttled: h.metrics.throttled.WithLabelValues(stream),
	}
}

// SubscribeOrder opens a track stream for one order.
func (h *Hub) SubscribeOrder(orderID string) (*viewer, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.viewers >= h.cfg.MaxViewers {
		return nil, ErrTooManyViewers
	}
	v := h.newViewer(streamTrack)
	v.orderID = orderID
	if h.byOrder[orderID] == nil {
		h.byOrder[orderID] = map[*viewer]struct{}{}
	}
	h.byOrder[orderID][v] = struct{}{}
	h.viewers++
	return v, nil
}

// SubscribeFleet opens a fleet stream for one ward, or for all wards when ward is "".
func (h *Hub) SubscribeFleet(ward string) (*viewer, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.viewers >= h.cfg.MaxViewers {
		return nil, ErrTooManyViewers
	}
	v := h.newViewer(streamFleet)
	v.ward = ward
	h.fleet[v] = struct{}{}
	h.viewers++
	return v, nil
}

// Unsubscribe removes a viewer. It is safe to call twice.
func (h *Hub) Unsubscribe(v *viewer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if v.stream == streamFleet {
		if _, ok := h.fleet[v]; ok {
			delete(h.fleet, v)
			h.viewers--
		}
		return
	}
	if set := h.byOrder[v.orderID]; set != nil {
		if _, ok := set[v]; ok {
			delete(set, v)
			h.viewers--
		}
		if len(set) == 0 {
			delete(h.byOrder, v.orderID)
		}
	}
}

// OnLocation handles one point from courier.locations: keep it if it is the newest for its
// courier, then send it to fleet viewers of its ward and to viewers of the courier's orders.
func (h *Hub) OnLocation(loc events.CourierLocation) {
	p := position{loc: loc, ward: wards.Nearest(loc.Lat, loc.Lng)}
	orderIDs := h.orders.OrdersOf(loc.CourierID)

	h.mu.Lock()
	prev, known := h.positions[loc.CourierID]
	if known && !loc.DeviceTime.After(prev.loc.DeviceTime) {
		h.mu.Unlock()
		return // older or duplicate point: latest wins, as in fleet-state
	}
	h.positions[loc.CourierID] = p
	fleet := h.fleetViewersLocked(p.ward)
	// Viewers filtered on the ward the courier just left must drop it from their map.
	var leftWard []*viewer
	if known && prev.ward != p.ward && prev.ward != "" {
		for v := range h.fleet {
			if v.ward == prev.ward {
				leftWard = append(leftWard, v)
			}
		}
	}
	var track []*viewer
	for _, id := range orderIDs {
		for v := range h.byOrder[id] {
			track = append(track, v)
		}
	}
	h.mu.Unlock()

	// Push outside the lock. A viewer that unsubscribed meanwhile just gets an event nobody reads.
	now := h.now()
	if len(leftWard) > 0 {
		gone := newMessage("courier_gone", courierGoneData{CourierID: loc.CourierID})
		for _, v := range leftWard {
			v.forget(loc.CourierID)
			v.push(gone)
		}
	}
	if len(fleet) > 0 {
		m := courierMessage(p)
		for _, v := range fleet {
			v.pushPosition(m, now)
		}
	}
	if len(track) > 0 {
		m := positionMessage(p)
		for _, v := range track {
			v.pushPosition(m, now)
		}
	}
}

func (h *Hub) fleetViewersLocked(ward string) []*viewer {
	out := make([]*viewer, 0, len(h.fleet))
	for v := range h.fleet {
		if v.ward == "" || v.ward == ward {
			out = append(out, v)
		}
	}
	return out
}

// OnOrderUpdate sends a changed order view to its viewers. A finished order gets a final "end"
// event and its viewers are released.
func (h *Hub) OnOrderUpdate(view OrderView) {
	h.mu.Lock()
	viewers := make([]*viewer, 0, len(h.byOrder[view.OrderID]))
	for v := range h.byOrder[view.OrderID] {
		viewers = append(viewers, v)
	}
	p, hasPosition := h.positions[view.CourierID]
	if view.Status.Terminal() {
		h.viewers -= len(viewers)
		delete(h.byOrder, view.OrderID)
	}
	h.mu.Unlock()
	if len(viewers) == 0 {
		return
	}

	status := statusMessage(view)
	for _, v := range viewers {
		v.push(status)
		switch {
		case view.Status.Terminal():
			v.push(endMessage(string(view.Status)))
		case view.CourierID != "" && hasPosition:
			// A new or changed assignment: show where the courier is right away.
			m := positionMessage(p)
			m.receivedAt = time.Time{} // a replayed position, not a live one: keep it out of the SLI
			v.push(m)
		}
	}
}

// OrderSnapshot returns the events a new track stream starts with: the status, the courier's
// last position when one is known, and "end" if the order is already finished.
func (h *Hub) OrderSnapshot(orderID string) []message {
	view, ok := h.orders.Get(orderID)
	if !ok {
		return nil
	}
	out := []message{statusMessage(view)}
	if view.Status.Terminal() {
		return append(out, endMessage(string(view.Status)))
	}
	h.mu.Lock()
	p, ok := h.positions[view.CourierID]
	h.mu.Unlock()
	if view.CourierID != "" && ok {
		out = append(out, positionMessage(p))
	}
	return out
}

// FleetSnapshot returns one "courier" event per known courier in the ward (or all wards).
func (h *Hub) FleetSnapshot(ward string) []message {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]message, 0, len(h.positions))
	for _, p := range h.positions {
		if ward == "" || p.ward == ward {
			out = append(out, courierMessage(p))
		}
	}
	return out
}

// SweepGone forgets couriers whose last point was received before cutoff and tells fleet
// viewers with a "courier_gone" event. It returns how many couriers it removed.
func (h *Hub) SweepGone(cutoff time.Time) int {
	type gone struct {
		id, ward string
	}
	var removed []gone
	h.mu.Lock()
	for id, p := range h.positions {
		if p.loc.ReceivedAt.Before(cutoff) {
			delete(h.positions, id)
			removed = append(removed, gone{id, p.ward})
		}
	}
	viewers := make([]*viewer, 0, len(h.fleet)) // every fleet viewer; filtered by ward below
	for v := range h.fleet {
		viewers = append(viewers, v)
	}
	h.mu.Unlock()

	for _, g := range removed {
		m := newMessage("courier_gone", courierGoneData{CourierID: g.id})
		for _, v := range viewers {
			if v.ward == "" || v.ward == g.ward {
				v.forget(g.id)
				v.push(m)
			}
		}
	}
	return len(removed)
}

// Couriers returns how many couriers have a known position.
func (h *Hub) Couriers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.positions)
}
