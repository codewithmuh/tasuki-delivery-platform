package events

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// contractsDir is the repository's contracts/events, relative to this package.
var contractsDir = filepath.Join("..", "..", "..", "..", "contracts", "events")

// schema compiles one contract schema with format assertions on, so "date-time" is checked.
func schema(t *testing.T, file string) *jsonschema.Schema {
	t.Helper()
	path := filepath.Join(contractsDir, file)
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open contract %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	doc, err := jsonschema.UnmarshalJSON(f)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	if err := c.AddResource(file, doc); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile(file)
	if err != nil {
		t.Fatalf("compile %s: %v", file, err)
	}
	return s
}

// validate marshals v the way producers do and validates the bytes against the schema.
func validate(t *testing.T, s *jsonschema.Schema, v any) error {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return s.Validate(inst)
}

func ptr[T any](v T) *T { return &v }

var now = time.Date(2026, 10, 14, 3, 4, 5, 123_000_000, time.UTC)

func sampleLocation() CourierLocation {
	return CourierLocation{
		Schema: SchemaCourierLocation, EventID: NewID(PrefixEvent), CourierID: NewID(PrefixCourier),
		Lat: 35.6640, Lng: 139.6982, AccuracyM: ptr(5.0), SpeedMPS: ptr(4.2), HeadingDeg: ptr(359.9),
		DeviceTime: now.Add(-time.Second), ReceivedAt: now, Status: CourierToDropoff, OrderID: ptr(NewID(PrefixOrder)),
	}
}

func TestCourierLocationMatchesSchema(t *testing.T) {
	s := schema(t, "courier-location.v1.schema.json")
	full := sampleLocation()
	minimal := CourierLocation{
		Schema: SchemaCourierLocation, EventID: NewID(PrefixEvent), CourierID: NewID(PrefixCourier),
		Lat: -90, Lng: 180, DeviceTime: now, ReceivedAt: now, Status: CourierIdle,
	}
	for name, loc := range map[string]CourierLocation{"full": full, "minimal": minimal} {
		if err := loc.Validate(); err != nil {
			t.Errorf("%s: Validate: %v", name, err)
		}
		if err := validate(t, s, loc); err != nil {
			t.Errorf("%s: schema: %v", name, err)
		}
	}
	// And the schema agrees with Validate on a bad value.
	bad := full
	bad.Status = "flying"
	if validate(t, s, bad) == nil || bad.Validate() == nil {
		t.Error("an unknown status must fail both the schema and Validate")
	}
}

func TestCourierLocationValidateRejects(t *testing.T) {
	cases := map[string]func(*CourierLocation){
		"schema":        func(l *CourierLocation) { l.Schema = "courier.location.v2" },
		"event id":      func(l *CourierLocation) { l.EventID = "evt_123" },
		"courier id":    func(l *CourierLocation) { l.CourierID = NewID(PrefixOrder) },
		"lat":           func(l *CourierLocation) { l.Lat = 90.1 },
		"lat NaN":       func(l *CourierLocation) { l.Lat = math.NaN() },
		"lng":           func(l *CourierLocation) { l.Lng = -180.5 },
		"accuracy":      func(l *CourierLocation) { l.AccuracyM = ptr(-1.0) },
		"speed":         func(l *CourierLocation) { l.SpeedMPS = ptr(-0.1) },
		"heading 360":   func(l *CourierLocation) { l.HeadingDeg = ptr(360.0) },
		"heading below": func(l *CourierLocation) { l.HeadingDeg = ptr(-1.0) },
		"device time":   func(l *CourierLocation) { l.DeviceTime = time.Time{} },
		"received at":   func(l *CourierLocation) { l.ReceivedAt = time.Time{} },
		"status":        func(l *CourierLocation) { l.Status = "" },
		"order id":      func(l *CourierLocation) { l.OrderID = ptr("ord_lowercase") },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			l := sampleLocation()
			mutate(&l)
			if l.Validate() == nil {
				t.Fatal("expected a validation error")
			}
		})
	}
}

func TestDecodeCourierLocation(t *testing.T) {
	want := sampleLocation()
	b, _ := json.Marshal(want)
	got, err := DecodeCourierLocation(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.CourierID != want.CourierID || !got.DeviceTime.Equal(want.DeviceTime) || *got.OrderID != *want.OrderID {
		t.Fatalf("round trip changed the event: %+v", got)
	}
	if _, err := DecodeCourierLocation([]byte("{not json")); err == nil {
		t.Error("expected a decode error")
	}
	if _, err := DecodeCourierLocation([]byte(`{"schema":"courier.location.v1"}`)); err == nil {
		t.Error("expected a validation error")
	}
}

func sampleOrderEvent(typ OrderEventType) OrderEvent {
	return OrderEvent{
		Schema: SchemaOrderEvent, EventID: NewID(PrefixEvent), Type: typ, OrderID: NewID(PrefixOrder),
		MerchantID: NewID(PrefixMerchant), OccurredAt: now,
		Order: OrderDetails{
			Store:      Store{ID: NewID(PrefixStore), Lat: 35.66, Lng: 139.70, Ward: "shibuya"},
			Dropoff:    Place{Lat: 35.65, Lng: 139.75, Ward: "minato"},
			ReadyAt:    ptr(now.Add(10 * time.Minute)),
			PromisedAt: now.Add(35 * time.Minute),
			FeeYen:     660,
		},
	}
}

func TestOrderEventMatchesSchema(t *testing.T) {
	s := schema(t, "order-event.v1.schema.json")
	confirmed := sampleOrderEvent(OrderConfirmed)
	confirmed.Order.ReadyAt = nil
	delivered := sampleOrderEvent(OrderDelivered)
	delivered.Order.CourierID = ptr(NewID(PrefixCourier))
	cancelled := sampleOrderEvent(OrderCancelled)
	cancelled.Reason = ptr("merchant_cancelled")
	for _, e := range []OrderEvent{confirmed, delivered, cancelled} {
		if err := validate(t, s, e); err != nil {
			t.Errorf("%s: %v", e.Type, err)
		}
		b, _ := json.Marshal(e)
		if _, err := DecodeOrderEvent(b); err != nil {
			t.Errorf("%s: decode: %v", e.Type, err)
		}
	}
}

func TestDispatchEventMatchesSchema(t *testing.T) {
	s := schema(t, "dispatch-event.v1.schema.json")
	assigned := DispatchEvent{
		Schema: SchemaDispatchEvent, EventID: NewID(PrefixEvent), Type: CourierAssigned, OrderID: NewID(PrefixOrder),
		CourierID: ptr(NewID(PrefixCourier)), OfferID: ptr(NewID(PrefixOffer)), Ward: "shibuya", OccurredAt: now,
		OfferExpiresAt: ptr(now.Add(30 * time.Second)), EtaPickupS: ptr(300), EtaDropoffS: ptr(900),
	}
	none := DispatchEvent{Schema: SchemaDispatchEvent, EventID: NewID(PrefixEvent), Type: NoCourierAvailable,
		OrderID: NewID(PrefixOrder), OccurredAt: now}
	for _, e := range []DispatchEvent{assigned, none} {
		if err := validate(t, s, e); err != nil {
			t.Errorf("%s: %v", e.Type, err)
		}
		b, _ := json.Marshal(e)
		if _, err := DecodeDispatchEvent(b); err != nil {
			t.Errorf("%s: decode: %v", e.Type, err)
		}
	}
}

func TestDecodeRejectsBadEvents(t *testing.T) {
	order := sampleOrderEvent(OrderConfirmed)
	dispatch := DispatchEvent{Schema: SchemaDispatchEvent, EventID: NewID(PrefixEvent), Type: OfferSent,
		OrderID: NewID(PrefixOrder), OccurredAt: now}
	orderCases := map[string]func(*OrderEvent){
		"schema":   func(e *OrderEvent) { e.Schema = SchemaDispatchEvent },
		"event id": func(e *OrderEvent) { e.EventID = "x" },
		"order id": func(e *OrderEvent) { e.OrderID = "x" },
		"type":     func(e *OrderEvent) { e.Type = "" },
	}
	for name, mutate := range orderCases {
		e := order
		mutate(&e)
		b, _ := json.Marshal(e)
		if _, err := DecodeOrderEvent(b); err == nil {
			t.Errorf("order %s: expected an error", name)
		}
	}
	dispatchCases := map[string]func(*DispatchEvent){
		"schema":     func(e *DispatchEvent) { e.Schema = SchemaOrderEvent },
		"event id":   func(e *DispatchEvent) { e.EventID = "x" },
		"order id":   func(e *DispatchEvent) { e.OrderID = "x" },
		"time":       func(e *DispatchEvent) { e.OccurredAt = time.Time{} },
		"courier id": func(e *DispatchEvent) { e.CourierID = ptr("cou_x") },
	}
	for name, mutate := range dispatchCases {
		e := dispatch
		mutate(&e)
		b, _ := json.Marshal(e)
		if _, err := DecodeDispatchEvent(b); err == nil {
			t.Errorf("dispatch %s: expected an error", name)
		}
	}
	for _, decode := range []func([]byte) error{
		func(b []byte) error { _, err := DecodeOrderEvent(b); return err },
		func(b []byte) error { _, err := DecodeDispatchEvent(b); return err },
	} {
		if decode([]byte("[")) == nil {
			t.Error("expected a JSON error")
		}
	}
}

func TestIDs(t *testing.T) {
	id := NewID(PrefixCourier)
	if len(id) != 30 || !strings.HasPrefix(id, "cou_") || !ValidID(PrefixCourier, id) {
		t.Fatalf("NewID = %q", id)
	}
	if ValidID(PrefixOrder, id) {
		t.Error("a courier ID is not an order ID")
	}
	for _, bad := range []string{"cou_", "cou_01JA2B3C4D5E6F7G8H9JKMNPQ", "cou_01ja2b3c4d5e6f7g8h9jkmnpqr", "cou_01JA2B3C4D5E6F7G8H9JKMNPQI"} {
		if ValidID(PrefixCourier, bad) {
			t.Errorf("ValidID(%q) = true", bad)
		}
	}
	if a, b := NewID(PrefixEvent), NewID(PrefixEvent); a >= b {
		t.Errorf("IDs must sort by creation: %s >= %s", a, b)
	}
}

func TestCourierStatusValid(t *testing.T) {
	for _, s := range []CourierStatus{CourierIdle, CourierToStore, CourierAtStore, CourierToDropoff, CourierOffline} {
		if !s.Valid() {
			t.Errorf("%s should be valid", s)
		}
	}
	if CourierStatus("busy").Valid() {
		t.Error("busy is not a status")
	}
}
