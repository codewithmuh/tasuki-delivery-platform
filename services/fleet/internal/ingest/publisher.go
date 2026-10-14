package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/events"
)

// ErrOverloaded means the publisher refused new events because its buffer is full. The handler
// sheds load instead of queueing more work in memory.
var ErrOverloaded = errors.New("publisher overloaded")

// Publisher sends location events to the courier.locations topic. Publish must return quickly
// when it cannot keep up (ErrOverloaded) and must give up when ctx ends.
type Publisher interface {
	Publish(ctx context.Context, locs []events.CourierLocation) error
}

// KafkaPublisher publishes with franz-go, keyed by courier_id so that one courier's points stay
// in order on one partition.
type KafkaPublisher struct {
	client *kgo.Client
	topic  string
}

// NewKafkaPublisher returns a publisher that writes to topic through client.
func NewKafkaPublisher(client *kgo.Client, topic string) *KafkaPublisher {
	return &KafkaPublisher{client: client, topic: topic}
}

// Publish buffers every event without blocking (TryProduce) and waits until Kafka has
// acknowledged all of them, one has failed, or ctx ends. A 202 therefore means "in Kafka".
func (p *KafkaPublisher) Publish(ctx context.Context, locs []events.CourierLocation) error {
	// One slot per record: a promise never blocks, even after we stopped waiting.
	results := make(chan error, len(locs))
	for i := range locs {
		value, err := json.Marshal(&locs[i])
		if err != nil {
			return fmt.Errorf("encode location: %w", err)
		}
		rec := &kgo.Record{
			Topic:     p.topic,
			Key:       []byte(locs[i].CourierID),
			Value:     value,
			Timestamp: locs[i].ReceivedAt, // fleet-state rebuilds "the last N minutes" by this time
		}
		p.client.TryProduce(ctx, rec, func(_ *kgo.Record, err error) { results <- err })
	}
	for range locs {
		select {
		case err := <-results:
			if errors.Is(err, kgo.ErrMaxBuffered) {
				return fmt.Errorf("%w: %w", ErrOverloaded, err)
			}
			if err != nil {
				return fmt.Errorf("produce to %s: %w", p.topic, err)
			}
		case <-ctx.Done():
			// Records already sent may still land in Kafka. That is fine: consumers keep the
			// newest point per courier, so a duplicate is harmless.
			return fmt.Errorf("produce to %s: %w", p.topic, ctx.Err())
		}
	}
	return nil
}
