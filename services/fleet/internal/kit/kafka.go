package kit

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
)

// KafkaConfig is how a service reaches Kafka.
type KafkaConfig struct {
	Brokers  []string // KAFKA_BROKERS, comma separated host:port list
	ClientID string   // shows up in broker logs and quotas; the service name
}

// LoadKafkaConfig reads KAFKA_BROKERS (default localhost:9092).
func LoadKafkaConfig(e *Env, clientID string) KafkaConfig {
	return KafkaConfig{Brokers: e.List("KAFKA_BROKERS", []string{"localhost:9092"}), ClientID: clientID}
}

// NewClient creates a franz-go client with the shared settings plus opts. The client connects
// lazily, so this succeeds even while Kafka is down: the service starts, serves /healthz, and
// its workers keep retrying until the brokers come back.
func (c KafkaConfig) NewClient(opts ...kgo.Opt) (*kgo.Client, error) {
	base := []kgo.Opt{kgo.SeedBrokers(c.Brokers...), kgo.ClientID(c.ClientID)}
	cl, err := kgo.NewClient(append(base, opts...)...)
	if err != nil {
		return nil, fmt.Errorf("kafka client: %w", err)
	}
	return cl, nil
}

// RecordHandler processes one polled batch of records. An error stops the consumer.
type RecordHandler func(ctx context.Context, records []*kgo.Record) error

// Consume polls cl until ctx is cancelled and hands every non-empty batch to handle. Fetch
// errors (a broker restarting, a leader moving) are logged and retried, because the client
// reconnects by itself. It returns nil on cancellation and the handler's error otherwise.
//
// After each batch it calls AllowRebalance: group consumers created with kgo.BlockRebalanceOnPoll
// can then commit inside handle before their partitions move to another replica. For any other
// client the call does nothing.
func Consume(ctx context.Context, cl *kgo.Client, logger *slog.Logger, handle RecordHandler) error {
	for {
		fetches := cl.PollFetches(ctx)
		if ctx.Err() != nil || fetches.IsClientClosed() {
			cl.AllowRebalance() // a group consumer cannot leave its group while a poll is unacknowledged
			return nil
		}
		hadErrors := false
		fetches.EachError(func(topic string, partition int32, err error) {
			hadErrors = true
			logger.Warn("kafka fetch failed", "topic", topic, "partition", partition, "err", err)
		})
		records := fetches.Records()
		if len(records) > 0 {
			if err := handle(ctx, records); err != nil {
				cl.AllowRebalance()
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		}
		cl.AllowRebalance()
		if hadErrors && len(records) == 0 {
			sleep(ctx, 500*time.Millisecond) // avoid a hot loop of identical errors
		}
	}
}

// Retry calls fn until it succeeds or ctx ends, waiting 0.5 s, 1 s, 2 s ... (at most 10 s)
// between attempts. It returns nil on success and ctx.Err() when cancelled.
func Retry(ctx context.Context, logger *slog.Logger, what string, fn func(context.Context) error) error {
	wait := 500 * time.Millisecond
	for attempt := 1; ; attempt++ {
		err := fn(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		logger.Warn("retrying", "what", what, "attempt", attempt, "in", wait.String(), "err", err)
		if !sleep(ctx, wait) {
			return ctx.Err()
		}
		wait = min(wait*2, 10*time.Second)
	}
}

// sleep waits for d and reports false if ctx ended first.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// CatchUp follows a replay of a fixed range of offsets: it knows, for every partition, the end
// offset (high watermark) the log had when the replay began, and reports when every partition has
// been read up to it. Services use it to stay unready until their in-memory state is rebuilt.
type CatchUp struct {
	mu        sync.Mutex
	remaining map[string]map[int32]span // partitions still to read, by topic
	done      chan struct{}
	closed    bool
}

// span is the offset range of one partition that a replay must read: [from, end).
type span struct{ from, end int64 }

// NewCatchUp builds a tracker from the offsets a replay starts at and the end offsets it must
// reach. Partitions with nothing to read (start >= end) and topics that do not exist yet are
// complete from the start.
func NewCatchUp(start, end kadm.ListedOffsets) *CatchUp {
	c := &CatchUp{remaining: map[string]map[int32]span{}, done: make(chan struct{})}
	end.Each(func(e kadm.ListedOffset) {
		if e.Err != nil || e.Partition < 0 {
			return // missing topic: nothing to replay
		}
		from := int64(0)
		if s, ok := start.Lookup(e.Topic, e.Partition); ok && s.Err == nil && s.Offset >= 0 {
			from = s.Offset
		}
		if from >= e.Offset {
			return
		}
		if c.remaining[e.Topic] == nil {
			c.remaining[e.Topic] = map[int32]span{}
		}
		c.remaining[e.Topic][e.Partition] = span{from: from, end: e.Offset}
	})
	c.closeIfDone()
	return c
}

// Observe records that a record was processed. Call it after the record has been applied.
func (c *CatchUp) Observe(r *kgo.Record) {
	c.mu.Lock()
	defer c.mu.Unlock()
	parts := c.remaining[r.Topic]
	if sp, ok := parts[r.Partition]; ok && r.Offset+1 >= sp.end {
		delete(parts, r.Partition)
		if len(parts) == 0 {
			delete(c.remaining, r.Topic)
		}
		c.closeIfDoneLocked()
	}
}

// Offsets returns where to start reading each partition that still has records to replay, in
// the form kgo.ConsumePartitions takes.
func (c *CatchUp) Offsets() map[string]map[int32]kgo.Offset {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]map[int32]kgo.Offset, len(c.remaining))
	for topic, parts := range c.remaining {
		out[topic] = make(map[int32]kgo.Offset, len(parts))
		for p, sp := range parts {
			out[topic][p] = kgo.NewOffset().At(sp.from)
		}
	}
	return out
}

// Done is closed once every partition has caught up.
func (c *CatchUp) Done() <-chan struct{} { return c.done }

// Caught reports whether the replay has caught up.
func (c *CatchUp) Caught() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

func (c *CatchUp) closeIfDone() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeIfDoneLocked()
}

func (c *CatchUp) closeIfDoneLocked() {
	if len(c.remaining) == 0 && !c.closed {
		c.closed = true
		close(c.done)
	}
}
