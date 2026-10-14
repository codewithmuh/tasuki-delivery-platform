package dispatch

import (
	"context"
	"log/slog"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
)

// producer is the part of *kgo.Client the outbox uses; tests substitute a fake.
type producer interface {
	TryProduce(ctx context.Context, r *kgo.Record, promise func(*kgo.Record, error))
}

// Outbox moves dispatch.events from the Redis outbox to Kafka. Every state-changing script
// writes its events to the outbox in the same atomic step (emitLua). Then:
//
//   - fast path: the caller hands the same events to Send, which produces them at once and
//     deletes each one from the outbox when Kafka acknowledges it;
//   - slow path: Run, on every replica, sends again any event still in the outbox after
//     OUTBOX_LEASE (Kafka was down, or the replica died before the acknowledgement).
//
// Delivery is at-least-once with a stable event_id, which is what consumers dedupe on.
type Outbox struct {
	store    *Store
	producer producer
	topic    string
	interval time.Duration
	batch    int
	logger   *slog.Logger
	metrics  *metrics
	now      func() time.Time
}

// Send produces events that are already in the outbox. It never blocks: when the producer's
// buffer is full the events simply wait for the relay.
func (o *Outbox) Send(ctx context.Context, evs []outboxEvent) {
	for _, e := range evs {
		rec := &kgo.Record{Topic: o.topic, Key: []byte(e.OrderID), Value: e.Body}
		id := e.ID
		o.producer.TryProduce(context.WithoutCancel(ctx), rec, func(_ *kgo.Record, err error) {
			if err != nil {
				o.metrics.publishFails.Inc()
				o.logger.Warn("dispatch event not acknowledged; the outbox relay will resend it", "event_id", id, "err", err)
				return
			}
			// The promise has no request context; give the delete its own short budget.
			ackCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := o.store.Acked(ackCtx, id); err != nil {
				// Harmless: the relay sends it again and consumers drop the duplicate.
				o.logger.Warn("could not remove an acknowledged event from the outbox", "event_id", id, "err", err)
			}
		})
	}
}

// Run relays overdue outbox events every interval until ctx is cancelled.
func (o *Outbox) Run(ctx context.Context) error {
	t := time.NewTicker(o.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			o.relay(ctx)
		}
	}
}

func (o *Outbox) relay(ctx context.Context) {
	evs, err := o.store.ClaimDue(ctx, o.now(), o.batch)
	if err != nil {
		if ctx.Err() == nil { // not just shutting down
			o.logger.Warn("outbox relay failed", "err", err)
		}
		return
	}
	if len(evs) > 0 {
		o.logger.Info("resending dispatch events from the outbox", "events", len(evs))
		o.Send(ctx, evs)
	}
}
