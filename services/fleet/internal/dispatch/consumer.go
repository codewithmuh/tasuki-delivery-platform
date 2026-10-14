package dispatch

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"slices"
	"sync"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/events"
	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/kit"
)

// Ownership is the set of orders.events partitions this replica holds in the "dispatch"
// consumer group. orders.events is keyed by order_id, so a partition's owner sees all of an
// order's events, and its matcher is the one that offers the order. Kafka moves partitions
// between replicas when one starts or stops; ownership is only a way to share the work, the
// Redis scripts keep the result correct while a partition is moving.
type Ownership struct {
	mu    sync.Mutex
	parts map[int32]struct{}
}

// NewOwnership returns an empty Ownership.
func NewOwnership() *Ownership { return &Ownership{parts: map[int32]struct{}{}} }

// Assign adds partitions (kgo.OnPartitionsAssigned).
func (o *Ownership) Assign(parts []int32) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, p := range parts {
		o.parts[p] = struct{}{}
	}
}

// Revoke removes partitions (kgo.OnPartitionsRevoked and OnPartitionsLost).
func (o *Ownership) Revoke(parts []int32) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, p := range parts {
		delete(o.parts, p)
	}
}

// Partitions returns the owned partitions in order.
func (o *Ownership) Partitions() []int32 {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]int32, 0, len(o.parts))
	for p := range o.parts {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

// Consumer applies orders.events to dispatch's state in Redis. Each event is applied by one
// script that also records its event_id, so a redelivered event changes nothing (dedupe).
type Consumer struct {
	store   *Store
	notify  *Notifier
	wake    func() // asks the matcher to look at a new order now
	metrics *metrics
	logger  *slog.Logger
}

// Handle applies one polled batch. When Redis is down it retries the same record until Redis
// is back (or ctx ends), so the batch is never committed half-applied.
func (c *Consumer) Handle(ctx context.Context, records []*kgo.Record) error {
	for _, r := range records {
		if err := kit.Retry(ctx, c.logger, "apply order event", func(ctx context.Context) error {
			return c.apply(ctx, r, true)
		}); err != nil {
			return err
		}
	}
	return nil
}

// apply applies one record. live is false during a recovery replay, which must not notify
// couriers again about things that happened before Redis was lost.
func (c *Consumer) apply(ctx context.Context, r *kgo.Record, live bool) error {
	e, err := events.DecodeOrderEvent(r.Value)
	if err != nil {
		c.invalid(r, err)
		return nil
	}
	var res string
	switch e.Type {
	case events.OrderConfirmed:
		if !usable(e.Order.Store.Lat, e.Order.Store.Lng) || !usable(e.Order.Dropoff.Lat, e.Order.Dropoff.Lng) ||
			!events.ValidID(events.PrefixStore, e.Order.Store.ID) {
			c.invalid(r, errUnusableOrder)
			return nil
		}
		if res, err = c.store.Confirm(ctx, r.Partition, e); err != nil {
			return err
		}
		if res == "applied" && live {
			c.wake()
		}
	case events.OrderCancelled, events.OrderDelivered, events.OrderFailed, events.OrderPickedUp:
		to := statusDone
		if e.Type == events.OrderPickedUp {
			to = statusPickedUp
		}
		var prev Order
		if prev, res, err = c.store.Close(ctx, r.Partition, e, to); err != nil {
			return err
		}
		if res == "applied" && live {
			c.tellCourier(ctx, e, prev)
		}
	default:
		return nil // a newer producer's event type: not ours to handle
	}
	c.metrics.orderEvents.WithLabelValues(string(e.Type), res).Inc()
	return nil
}

// tellCourier withdraws an open offer from the courier's screen, and tells an assigned courier
// that their order was cancelled.
func (c *Consumer) tellCourier(ctx context.Context, e events.OrderEvent, prev Order) {
	reason := reasonOrderClosed
	if e.Type == events.OrderCancelled {
		reason = reasonOrderCancelled
	}
	switch {
	case prev.Status == statusOffered:
		c.notify.Cancelled(ctx, prev.CourierID, prev.OfferID, reason)
		c.metrics.offers.WithLabelValues("withdrawn").Inc()
		c.logger.Info("offer withdrawn", "order_id", e.OrderID, "offer_id", prev.OfferID, "reason", reason)
	case prev.Status == statusAssigned && e.Type == events.OrderCancelled && prev.OfferID != "":
		c.notify.Cancelled(ctx, prev.CourierID, prev.OfferID, reason)
	}
}

func (c *Consumer) invalid(r *kgo.Record, err error) {
	c.metrics.invalid.WithLabelValues(r.Topic).Inc()
	c.logger.Warn("skipping an unusable record", "topic", r.Topic, "partition", r.Partition, "offset", r.Offset,
		"err", err)
}

// errUnusableOrder means an OrderConfirmed cannot be dispatched: no store ID, or coordinates
// that Redis GEO cannot index.
var errUnusableOrder = errors.New("order has no store ID or unusable coordinates")

// usable reports whether a point can be searched with Redis GEO (latitude within ±85.05°).
func usable(lat, lng float64) bool {
	return !math.IsNaN(lat) && !math.IsNaN(lng) && math.Abs(lat) <= 85.05112878 && math.Abs(lng) <= 180
}
