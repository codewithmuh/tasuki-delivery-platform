package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/redis/go-redis/v9"

	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/kit"
)

// SSE event names on the courier offer stream (contracts/README.md).
const (
	eventOffer          = "offer"
	eventOfferCancelled = "offer_cancelled"
)

// Reasons sent with offer_cancelled.
const (
	reasonOrderCancelled = "order_cancelled" // the merchant (or orders, after 15 min) cancelled the order
	reasonOrderClosed    = "order_closed"    // the order ended some other way while the offer was open
	reasonExpired        = "expired"         // nobody answered within OFFER_TTL
)

// message is one server-sent event for one courier. It travels between replicas on the Redis
// channel dispatch:offers, because the replica that makes an offer is rarely the one holding the
// courier's stream.
type message struct {
	CourierID string          `json:"courier_id"`
	Event     string          `json:"event"`
	OfferID   string          `json:"offer_id"`
	Data      json.RawMessage `json:"data"`
}

func (m message) frame() []byte {
	return []byte("event: " + m.Event + "\ndata: " + string(m.Data) + "\n\n")
}

// Notifier publishes messages for courier streams. Publishing is best effort: pub/sub keeps no
// copy for a replica that is reconnecting, so every stream also re-reads its courier's open offer
// at each heartbeat and on connect.
type Notifier struct {
	rdb    redis.UniversalClient
	logger *slog.Logger
}

func (n *Notifier) publish(ctx context.Context, m message) {
	b, err := json.Marshal(m)
	if err == nil {
		err = n.rdb.Publish(ctx, offersChannel, b).Err()
	}
	if err != nil {
		n.logger.Warn("could not publish to the offer streams", "event", m.Event, "offer_id", m.OfferID, "err", err)
	}
}

// Offer sends an offer to the courier's stream, wherever it is connected.
func (n *Notifier) Offer(ctx context.Context, courierID, offerID string, payload []byte) {
	n.publish(ctx, message{CourierID: courierID, Event: eventOffer, OfferID: offerID, Data: payload})
}

// Cancelled tells the courier that an offer (open or accepted) no longer stands.
func (n *Notifier) Cancelled(ctx context.Context, courierID, offerID, reason string) {
	data, _ := json.Marshal(map[string]string{"offer_id": offerID, "reason": reason}) // strings only: cannot fail
	n.publish(ctx, message{CourierID: courierID, Event: eventOfferCancelled, OfferID: offerID, Data: data})
}

// errTooManyStreams means this replica holds OFFER_MAX_STREAMS streams already.
var errTooManyStreams = errors.New("too many offer streams")

// courierStream is one open SSE connection. Its queue is small: a courier gets an offer every few
// minutes at most, and a message that does not fit is recovered by the heartbeat re-read.
type courierStream struct {
	courierID string
	ch        chan message
}

// Hub routes messages from the Redis channel to the streams connected to this replica.
type Hub struct {
	max     int
	metrics *metrics

	mu      sync.Mutex
	streams map[string]map[*courierStream]struct{} // courier ID -> open streams (a reconnect may overlap)
	n       int

	subscribed atomic.Bool
}

// NewHub returns a Hub that accepts up to maxStreams streams.
func NewHub(maxStreams int, m *metrics) *Hub {
	return &Hub{max: maxStreams, metrics: m, streams: map[string]map[*courierStream]struct{}{}}
}

// Register adds a stream for the courier.
func (h *Hub) Register(courierID string) (*courierStream, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.n >= h.max {
		return nil, errTooManyStreams
	}
	s := &courierStream{courierID: courierID, ch: make(chan message, 8)}
	if h.streams[courierID] == nil {
		h.streams[courierID] = map[*courierStream]struct{}{}
	}
	h.streams[courierID][s] = struct{}{}
	h.n++
	h.metrics.streams.Set(float64(h.n))
	return s, nil
}

// Unregister removes a stream.
func (h *Hub) Unregister(s *courierStream) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.streams[s.courierID][s]; !ok {
		return
	}
	delete(h.streams[s.courierID], s)
	if len(h.streams[s.courierID]) == 0 {
		delete(h.streams, s.courierID)
	}
	h.n--
	h.metrics.streams.Set(float64(h.n))
}

// Deliver hands m to every stream of its courier on this replica, without blocking.
func (h *Hub) Deliver(m message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.streams[m.CourierID] {
		select {
		case s.ch <- m:
		default: // full: the stream's next heartbeat re-reads the open offer from Redis
		}
	}
}

// Subscribed reports whether the hub is listening on the Redis channel.
func (h *Hub) Subscribed() bool { return h.subscribed.Load() }

// Follow listens on dispatch:offers and delivers every message until ctx is cancelled. One
// subscription per replica, not one per courier: thousands of couriers would otherwise need
// thousands of Redis connections, and at a few offers per second every replica can afford to
// see every message and drop the ones for couriers it does not hold.
func (h *Hub) Follow(ctx context.Context, rdb redis.UniversalClient, logger *slog.Logger) error {
	var ps *redis.PubSub
	if err := kit.Retry(ctx, logger, "subscribe to "+offersChannel, func(ctx context.Context) error {
		ps = rdb.Subscribe(ctx, offersChannel)
		if _, err := ps.Receive(ctx); err != nil { // waits for the subscription to be confirmed
			_ = ps.Close()
			return err
		}
		return nil
	}); err != nil {
		return nil // cancelled
	}
	defer func() { _ = ps.Close() }()
	h.subscribed.Store(true)
	defer h.subscribed.Store(false)
	ch := ps.Channel() // go-redis reconnects and resubscribes by itself
	for {
		select {
		case <-ctx.Done():
			return nil
		case raw, ok := <-ch:
			if !ok {
				return nil
			}
			var m message
			if err := json.Unmarshal([]byte(raw.Payload), &m); err != nil || m.CourierID == "" {
				logger.Warn("ignoring a malformed offer stream message", "err", err)
				continue
			}
			h.Deliver(m)
		}
	}
}
