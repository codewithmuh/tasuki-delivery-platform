package state

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/events"
)

// Redis keys (contracts/README.md, "Redis keys"). LastSeenKey and RebuiltKey are additions that
// only fleet-state needs: the first lets the sweeper find stale couriers, the second tells
// fleet-state (and later dispatch) whether Redis has been rebuilt since it was last emptied.
const (
	CourierKeyPrefix = "courier:"               // hash per courier, TTL COURIER_TTL
	GeoAllKey        = "geo:couriers:all"       // every courier seen in the last SEEN_WINDOW
	GeoAvailableKey  = "geo:couriers:available" // idle couriers seen in the last SEEN_WINDOW
	LastSeenKey      = "couriers:last_seen"     // sorted set: courier ID -> received_at (unix ms)
	RebuiltKey       = "fleet:state:rebuilt_at" // RFC 3339 time of the last completed rebuild

	// RebuildingKeyPrefix + token marks one rebuild in flight. Losing Redis deletes it, which is how a
	// rebuild finds out that the data it replayed before the loss is gone.
	RebuildingKeyPrefix = "fleet:state:rebuilding:"
)

// ErrFlushedDuringRebuild means Redis lost its data while a rebuild was replaying, so the rebuild
// must start again instead of marking a half-empty Redis as rebuilt.
var ErrFlushedDuringRebuild = errors.New("redis lost its data during the rebuild")

// markRebuiltScript publishes the rebuild marker only if this rebuild's in-flight key survived,
// and never moves the marker backwards (two replicas may rebuild at once and finish out of order).
// RFC 3339 times in UTC with whole seconds compare correctly as strings.
var markRebuiltScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then
  return 0
end
redis.call('DEL', KEYS[1])
local current = redis.call('GET', KEYS[2])
if not current or ARGV[1] > current then
  redis.call('SET', KEYS[2], ARGV[1])
end
return 1
`)

// upsertScript applies one point only if it is newer (by device_time) than the stored one, so a
// late or duplicated Kafka message can never move a courier backwards. Doing the comparison and
// the writes in one script makes them atomic: two consumers racing on the same courier cannot
// interleave between "read" and "write".
//
// GEOADD rejects latitudes beyond ±85.05° (the Web Mercator limit). Such points are valid in the
// schema but can never be in Tokyo, so they update the hash and skip the geo sets instead of
// making the script fail halfway (Redis does not roll back a script that errors).
var upsertScript = redis.NewScript(`
local prev = redis.call('HGET', KEYS[1], 'device_time_ms')
if prev and tonumber(prev) >= tonumber(ARGV[2]) then
  return 0
end
redis.call('HSET', KEYS[1],
  'lat', ARGV[3], 'lng', ARGV[4], 'status', ARGV[5], 'order_id', ARGV[6],
  'device_time', ARGV[7], 'received_at', ARGV[8], 'device_time_ms', ARGV[2])
redis.call('EXPIRE', KEYS[1], ARGV[9])
local lat = tonumber(ARGV[3])
if ARGV[5] == 'offline' or lat > 85.05112878 or lat < -85.05112878 then
  redis.call('ZREM', KEYS[2], ARGV[1])
  redis.call('ZREM', KEYS[3], ARGV[1])
  redis.call('ZREM', KEYS[4], ARGV[1])
  return 1
end
redis.call('GEOADD', KEYS[2], ARGV[4], ARGV[3], ARGV[1])
if ARGV[5] == 'idle' then
  redis.call('GEOADD', KEYS[3], ARGV[4], ARGV[3], ARGV[1])
else
  redis.call('ZREM', KEYS[3], ARGV[1])
end
redis.call('ZADD', KEYS[4], ARGV[10], ARGV[1])
return 1
`)

// sweepScript removes couriers not seen since a cutoff from both geo sets, in batches. It is a
// script so that "find stale" and "remove" are atomic: a point arriving in between cannot be
// removed by mistake.
var sweepScript = redis.NewScript(`
local stale = redis.call('ZRANGEBYSCORE', KEYS[1], '-inf', ARGV[1], 'LIMIT', 0, ARGV[2])
for _, id in ipairs(stale) do
  redis.call('ZREM', KEYS[1], id)
  redis.call('ZREM', KEYS[2], id)
  redis.call('ZREM', KEYS[3], id)
end
return #stale
`)

// Result is what happened to one point.
type Result int

// Point results.
const (
	Applied Result = iota // newest point for this courier: written
	Stale                 // an equal or newer point was already stored: ignored
	Failed                // Redis rejected this point (a data problem, not an outage)
)

// Store keeps the live fleet in Redis.
type Store struct {
	rdb redis.UniversalClient
	ttl time.Duration
}

// NewStore returns a Store. ttl is how long a courier hash outlives its last point.
func NewStore(rdb redis.UniversalClient, ttl time.Duration) *Store {
	return &Store{rdb: rdb, ttl: ttl}
}

// Apply writes points in one pipelined round trip and reports a Result per point. It returns an
// error only when Redis is unreachable or misbehaving, in which case nothing can be assumed and
// the caller retries the whole batch (safe, because applying a point twice changes nothing).
func (s *Store) Apply(ctx context.Context, points []events.CourierLocation) ([]Result, error) {
	results, err := s.apply(ctx, points)
	if redis.HasErrorPrefix(err, "NOSCRIPT") {
		// Redis forgot the script: it restarted, which is exactly when a rebuild replays
		// points. Load it again and retry once.
		if err := upsertScript.Load(ctx, s.rdb).Err(); err != nil {
			return nil, fmt.Errorf("load upsert script: %w", err)
		}
		results, err = s.apply(ctx, points)
	}
	return results, err
}

func (s *Store) apply(ctx context.Context, points []events.CourierLocation) ([]Result, error) {
	cmds := make([]*redis.Cmd, len(points))
	ttl := strconv.Itoa(max(1, int(s.ttl.Seconds())))
	// Pipelined's own error is the first failed command; each command is inspected below instead.
	_, _ = s.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		for i := range points {
			cmds[i] = upsertScript.EvalSha(ctx, p, keysFor(points[i].CourierID), argsFor(&points[i], ttl)...)
		}
		return nil
	})
	results := make([]Result, len(points))
	for i, cmd := range cmds {
		n, cmdErr := cmd.Int()
		var redisErr redis.Error
		switch {
		case cmdErr == nil && n == 1:
			results[i] = Applied
		case cmdErr == nil:
			results[i] = Stale
		case redis.HasErrorPrefix(cmdErr, "NOSCRIPT"):
			return nil, cmdErr
		case errors.As(cmdErr, &redisErr):
			results[i] = Failed // Redis answered with an error for this point only
		default:
			return nil, fmt.Errorf("redis: %w", cmdErr) // network or timeout: retry the batch
		}
	}
	return results, nil
}

func keysFor(courierID string) []string {
	return []string{CourierKeyPrefix + courierID, GeoAllKey, GeoAvailableKey, LastSeenKey}
}

func argsFor(p *events.CourierLocation, ttl string) []any {
	orderID := ""
	if p.OrderID != nil {
		orderID = *p.OrderID
	}
	return []any{
		p.CourierID,
		p.DeviceTime.UnixMilli(), // exact in Lua's doubles until the year 287,396
		strconv.FormatFloat(p.Lat, 'f', -1, 64),
		strconv.FormatFloat(p.Lng, 'f', -1, 64),
		string(p.Status),
		orderID,
		p.DeviceTime.UTC().Format(time.RFC3339Nano),
		p.ReceivedAt.UTC().Format(time.RFC3339Nano),
		ttl,
		p.ReceivedAt.UnixMilli(),
	}
}

// Sweep removes couriers whose last point was received before cutoff from the geo sets and
// returns how many it removed. Their hashes stay until their TTL, so "last known position"
// lookups still work.
func (s *Store) Sweep(ctx context.Context, cutoff time.Time) (int, error) {
	const batch = 1000
	total := 0
	for {
		n, err := sweepScript.Run(ctx, s.rdb, []string{LastSeenKey, GeoAllKey, GeoAvailableKey},
			cutoff.UnixMilli(), batch).Int()
		if err != nil {
			return total, fmt.Errorf("sweep: %w", err)
		}
		total += n
		if n < batch {
			return total, nil
		}
	}
}

// BeginRebuild records that the rebuild identified by token has started. The key expires so a
// rebuild that crashed halfway leaves nothing behind.
func (s *Store) BeginRebuild(ctx context.Context, token string) error {
	if err := s.rdb.Set(ctx, RebuildingKeyPrefix+token, "1", time.Hour).Err(); err != nil {
		return fmt.Errorf("begin rebuild: %w", err)
	}
	return nil
}

// MarkRebuilt records that Redis now holds everything the replay window covers, as of at (when the
// rebuild started). It returns ErrFlushedDuringRebuild if Redis lost its data since BeginRebuild.
func (s *Store) MarkRebuilt(ctx context.Context, token string, at time.Time) error {
	keys := []string{RebuildingKeyPrefix + token, RebuiltKey}
	ok, err := markRebuiltScript.Run(ctx, s.rdb, keys, at.UTC().Format(time.RFC3339)).Int()
	if err != nil {
		return fmt.Errorf("mark rebuilt: %w", err)
	}
	if ok == 0 {
		return ErrFlushedDuringRebuild
	}
	return nil
}

// Rebuilt reports whether the rebuild marker exists. It disappears when Redis loses its data
// (a restart without persistence, a failover to an empty replica, FLUSHALL).
func (s *Store) Rebuilt(ctx context.Context) (bool, error) {
	n, err := s.rdb.Exists(ctx, RebuiltKey).Result()
	if err != nil {
		return false, fmt.Errorf("check rebuild marker: %w", err)
	}
	return n == 1, nil
}

// Ping checks that Redis answers.
func (s *Store) Ping(ctx context.Context) error {
	return s.rdb.Ping(ctx).Err()
}
