package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/state"
)

// Redis keys that dispatch owns, next to the fleet-state keys it reads (state.GeoAvailableKey,
// state.GeoAllKey, state.RebuiltKey). Redis runs as one primary without cluster mode (contracts),
// so a Lua script may change several of these keys in one atomic step.
const (
	orderKeyPrefix = "dispatch:order:"        // hash: dispatch's view of one order (fields below)
	pendingPrefix  = "dispatch:pending:"      // set per orders.events partition: orders without a courier
	triedPrefix    = "dispatch:tried:"        // set per order: couriers who declined it or let an offer expire
	offerKeyPrefix = "dispatch:offer:"        // hash per offer
	reservedPrefix = "dispatch:reserved:"     // string per courier: their open offer (SET NX PX OFFER_TTL)
	busyPrefix     = "dispatch:busy:"         // string per courier: the order they carry, accept to delivery
	seenPrefix     = "dispatch:event:"        // string per consumed event_id, for dedupe
	outboxKey      = "dispatch:outbox"        // sorted set: event_id -> when the relay may (re)send it (ms)
	outboxBodies   = "dispatch:outbox:events" // hash: event_id -> event JSON
	sentinelKey    = "dispatch:sentinel"      // exists while Redis still holds dispatch's state
	offersChannel  = "dispatch:offers"        // pub/sub: messages for courier offer streams, on every replica
	noKey          = "dispatch:none"          // stands in for a script key that is not needed; never written
)

// An order's status as dispatch sees it. "pending" and "offered" orders sit in their partition's
// pending set; the others have left it. Status only moves forward, apart from offered -> pending
// when an offer is declined or expires.
const (
	statusPending  = "pending"   // waiting for an offer
	statusOffered  = "offered"   // one open offer to one courier
	statusAssigned = "assigned"  // a courier accepted
	statusPickedUp = "picked_up" // the courier has it
	statusDone     = "done"      // delivered, failed or cancelled: dispatch is finished with it
)

// Offer statuses (the "status" field of an offer hash).
const (
	offerOpen      = "open"
	offerAccepted  = "accepted"
	offerDeclined  = "declined"
	offerExpired   = "expired"
	offerWithdrawn = "withdrawn"
)

// Point is a position.
type Point struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

// Order is dispatch's view of one order, read from its dispatch:order:{id} hash.
type Order struct {
	ID             string
	Status         string // "" for a missing record
	Ward           string // the store's ward: the dispatch zone
	Partition      int32  // orders.events partition, which decides the replica that matches it
	StoreID        string
	Store          Point
	Dropoff        Point
	ReadyAt        *time.Time
	ConfirmedAt    time.Time // OrderConfirmed.occurred_at: the SLI clock starts here
	Attempts       int       // offers made so far
	NextAt         time.Time // when the matcher may try again
	OfferID        string    // the open offer (offered) or the accepted one (assigned, picked_up)
	CourierID      string
	OfferExpiresAt time.Time
	EtaPickupS     int
	EtaDropoffS    int
}

// Offer is one offer, read from its dispatch:offer:{id} hash.
type Offer struct {
	ID          string
	OrderID     string
	CourierID   string
	Status      string
	ExpiresAt   time.Time
	Ward        string
	Partition   int32
	ConfirmedAt time.Time
	EtaPickupS  int
	EtaDropoffS int
	Payload     string // the SSE "offer" data, kept so a reconnecting courier gets it again
}

// StoreConfig sets the lifetimes of dispatch's keys.
type StoreConfig struct {
	OfferTTL    time.Duration // OFFER_TTL: a courier's reservation lasts exactly as long as the offer
	OfferKeep   time.Duration // an answered offer stays readable this long, so late answers get 409
	OrderTTL    time.Duration // ORDER_STATE_TTL: order records and dedupe keys outlive the last change by this
	BusyTTL     time.Duration // COURIER_BUSY_TTL: safety net if a delivery event never arrives
	OutboxLease time.Duration // OUTBOX_LEASE: an unacknowledged event is sent again after this
}

// Store keeps dispatch's state in Redis and reads fleet-state's.
type Store struct {
	rdb redis.UniversalClient
	cfg StoreConfig
}

// NewStore returns a Store.
func NewStore(rdb redis.UniversalClient, cfg StoreConfig) *Store {
	return &Store{rdb: rdb, cfg: cfg}
}

func orderKey(id string) string           { return orderKeyPrefix + id }
func pendingKey(partition int32) string   { return pendingPrefix + strconv.Itoa(int(partition)) }
func triedKey(orderID string) string      { return triedPrefix + orderID }
func offerKey(id string) string           { return offerKeyPrefix + id }
func reservedKey(courierID string) string { return reservedPrefix + courierID }
func busyKey(courierID string) string     { return busyPrefix + courierID }
func seenKey(eventID string) string       { return seenPrefix + eventID }

// keyIf returns key(id), or the unused placeholder key when id is empty.
func keyIf(key func(string) string, id string) string {
	if id == "" {
		return noKey
	}
	return key(id)
}

// Order reads one order. A missing record gives an Order with an empty Status.
func (s *Store) Order(ctx context.Context, id string) (Order, error) {
	fields, err := s.rdb.HGetAll(ctx, orderKey(id)).Result()
	if err != nil {
		return Order{}, fmt.Errorf("read order: %w", err)
	}
	return parseOrder(id, fields), nil
}

func parseOrder(id string, f map[string]string) Order {
	o := Order{
		ID: id, Status: f["status"], Ward: f["ward"], StoreID: f["store_id"],
		Partition:   int32(atoi(f["part"])), //nolint:gosec // partition numbers are small; written by us
		Store:       Point{Lat: atof(f["store_lat"]), Lng: atof(f["store_lng"])},
		Dropoff:     Point{Lat: atof(f["drop_lat"]), Lng: atof(f["drop_lng"])},
		ConfirmedAt: fromMS(f["confirmed_ms"]), Attempts: atoi(f["attempts"]), NextAt: fromMS(f["next_ms"]),
		OfferID: f["offer_id"], CourierID: f["courier_id"], OfferExpiresAt: fromMS(f["offer_exp_ms"]),
		EtaPickupS: atoi(f["eta_pickup_s"]), EtaDropoffS: atoi(f["eta_dropoff_s"]),
	}
	if t, err := time.Parse(time.RFC3339Nano, f["ready_at"]); err == nil {
		o.ReadyAt = &t
	}
	return o
}

// PendingOrders returns the orders in the pending sets of the given partitions, oldest first is
// left to the caller. Set members whose record has gone (expired) or moved on are removed.
func (s *Store) PendingOrders(ctx context.Context, partitions []int32) ([]Order, error) {
	if len(partitions) == 0 {
		return nil, nil
	}
	members := make([]*redis.StringSliceCmd, len(partitions))
	if _, err := s.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		for i, part := range partitions {
			members[i] = p.SMembers(ctx, pendingKey(part))
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read pending sets: %w", err)
	}
	type ref struct {
		part int32
		id   string
	}
	var refs []ref
	for i, cmd := range members {
		for _, id := range cmd.Val() {
			refs = append(refs, ref{partitions[i], id})
		}
	}
	records := make([]*redis.MapStringStringCmd, len(refs))
	if _, err := s.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		for i, r := range refs {
			records[i] = p.HGetAll(ctx, orderKey(r.id))
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read pending orders: %w", err)
	}
	orders := make([]Order, 0, len(refs))
	for i, r := range refs {
		o := parseOrder(r.id, records[i].Val())
		if o.Status != statusPending && o.Status != statusOffered {
			// Scripts remove an order from its set when it moves on; this catches expired records.
			s.rdb.SRem(ctx, pendingKey(r.part), r.id)
			continue
		}
		orders = append(orders, o)
	}
	return orders, nil
}

// Nearby is a courier from the geo search, with the straight-line distance to the search centre.
type Nearby struct {
	CourierID string
	Point
	DistanceM float64
}

// Nearby returns up to count available couriers within radiusM metres of p, nearest first
// (fleet-state's geo:couriers:available: idle and seen in the last 60 s).
func (s *Store) Nearby(ctx context.Context, p Point, radiusM float64, count int) ([]Nearby, error) {
	locs, err := s.rdb.GeoSearchLocation(ctx, state.GeoAvailableKey, &redis.GeoSearchLocationQuery{
		GeoSearchQuery: redis.GeoSearchQuery{
			Longitude: p.Lng, Latitude: p.Lat, Radius: radiusM, RadiusUnit: "m", Sort: "ASC", Count: count,
		},
		WithCoord: true, WithDist: true,
	}).Result()
	if err != nil {
		return nil, fmt.Errorf("geo search: %w", err)
	}
	out := make([]Nearby, len(locs))
	for i, l := range locs {
		out[i] = Nearby{CourierID: l.Name, Point: Point{Lat: l.Latitude, Lng: l.Longitude}, DistanceM: l.Dist}
	}
	return out, nil
}

// Engaged returns the couriers, of those given, who hold an open offer or carry an order. It is
// a cheap filter before ranking; the offer script checks again atomically.
func (s *Store) Engaged(ctx context.Context, courierIDs []string) (map[string]bool, error) {
	if len(courierIDs) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, 2*len(courierIDs))
	for _, id := range courierIDs {
		keys = append(keys, reservedKey(id), busyKey(id))
	}
	vals, err := s.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, fmt.Errorf("read courier reservations: %w", err)
	}
	engaged := map[string]bool{}
	for i, id := range courierIDs {
		if vals[2*i] != nil || vals[2*i+1] != nil {
			engaged[id] = true
		}
	}
	return engaged, nil
}

// Tried returns the couriers who declined this order or let an offer for it expire.
func (s *Store) Tried(ctx context.Context, orderID string) (map[string]bool, error) {
	ids, err := s.rdb.SMembers(ctx, triedKey(orderID)).Result()
	if err != nil {
		return nil, fmt.Errorf("read tried couriers: %w", err)
	}
	tried := make(map[string]bool, len(ids))
	for _, id := range ids {
		tried[id] = true
	}
	return tried, nil
}

// Offer reads one offer. ok is false when it does not exist (never did, or kept long enough).
func (s *Store) Offer(ctx context.Context, id string) (Offer, bool, error) {
	f, err := s.rdb.HGetAll(ctx, offerKey(id)).Result()
	if err != nil {
		return Offer{}, false, fmt.Errorf("read offer: %w", err)
	}
	if f["status"] == "" {
		return Offer{}, false, nil
	}
	return Offer{
		ID: id, OrderID: f["order_id"], CourierID: f["courier_id"], Status: f["status"],
		ExpiresAt: fromMS(f["exp_ms"]), Ward: f["ward"],
		Partition:   int32(atoi(f["part"])), //nolint:gosec // partition numbers are small; written by us
		ConfirmedAt: fromMS(f["confirmed_ms"]), EtaPickupS: atoi(f["eta_pickup_s"]),
		EtaDropoffS: atoi(f["eta_dropoff_s"]), Payload: f["payload"],
	}, true, nil
}

// OpenOffer returns the courier's open offer, if they have one that has not expired.
func (s *Store) OpenOffer(ctx context.Context, courierID string, now time.Time) (Offer, bool, error) {
	id, err := s.rdb.Get(ctx, reservedKey(courierID)).Result()
	if errors.Is(err, redis.Nil) {
		return Offer{}, false, nil
	}
	if err != nil {
		return Offer{}, false, fmt.Errorf("read reservation: %w", err)
	}
	o, ok, err := s.Offer(ctx, id)
	if err != nil || !ok || o.Status != offerOpen || !now.Before(o.ExpiresAt) || o.CourierID != courierID {
		return Offer{}, false, err
	}
	return o, true, nil
}

// SentinelExists reports whether dispatch's sentinel key is still there. It disappears with
// everything else when Redis loses its data.
func (s *Store) SentinelExists(ctx context.Context) (bool, error) {
	n, err := s.rdb.Exists(ctx, sentinelKey).Result()
	if err != nil {
		return false, fmt.Errorf("check sentinel: %w", err)
	}
	return n == 1, nil
}

// SetSentinel records that Redis holds dispatch's state again.
func (s *Store) SetSentinel(ctx context.Context, at time.Time) error {
	if err := s.rdb.Set(ctx, sentinelKey, at.UTC().Format(time.RFC3339), 0).Err(); err != nil {
		return fmt.Errorf("set sentinel: %w", err)
	}
	return nil
}

// FleetRebuiltAt returns when fleet-state last started a completed rebuild of the geo sets
// (fleet:state:rebuilt_at). ok is false while the marker is missing.
func (s *Store) FleetRebuiltAt(ctx context.Context) (time.Time, bool, error) {
	v, err := s.rdb.Get(ctx, state.RebuiltKey).Result()
	if errors.Is(err, redis.Nil) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read %s: %w", state.RebuiltKey, err)
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("parse %s: %w", state.RebuiltKey, err)
	}
	return t, true, nil
}

// Counts returns the sizes of fleet-state's geo sets and of the outbox, for the gauges.
func (s *Store) Counts(ctx context.Context) (available, online, outbox int64, err error) {
	var a, o, b *redis.IntCmd
	if _, err := s.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		a = p.ZCard(ctx, state.GeoAvailableKey)
		o = p.ZCard(ctx, state.GeoAllKey)
		b = p.ZCard(ctx, outboxKey)
		return nil
	}); err != nil {
		return 0, 0, 0, fmt.Errorf("count couriers: %w", err)
	}
	return a.Val(), o.Val(), b.Val(), nil
}

// Ping checks that Redis answers.
func (s *Store) Ping(ctx context.Context) error {
	return s.rdb.Ping(ctx).Err()
}

func ms(t time.Time) int64 { return t.UnixMilli() }

func fromMS(s string) time.Time {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n == 0 {
		return time.Time{}
	}
	return time.UnixMilli(n)
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s) // fields written by our own scripts; missing means 0
	return n
}

func atof(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
