// Package events holds the Go types of Tasuki's Kafka events. Each type mirrors a JSON Schema in
// contracts/events field for field; the tests marshal samples and validate them against those
// schema files, so the Go side cannot drift from the contract unnoticed.
package events

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"
)

// Schema names, sent in every event's "schema" field.
const (
	SchemaCourierLocation = "courier.location.v1"
	SchemaOrderEvent      = "order.event.v1"
	SchemaDispatchEvent   = "dispatch.event.v1"
)

// Topic names and their keys (contracts/README.md, "Events").
const (
	TopicCourierLocations = "courier.locations" // key: courier_id
	TopicOrders           = "orders.events"     // key: order_id
	TopicDispatch         = "dispatch.events"   // key: order_id
)

// CourierStatus is what the courier is doing when a GPS point is taken.
type CourierStatus string

// Courier statuses allowed by courier-location.v1.
const (
	CourierIdle      CourierStatus = "idle"
	CourierToStore   CourierStatus = "to_store"
	CourierAtStore   CourierStatus = "at_store"
	CourierToDropoff CourierStatus = "to_dropoff"
	CourierOffline   CourierStatus = "offline"
)

// Valid reports whether s is one of the statuses in the schema.
func (s CourierStatus) Valid() bool {
	switch s {
	case CourierIdle, CourierToStore, CourierAtStore, CourierToDropoff, CourierOffline:
		return true
	}
	return false
}

// CourierLocation is one GPS point (courier.location.v1). Optional numbers are pointers so that
// "absent" and "zero" stay different: a speed of 0 m/s is a real reading.
type CourierLocation struct {
	Schema     string        `json:"schema"`
	EventID    string        `json:"event_id"`
	CourierID  string        `json:"courier_id"`
	Lat        float64       `json:"lat"`
	Lng        float64       `json:"lng"`
	AccuracyM  *float64      `json:"accuracy_m,omitempty"`
	SpeedMPS   *float64      `json:"speed_mps,omitempty"`
	HeadingDeg *float64      `json:"heading_deg,omitempty"`
	DeviceTime time.Time     `json:"device_time"` // when the device took the reading
	ReceivedAt time.Time     `json:"received_at"` // when fleet-ingest accepted it; freshness SLIs start here
	Status     CourierStatus `json:"status"`
	OrderID    *string       `json:"order_id,omitempty"`
}

// Validate checks the rules of courier.location.v1 that Go's type system cannot express.
func (l *CourierLocation) Validate() error {
	switch {
	case l.Schema != SchemaCourierLocation:
		return fmt.Errorf("schema must be %q", SchemaCourierLocation)
	case !ValidID(PrefixEvent, l.EventID):
		return errors.New("event_id must be evt_<ULID>")
	case !ValidID(PrefixCourier, l.CourierID):
		return errors.New("courier_id must be cou_<ULID>")
	case !inRange(l.Lat, -90, 90):
		return errors.New("lat must be between -90 and 90")
	case !inRange(l.Lng, -180, 180):
		return errors.New("lng must be between -180 and 180")
	case l.AccuracyM != nil && !inRange(*l.AccuracyM, 0, math.MaxFloat64):
		return errors.New("accuracy_m must be >= 0")
	case l.SpeedMPS != nil && !inRange(*l.SpeedMPS, 0, math.MaxFloat64):
		return errors.New("speed_mps must be >= 0")
	case l.HeadingDeg != nil && (!inRange(*l.HeadingDeg, 0, 360) || *l.HeadingDeg == 360):
		return errors.New("heading_deg must be >= 0 and < 360")
	case l.DeviceTime.IsZero():
		return errors.New("device_time is required")
	case l.ReceivedAt.IsZero():
		return errors.New("received_at is required")
	case !l.Status.Valid():
		return fmt.Errorf("status %q is not one of idle, to_store, at_store, to_dropoff, offline", l.Status)
	case l.OrderID != nil && !ValidID(PrefixOrder, *l.OrderID):
		return errors.New("order_id must be ord_<ULID> or null")
	}
	return nil
}

func inRange(v, lo, hi float64) bool {
	return !math.IsNaN(v) && v >= lo && v <= hi
}

// DecodeCourierLocation parses and validates one courier.locations message.
func DecodeCourierLocation(b []byte) (CourierLocation, error) {
	var l CourierLocation
	if err := json.Unmarshal(b, &l); err != nil {
		return CourierLocation{}, fmt.Errorf("decode %s: %w", SchemaCourierLocation, err)
	}
	if err := l.Validate(); err != nil {
		return CourierLocation{}, fmt.Errorf("invalid %s: %w", SchemaCourierLocation, err)
	}
	return l, nil
}

// OrderEventType is a step in an order's lifecycle (order.event.v1).
type OrderEventType string

// Order event types.
const (
	OrderConfirmed OrderEventType = "OrderConfirmed"
	OrderCancelled OrderEventType = "OrderCancelled"
	OrderPickedUp  OrderEventType = "OrderPickedUp"
	OrderDelivered OrderEventType = "OrderDelivered"
	OrderFailed    OrderEventType = "OrderFailed"
)

// OrderEvent is a change in an order's lifecycle, published by orders through its outbox. It
// carries no customer name, phone or address: only the coordinates dispatch needs.
type OrderEvent struct {
	Schema     string         `json:"schema"`
	EventID    string         `json:"event_id"`
	Type       OrderEventType `json:"type"`
	OrderID    string         `json:"order_id"`
	MerchantID string         `json:"merchant_id"`
	OccurredAt time.Time      `json:"occurred_at"`
	Order      OrderDetails   `json:"order"`
	Reason     *string        `json:"reason,omitempty"` // set on OrderCancelled and OrderFailed
}

// OrderDetails is the "order" object of an order event.
type OrderDetails struct {
	Store      Store      `json:"store"`
	Dropoff    Place      `json:"dropoff"`
	ReadyAt    *time.Time `json:"ready_at,omitempty"` // when the store expects the order to be ready
	PromisedAt time.Time  `json:"promised_at"`        // delivery time promised at confirmation
	FeeYen     int64      `json:"fee_yen"`            // integer yen, including 10 % consumption tax
	CourierID  *string    `json:"courier_id,omitempty"`
}

// Store is where an order is picked up.
type Store struct {
	ID   string  `json:"id"`
	Lat  float64 `json:"lat"`
	Lng  float64 `json:"lng"`
	Ward string  `json:"ward"`
}

// Place is a point with its ward, used for the drop-off.
type Place struct {
	Lat  float64 `json:"lat"`
	Lng  float64 `json:"lng"`
	Ward string  `json:"ward"`
}

// DecodeOrderEvent parses one orders.events message and checks the fields consumers rely on.
// Unknown event types are accepted, so a newer producer does not break this consumer.
func DecodeOrderEvent(b []byte) (OrderEvent, error) {
	var e OrderEvent
	if err := json.Unmarshal(b, &e); err != nil {
		return OrderEvent{}, fmt.Errorf("decode %s: %w", SchemaOrderEvent, err)
	}
	switch {
	case e.Schema != SchemaOrderEvent:
		return OrderEvent{}, fmt.Errorf("schema must be %q", SchemaOrderEvent)
	case !ValidID(PrefixEvent, e.EventID):
		return OrderEvent{}, errors.New("event_id must be evt_<ULID>")
	case !ValidID(PrefixOrder, e.OrderID):
		return OrderEvent{}, errors.New("order_id must be ord_<ULID>")
	case e.Type == "" || e.OccurredAt.IsZero():
		return OrderEvent{}, errors.New("type and occurred_at are required")
	}
	return e, nil
}

// DispatchEventType is a dispatch decision or offer outcome (dispatch.event.v1).
type DispatchEventType string

// Dispatch event types.
const (
	OfferSent          DispatchEventType = "OfferSent"
	OfferAccepted      DispatchEventType = "OfferAccepted"
	OfferDeclined      DispatchEventType = "OfferDeclined"
	OfferExpired       DispatchEventType = "OfferExpired"
	CourierAssigned    DispatchEventType = "CourierAssigned"
	NoCourierAvailable DispatchEventType = "NoCourierAvailable"
)

// DispatchEvent is published by dispatch, keyed by order_id.
type DispatchEvent struct {
	Schema         string            `json:"schema"`
	EventID        string            `json:"event_id"`
	Type           DispatchEventType `json:"type"`
	OrderID        string            `json:"order_id"`
	CourierID      *string           `json:"courier_id,omitempty"`
	OfferID        *string           `json:"offer_id,omitempty"`
	Ward           string            `json:"ward,omitempty"`
	OccurredAt     time.Time         `json:"occurred_at"`
	OfferExpiresAt *time.Time        `json:"offer_expires_at,omitempty"`
	EtaPickupS     *int              `json:"eta_pickup_s,omitempty"`  // road time courier -> store, seconds
	EtaDropoffS    *int              `json:"eta_dropoff_s,omitempty"` // road time store -> drop-off, seconds
}

// DecodeDispatchEvent parses one dispatch.events message and checks the fields consumers rely on.
func DecodeDispatchEvent(b []byte) (DispatchEvent, error) {
	var e DispatchEvent
	if err := json.Unmarshal(b, &e); err != nil {
		return DispatchEvent{}, fmt.Errorf("decode %s: %w", SchemaDispatchEvent, err)
	}
	switch {
	case e.Schema != SchemaDispatchEvent:
		return DispatchEvent{}, fmt.Errorf("schema must be %q", SchemaDispatchEvent)
	case !ValidID(PrefixEvent, e.EventID):
		return DispatchEvent{}, errors.New("event_id must be evt_<ULID>")
	case !ValidID(PrefixOrder, e.OrderID):
		return DispatchEvent{}, errors.New("order_id must be ord_<ULID>")
	case e.Type == "" || e.OccurredAt.IsZero():
		return DispatchEvent{}, errors.New("type and occurred_at are required")
	case e.CourierID != nil && !ValidID(PrefixCourier, *e.CourierID):
		return DispatchEvent{}, errors.New("courier_id must be cou_<ULID> or null")
	}
	return e, nil
}
