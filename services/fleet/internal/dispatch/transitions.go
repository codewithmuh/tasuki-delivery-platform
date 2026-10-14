package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/events"
)

// Every change to an order, an offer or a courier's reservation is one Lua script, so it is atomic
// on the Redis primary. Correctness with several replicas comes from these scripts, not from who
// owns a partition: an order can only be offered while it is "pending", a courier can only be
// reserved with SET NX, and an answer only counts while its offer is "open". Two replicas that
// briefly both think they own an order (during a rebalance) therefore cannot double-offer it.
//
// Scripts receive every key they touch in KEYS. When a key depends on the current record (which
// courier holds the order, say), the Go side reads the record first and passes what it saw in
// ARGV; the script checks it is unchanged ("compare and set") and otherwise answers "conflict",
// and the Go side reads again.

// errConflict means the record kept changing under a compare-and-set: a bug or extreme contention.
var errConflict = errors.New("order changed concurrently too many times")

const maxCASAttempts = 10

// emitLua is shared by every script that produces dispatch.events. It writes the events passed as
// (id, json) pairs after ARGV[first] to the outbox, due for the relay at ARGV[first]. The events
// are written in the same atomic step as the state change (a transactional outbox), so a state
// change can never lose its event, even if this process dies before Kafka acknowledges it.
const emitLua = `
local function emit(zkey, hkey, first)
  for i = first + 1, #ARGV, 2 do
    redis.call('HSET', hkey, ARGV[i], ARGV[i + 1])
    redis.call('ZADD', zkey, ARGV[first], ARGV[i])
  end
end
`

// outboxEvent is one dispatch event ready for Kafka.
type outboxEvent struct {
	ID      string
	OrderID string
	Body    []byte
}

func encode(evs ...events.DispatchEvent) ([]outboxEvent, error) {
	out := make([]outboxEvent, len(evs))
	for i := range evs {
		b, err := json.Marshal(&evs[i])
		if err != nil {
			return nil, fmt.Errorf("encode %s: %w", evs[i].Type, err)
		}
		out[i] = outboxEvent{ID: evs[i].EventID, OrderID: evs[i].OrderID, Body: b}
	}
	return out, nil
}

// withEvents appends the outbox arguments for emitLua: the due time, then (id, json) pairs.
func (s *Store) withEvents(args []any, now time.Time, evs []outboxEvent) []any {
	args = append(args, ms(now.Add(s.cfg.OutboxLease)))
	for _, e := range evs {
		args = append(args, e.ID, e.Body)
	}
	return args
}

func secs(d time.Duration) int64 { return max(1, int64(d/time.Second)) }

// ---- orders.events -------------------------------------------------------------------------

var confirmScript = redis.NewScript(`
-- KEYS: 1 order, 2 pending set of its partition, 3 dedupe key
-- ARGV: 1 order ID, 2 order TTL (s), 3 dedupe TTL (s), 4... field/value pairs of the record
if redis.call('EXISTS', KEYS[3]) == 1 then return 'duplicate' end
redis.call('SET', KEYS[3], '1', 'EX', ARGV[3])
-- Any status means the order is known: a replayed OrderConfirmed must not resurrect it.
if redis.call('HEXISTS', KEYS[1], 'status') == 1 then return 'ignored' end
redis.call('HSET', KEYS[1], unpack(ARGV, 4))
redis.call('EXPIRE', KEYS[1], ARGV[2])
redis.call('SADD', KEYS[2], ARGV[1])
return 'applied'
`)

// Confirm records a newly confirmed order and queues it on its partition's pending set. It
// returns "applied", "duplicate" (event already seen) or "ignored" (order already known).
func (s *Store) Confirm(ctx context.Context, partition int32, e events.OrderEvent) (string, error) {
	readyAt := ""
	if e.Order.ReadyAt != nil {
		readyAt = e.Order.ReadyAt.UTC().Format(time.RFC3339Nano)
	}
	st, d := e.Order.Store, e.Order.Dropoff
	res, err := confirmScript.Run(ctx, s.rdb,
		[]string{orderKey(e.OrderID), pendingKey(partition), seenKey(e.EventID)},
		e.OrderID, secs(s.cfg.OrderTTL), secs(s.cfg.OrderTTL),
		"status", statusPending, "ward", st.Ward, "part", partition, "store_id", st.ID,
		"store_lat", ftoa(st.Lat), "store_lng", ftoa(st.Lng), "drop_lat", ftoa(d.Lat), "drop_lng", ftoa(d.Lng),
		"ready_at", readyAt, "confirmed_ms", ms(e.OccurredAt), "next_ms", 0,
	).Text()
	if err != nil {
		return "", fmt.Errorf("confirm order: %w", err)
	}
	return res, nil
}

var closeScript = redis.NewScript(`
-- KEYS: 1 order, 2 pending set, 3 dedupe key, 4 offer, 5 reserved:{offered courier},
--       6 busy:{courier}
-- ARGV: 1 order ID, 2-4 expected status, offer ID and courier ID, 5 order TTL (s), 6 dedupe TTL (s),
--       7 new status (picked_up or done), 8 busy TTL (s), 9 courier who picked up ("" when unknown)
if redis.call('EXISTS', KEYS[3]) == 1 then return 'duplicate' end
local cur = redis.call('HMGET', KEYS[1], 'status', 'offer_id', 'courier_id')
if (cur[1] or '') ~= ARGV[2] or (cur[2] or '') ~= ARGV[3] or (cur[3] or '') ~= ARGV[4] then
  return 'conflict'
end
redis.call('SET', KEYS[3], '1', 'EX', ARGV[6])
local rank = {pending = 1, offered = 1, assigned = 2, picked_up = 3, done = 4}
if (rank[ARGV[2]] or 0) >= rank[ARGV[7]] then return 'ignored' end
-- A missing record becomes a tombstone, so a replayed OrderConfirmed cannot bring it back.
redis.call('HSET', KEYS[1], 'status', ARGV[7])
redis.call('EXPIRE', KEYS[1], ARGV[5])
redis.call('SREM', KEYS[2], ARGV[1])
if ARGV[2] == 'offered' then
  if redis.call('HGET', KEYS[4], 'status') == 'open' then redis.call('HSET', KEYS[4], 'status', 'withdrawn') end
  if redis.call('GET', KEYS[5]) == ARGV[3] then redis.call('DEL', KEYS[5]) end
end
if ARGV[7] == 'done' then
  if redis.call('GET', KEYS[6]) == ARGV[1] then redis.call('DEL', KEYS[6]) end
elseif ARGV[9] ~= '' then
  redis.call('HSET', KEYS[1], 'courier_id', ARGV[9])
  redis.call('SET', KEYS[6], ARGV[1], 'EX', ARGV[8])
end
return 'applied'
`)

// Close moves an order to picked_up or done (delivered, failed, cancelled). An open offer is
// withdrawn and the courier's reservation released; "done" also frees the courier who carried
// it. It returns the order as it was before, and "applied", "duplicate" or "ignored".
func (s *Store) Close(ctx context.Context, partition int32, e events.OrderEvent, to string) (Order, string, error) {
	for range maxCASAttempts {
		prev, err := s.Order(ctx, e.OrderID)
		if err != nil {
			return Order{}, "", err
		}
		pickedBy := ""
		if to == statusPickedUp {
			pickedBy = prev.CourierID
			if e.Order.CourierID != nil && events.ValidID(events.PrefixCourier, *e.Order.CourierID) {
				pickedBy = *e.Order.CourierID
			}
		}
		busyFor := prev.CourierID
		if to == statusPickedUp {
			busyFor = pickedBy
		}
		offerHolder := ""
		if prev.Status == statusOffered {
			offerHolder = prev.CourierID
		}
		res, err := closeScript.Run(ctx, s.rdb,
			[]string{orderKey(e.OrderID), pendingKey(partition), seenKey(e.EventID), keyIf(offerKey, prev.OfferID),
				keyIf(reservedKey, offerHolder), keyIf(busyKey, busyFor)},
			e.OrderID, prev.Status, prev.OfferID, prev.CourierID, secs(s.cfg.OrderTTL), secs(s.cfg.OrderTTL),
			to, secs(s.cfg.BusyTTL), pickedBy,
		).Text()
		if err != nil {
			return Order{}, "", fmt.Errorf("close order: %w", err)
		}
		if res != "conflict" {
			return prev, res, nil
		}
	}
	return Order{}, "", errConflict
}

// ---- matching ------------------------------------------------------------------------------

var offerScript = redis.NewScript(emitLua + `
-- KEYS: 1 order, 2 reserved:{courier}, 3 busy:{courier}, 4 offer, 5 outbox, 6 outbox bodies
-- ARGV: 1 offer ID, 2 courier ID, 3 reservation (ms), 4 offer record lifetime (ms), 5 expires (unix ms),
--       6 eta_pickup_s, 7 eta_dropoff_s, 8 SSE payload, 9 order ID, 10 ward, 11 partition,
--       12 confirmed (unix ms), 13 outbox due (ms), 14... events
if redis.call('HGET', KEYS[1], 'status') ~= 'pending' then return 'order_moved' end
if redis.call('EXISTS', KEYS[3]) == 1 then return 'courier_busy' end
-- The reservation: SET NX, so two replicas can never hold an offer for one courier at once, and
-- PX = the offer's lifetime, so it frees itself even if nobody processes the expiry.
if not redis.call('SET', KEYS[2], ARGV[1], 'NX', 'PX', ARGV[3]) then return 'courier_reserved' end
redis.call('HSET', KEYS[4], 'order_id', ARGV[9], 'courier_id', ARGV[2], 'status', 'open', 'exp_ms', ARGV[5],
  'eta_pickup_s', ARGV[6], 'eta_dropoff_s', ARGV[7], 'payload', ARGV[8], 'ward', ARGV[10], 'part', ARGV[11],
  'confirmed_ms', ARGV[12])
redis.call('PEXPIRE', KEYS[4], ARGV[4])
redis.call('HSET', KEYS[1], 'status', 'offered', 'offer_id', ARGV[1], 'courier_id', ARGV[2],
  'offer_exp_ms', ARGV[5], 'eta_pickup_s', ARGV[6], 'eta_dropoff_s', ARGV[7])
redis.call('HINCRBY', KEYS[1], 'attempts', 1)
emit(KEYS[5], KEYS[6], 13)
return 'offered'
`)

// Offer results.
const (
	resultOffered         = "offered"
	resultOrderMoved      = "order_moved"      // no longer pending: cancelled, or offered by another replica
	resultCourierBusy     = "courier_busy"     // carries an order
	resultCourierReserved = "courier_reserved" // holds another open offer
)

// newOffer is everything the offer script needs.
type newOffer struct {
	Order       Order
	OfferID     string
	CourierID   string
	ExpiresAt   time.Time
	EtaPickupS  int
	EtaDropoffS int
	Payload     []byte
	Events      []outboxEvent
}

// CreateOffer reserves the courier and records the offer, atomically. It returns resultOffered or
// the reason it did not.
func (s *Store) CreateOffer(ctx context.Context, now time.Time, n newOffer) (string, error) {
	args := []any{n.OfferID, n.CourierID, s.cfg.OfferTTL.Milliseconds(), s.cfg.OfferKeep.Milliseconds(),
		ms(n.ExpiresAt), n.EtaPickupS, n.EtaDropoffS, n.Payload, n.Order.ID, n.Order.Ward, n.Order.Partition,
		ms(n.Order.ConfirmedAt)}
	res, err := offerScript.Run(ctx, s.rdb,
		[]string{orderKey(n.Order.ID), reservedKey(n.CourierID), busyKey(n.CourierID), offerKey(n.OfferID),
			outboxKey, outboxBodies},
		s.withEvents(args, now, n.Events)...,
	).Text()
	if err != nil {
		return "", fmt.Errorf("create offer: %w", err)
	}
	return res, nil
}

var expireScript = redis.NewScript(emitLua + `
-- KEYS: 1 offer, 2 order, 3 reserved:{courier}, 4 tried set, 5 outbox, 6 outbox bodies
-- ARGV: 1 offer ID, 2 order ID, 3 courier ID, 4 now (ms), 5 next attempt (ms), 6 order TTL (s),
--       7 outbox due (ms), 8... events
local cur = redis.call('HMGET', KEYS[2], 'status', 'offer_id', 'offer_exp_ms')
if cur[1] ~= 'offered' or cur[2] ~= ARGV[1] then return 'moved' end
if tonumber(ARGV[4]) < tonumber(cur[3]) then return 'not_expired' end
if redis.call('HGET', KEYS[1], 'status') == 'open' then redis.call('HSET', KEYS[1], 'status', 'expired') end
redis.call('HSET', KEYS[2], 'status', 'pending', 'offer_id', '', 'courier_id', '', 'offer_exp_ms', '0',
  'next_ms', ARGV[5])
redis.call('SADD', KEYS[4], ARGV[3])
redis.call('EXPIRE', KEYS[4], ARGV[6])
if redis.call('GET', KEYS[3]) == ARGV[1] then redis.call('DEL', KEYS[3]) end
emit(KEYS[5], KEYS[6], 7)
return 'expired'
`)

// ExpireOffer returns an order whose offer ran out to "pending", remembers the courier so they
// are not asked again, and emits OfferExpired. ok is false when the offer was answered first.
func (s *Store) ExpireOffer(ctx context.Context, o Order, now, next time.Time, evs []outboxEvent) (bool, error) {
	args := []any{o.OfferID, o.ID, o.CourierID, ms(now), ms(next), secs(s.cfg.OrderTTL)}
	res, err := expireScript.Run(ctx, s.rdb,
		[]string{offerKey(o.OfferID), orderKey(o.ID), reservedKey(o.CourierID), triedKey(o.ID), outboxKey, outboxBodies},
		s.withEvents(args, now, evs)...,
	).Text()
	if err != nil {
		return false, fmt.Errorf("expire offer: %w", err)
	}
	return res == "expired", nil
}

var noCourierScript = redis.NewScript(emitLua + `
-- KEYS: 1 order, 2 outbox, 3 outbox bodies
-- ARGV: 1 now (ms), 2 next attempt (ms), 3 report ("1" or "0"), 4 report interval (ms), 5 outbox due, 6... events
if redis.call('HGET', KEYS[1], 'status') ~= 'pending' then return {0, 0} end
redis.call('HSET', KEYS[1], 'next_ms', ARGV[2])
if ARGV[3] ~= '1' then return {0, 0} end
local last = tonumber(redis.call('HGET', KEYS[1], 'reported_ms') or '0')
if last > 0 and tonumber(ARGV[1]) - last < tonumber(ARGV[4]) then return {0, 0} end
redis.call('HSET', KEYS[1], 'reported_ms', ARGV[1])
-- The SLI is observed once per order: whoever sets "sli" first observes it.
local first = redis.call('HSETNX', KEYS[1], 'sli', '1')
emit(KEYS[2], KEYS[3], 5)
return {1, first}
`)

// NoOffer records that an attempt made no offer: the order is tried again at next. With report
// set, it also emits NoCourierAvailable, unless one was emitted less than interval ago. reported
// says whether it did; firstSLI whether this is the order's first SLI observation.
func (s *Store) NoOffer(ctx context.Context, o Order, now, next time.Time, report bool, interval time.Duration,
	evs []outboxEvent,
) (reported, firstSLI bool, err error) {
	flag := "0"
	if report {
		flag = "1"
	}
	args := []any{ms(now), ms(next), flag, interval.Milliseconds()}
	res, err := noCourierScript.Run(ctx, s.rdb, []string{orderKey(o.ID), outboxKey, outboxBodies},
		s.withEvents(args, now, evs)...).Int64Slice()
	if err != nil {
		return false, false, fmt.Errorf("record no offer: %w", err)
	}
	return res[0] == 1, res[1] == 1, nil
}

// ---- courier answers -----------------------------------------------------------------------

var acceptScript = redis.NewScript(emitLua + `
-- KEYS: 1 offer, 2 order, 3 reserved:{courier}, 4 busy:{courier}, 5 pending set, 6 outbox, 7 outbox bodies
-- ARGV: 1 offer ID, 2 order ID, 3 courier ID, 4 now (ms), 5 busy TTL (s), 6 outbox due (ms), 7... events
local o = redis.call('HMGET', KEYS[1], 'status', 'courier_id', 'exp_ms')
if not o[1] then return {'not_found', 0} end
if o[2] ~= ARGV[3] then return {'not_yours', 0} end
if o[1] == 'accepted' then return {'already_accepted', 0} end
if o[1] ~= 'open' then return {o[1], 0} end
if tonumber(ARGV[4]) >= tonumber(o[3]) then return {'expired', 0} end
local cur = redis.call('HMGET', KEYS[2], 'status', 'offer_id')
if cur[1] ~= 'offered' or cur[2] ~= ARGV[1] then return {'withdrawn', 0} end
local busy = redis.call('GET', KEYS[4])
if busy and busy ~= ARGV[2] then return {'courier_busy', 0} end
redis.call('HSET', KEYS[1], 'status', 'accepted')
redis.call('HSET', KEYS[2], 'status', 'assigned')
redis.call('SREM', KEYS[5], ARGV[2])
if redis.call('GET', KEYS[3]) == ARGV[1] then redis.call('DEL', KEYS[3]) end
redis.call('SET', KEYS[4], ARGV[2], 'EX', ARGV[5])
local first = redis.call('HSETNX', KEYS[2], 'sli', '1')
emit(KEYS[6], KEYS[7], 6)
return {'accepted', first}
`)

var declineScript = redis.NewScript(emitLua + `
-- KEYS: 1 offer, 2 order, 3 reserved:{courier}, 4 tried set, 5 outbox, 6 outbox bodies
-- ARGV: 1 offer ID, 2 order ID, 3 courier ID, 4 now (ms), 5 next attempt (ms), 6 order TTL (s),
--       7 outbox due (ms), 8... events
local o = redis.call('HMGET', KEYS[1], 'status', 'courier_id', 'exp_ms')
if not o[1] then return {'not_found', 0} end
if o[2] ~= ARGV[3] then return {'not_yours', 0} end
if o[1] == 'declined' then return {'already_declined', 0} end
if o[1] ~= 'open' then return {o[1], 0} end
if tonumber(ARGV[4]) >= tonumber(o[3]) then return {'expired', 0} end
local cur = redis.call('HMGET', KEYS[2], 'status', 'offer_id')
if cur[1] ~= 'offered' or cur[2] ~= ARGV[1] then return {'withdrawn', 0} end
redis.call('HSET', KEYS[1], 'status', 'declined')
redis.call('HSET', KEYS[2], 'status', 'pending', 'offer_id', '', 'courier_id', '', 'offer_exp_ms', '0',
  'next_ms', ARGV[5])
redis.call('SADD', KEYS[4], ARGV[3])
redis.call('EXPIRE', KEYS[4], ARGV[6])
if redis.call('GET', KEYS[3]) == ARGV[1] then redis.call('DEL', KEYS[3]) end
emit(KEYS[5], KEYS[6], 7)
return {'declined', 0}
`)

// Answer results.
const (
	answerAccepted        = "accepted"
	answerDeclined        = "declined"
	answerAlreadyAccepted = "already_accepted" // a retried accept: same answer, nothing changes
	answerAlreadyDeclined = "already_declined"
	answerNotFound        = "not_found"
	answerNotYours        = "not_yours"
	answerExpired         = "expired"
	answerCourierBusy     = "courier_busy"
)

// Answer is the outcome of an accept or decline.
type Answer struct {
	Result   string
	Offer    Offer
	FirstSLI bool          // accept only: this assignment is the order's SLI observation
	Events   []outboxEvent // written to the outbox; send them
}

// Accept assigns the order to the courier if their offer is still open and unexpired, and emits
// OfferAccepted and CourierAssigned. Any replica can serve it: the offer lives in Redis.
func (s *Store) Accept(ctx context.Context, offerID, courierID string, now time.Time) (Answer, error) {
	o, ok, err := s.Offer(ctx, offerID)
	if err != nil || !ok {
		return Answer{Result: answerNotFound}, err
	}
	if o.CourierID != courierID {
		return Answer{Result: answerNotYours, Offer: o}, nil
	}
	accepted := newEvent(events.OfferAccepted, o.OrderID, o.Ward, now)
	accepted.CourierID, accepted.OfferID = &o.CourierID, &o.ID
	assigned := accepted
	assigned.Type, assigned.EventID = events.CourierAssigned, events.NewID(events.PrefixEvent)
	assigned.EtaPickupS, assigned.EtaDropoffS = &o.EtaPickupS, &o.EtaDropoffS
	evs, err := encode(accepted, assigned)
	if err != nil {
		return Answer{}, err
	}
	args := []any{offerID, o.OrderID, courierID, ms(now), secs(s.cfg.BusyTTL)}
	res, err := acceptScript.Run(ctx, s.rdb,
		[]string{offerKey(offerID), orderKey(o.OrderID), reservedKey(courierID), busyKey(courierID),
			pendingKey(o.Partition), outboxKey, outboxBodies},
		s.withEvents(args, now, evs)...).Slice()
	if err != nil {
		return Answer{}, fmt.Errorf("accept offer: %w", err)
	}
	a := Answer{Result: fmt.Sprint(res[0]), Offer: o}
	if a.Result == answerAccepted {
		a.FirstSLI, a.Events = res[1] == int64(1), evs
	}
	return a, nil
}

// Decline returns the order to "pending" (tried again at next), remembers the courier so they
// are not asked again, and emits OfferDeclined.
func (s *Store) Decline(ctx context.Context, offerID, courierID string, now, next time.Time) (Answer, error) {
	o, ok, err := s.Offer(ctx, offerID)
	if err != nil || !ok {
		return Answer{Result: answerNotFound}, err
	}
	if o.CourierID != courierID {
		return Answer{Result: answerNotYours, Offer: o}, nil
	}
	declined := newEvent(events.OfferDeclined, o.OrderID, o.Ward, now)
	declined.CourierID, declined.OfferID = &o.CourierID, &o.ID
	evs, err := encode(declined)
	if err != nil {
		return Answer{}, err
	}
	args := []any{offerID, o.OrderID, courierID, ms(now), ms(next), secs(s.cfg.OrderTTL)}
	res, err := declineScript.Run(ctx, s.rdb,
		[]string{offerKey(offerID), orderKey(o.OrderID), reservedKey(courierID), triedKey(o.OrderID),
			outboxKey, outboxBodies},
		s.withEvents(args, now, evs)...).Slice()
	if err != nil {
		return Answer{}, fmt.Errorf("decline offer: %w", err)
	}
	a := Answer{Result: fmt.Sprint(res[0]), Offer: o}
	if a.Result == answerDeclined {
		a.Events = evs
	}
	return a, nil
}

// ---- recovery: dispatch.events -------------------------------------------------------------

var restoreScript = redis.NewScript(`
-- KEYS: 1 order, 2 pending set, 3 dedupe key, 4 busy:{courier}, 5 tried set
-- ARGV: 1 event type, 2 order ID, 3 expected status, 4 courier ID, 5 offer ID, 6 occurred (ms),
--       7 order TTL (s), 8 dedupe TTL (s), 9 busy TTL (s), 10 eta_pickup_s, 11 eta_dropoff_s
if redis.call('EXISTS', KEYS[3]) == 1 then return 'duplicate' end
local st = redis.call('HGET', KEYS[1], 'status') or ''
if st ~= ARGV[3] then return 'conflict' end
redis.call('SET', KEYS[3], '1', 'EX', ARGV[8])
local t = ARGV[1]
if t == 'CourierAssigned' then
  if st ~= '' and st ~= 'pending' and st ~= 'offered' then return 'ignored' end
  redis.call('HSET', KEYS[1], 'status', 'assigned', 'courier_id', ARGV[4], 'offer_id', ARGV[5], 'sli', '1',
    'eta_pickup_s', ARGV[10], 'eta_dropoff_s', ARGV[11])
  redis.call('SREM', KEYS[2], ARGV[2])
  redis.call('SET', KEYS[4], ARGV[2], 'EX', ARGV[9])
elseif t == 'NoCourierAvailable' then
  -- On a record without status (a stub), this keeps the SLI and report time until OrderConfirmed arrives.
  redis.call('HSET', KEYS[1], 'sli', '1')
  if tonumber(ARGV[6]) > tonumber(redis.call('HGET', KEYS[1], 'reported_ms') or '0') then
    redis.call('HSET', KEYS[1], 'reported_ms', ARGV[6])
  end
elseif t == 'OfferSent' then
  if st ~= '' and st ~= 'pending' and st ~= 'offered' then return 'ignored' end
  redis.call('HINCRBY', KEYS[1], 'attempts', 1)
elseif (t == 'OfferDeclined' or t == 'OfferExpired') and ARGV[4] ~= '' then
  redis.call('SADD', KEYS[5], ARGV[4])
  redis.call('EXPIRE', KEYS[5], ARGV[7])
  return 'applied'
else
  return 'ignored'
end
redis.call('EXPIRE', KEYS[1], ARGV[7])
return 'applied'
`)

// Restore applies one of dispatch's own past events to a rebuilt Redis (recovery after a Redis
// loss), so assigned orders are not dispatched again and the SLI is not observed twice.
// Open offers are not restored: they are gone, and their orders are offered afresh.
func (s *Store) Restore(ctx context.Context, e events.DispatchEvent) (string, error) {
	courier, offer := deref(e.CourierID), deref(e.OfferID)
	for range maxCASAttempts {
		cur, err := s.Order(ctx, e.OrderID)
		if err != nil {
			return "", err
		}
		pending := noKey
		if cur.Status != "" {
			pending = pendingKey(cur.Partition)
		}
		res, err := restoreScript.Run(ctx, s.rdb,
			[]string{orderKey(e.OrderID), pending, seenKey(e.EventID), keyIf(busyKey, courier), triedKey(e.OrderID)},
			string(e.Type), e.OrderID, cur.Status, courier, offer, ms(e.OccurredAt), secs(s.cfg.OrderTTL),
			secs(s.cfg.OrderTTL), secs(s.cfg.BusyTTL), derefInt(e.EtaPickupS), derefInt(e.EtaDropoffS),
		).Text()
		if err != nil {
			return "", fmt.Errorf("restore %s: %w", e.Type, err)
		}
		if res != "conflict" {
			return res, nil
		}
	}
	return "", errConflict
}

// ---- outbox --------------------------------------------------------------------------------

var claimScript = redis.NewScript(`
-- KEYS: 1 outbox, 2 outbox bodies. ARGV: 1 now (ms), 2 lease until (ms), 3 limit
-- Takes the events that are due and pushes their due time a lease into the future, so another
-- replica does not send them at the same time. They are deleted once Kafka acknowledges them.
local ids = redis.call('ZRANGEBYSCORE', KEYS[1], '-inf', ARGV[1], 'LIMIT', 0, ARGV[3])
local out = {}
for _, id in ipairs(ids) do
  local body = redis.call('HGET', KEYS[2], id)
  if body then
    redis.call('ZADD', KEYS[1], ARGV[2], id)
    table.insert(out, id)
    table.insert(out, body)
  else
    redis.call('ZREM', KEYS[1], id)
  end
end
return out
`)

// ClaimDue takes up to limit outbox events whose send time has passed, leasing them for
// OutboxLease.
func (s *Store) ClaimDue(ctx context.Context, now time.Time, limit int) ([]outboxEvent, error) {
	vals, err := claimScript.Run(ctx, s.rdb, []string{outboxKey, outboxBodies},
		ms(now), ms(now.Add(s.cfg.OutboxLease)), limit).StringSlice()
	if err != nil {
		return nil, fmt.Errorf("claim outbox events: %w", err)
	}
	out := make([]outboxEvent, 0, len(vals)/2)
	for i := 0; i+1 < len(vals); i += 2 {
		var ref struct {
			OrderID string `json:"order_id"`
		}
		_ = json.Unmarshal([]byte(vals[i+1]), &ref) // our own JSON; a bad one is still sent, unkeyed
		out = append(out, outboxEvent{ID: vals[i], OrderID: ref.OrderID, Body: []byte(vals[i+1])})
	}
	return out, nil
}

// Acked removes events that Kafka has acknowledged from the outbox.
func (s *Store) Acked(ctx context.Context, ids ...string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.ZRem(ctx, outboxKey, toAny(ids)...)
		p.HDel(ctx, outboxBodies, ids...)
		return nil
	})
	if err != nil {
		return fmt.Errorf("remove acknowledged events: %w", err)
	}
	return nil
}

// ---- helpers -------------------------------------------------------------------------------

// newEvent returns a dispatch event with the fields every event has.
func newEvent(typ events.DispatchEventType, orderID, ward string, now time.Time) events.DispatchEvent {
	return events.DispatchEvent{
		Schema: events.SchemaDispatchEvent, EventID: events.NewID(events.PrefixEvent), Type: typ,
		OrderID: orderID, Ward: ward, OccurredAt: now.UTC(),
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefInt(n *int) string {
	if n == nil {
		return "0"
	}
	return strconv.Itoa(*n)
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}
