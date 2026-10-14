package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/events"
	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/kit"
)

// replayTimeout bounds one recovery replay; a failed or slow one is retried on the next check.
const replayTimeout = 2 * time.Minute

// Guard decides whether this replica may dispatch. Redis is a cache that can lose everything
// (a restart without persistence, a failover to an empty replica). dispatch notices that its
// sentinel key is gone, and then:
//
//  1. pauses matching and fails readiness;
//  2. waits until fleet-state has rebuilt the geo sets after the loss (fleet:state:rebuilt_at
//     at or after the last time the sentinel was seen), because matching against a half-empty
//     geo index would send every order to NoCourierAvailable;
//  3. replays the last RECOVERY_WINDOW of orders.events and dispatch.events into Redis, so
//     pending orders come back and assigned ones are not dispatched twice;
//  4. sets the sentinel and resumes.
//
// The same steps run on a first start against an empty Redis.
type Guard struct {
	store    *Store
	replay   func(ctx context.Context) error
	interval time.Duration
	metrics  *metrics
	logger   *slog.Logger
	now      func() time.Time

	dispatching atomic.Bool
	recovering  bool      // only touched by the Run goroutine
	lastOK      time.Time // last time the sentinel was seen
	lossFloor   time.Time // the loss happened after this (zero: unknown, e.g. at start)
}

// Dispatching reports whether matching may run.
func (g *Guard) Dispatching() bool { return g.dispatching.Load() }

// Ready is the readiness check.
func (g *Guard) Ready(context.Context) error {
	if !g.Dispatching() {
		return errors.New("waiting for fleet-state's rebuild or replaying dispatch state")
	}
	return nil
}

// Run checks the sentinel every interval until ctx is cancelled.
func (g *Guard) Run(ctx context.Context) error {
	t := time.NewTicker(g.interval)
	defer t.Stop()
	for {
		g.step(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (g *Guard) set(dispatching bool) {
	g.dispatching.Store(dispatching)
	if dispatching {
		g.metrics.paused.Set(0)
	} else {
		g.metrics.paused.Set(1)
	}
}

func (g *Guard) step(ctx context.Context) {
	present, err := g.store.SentinelExists(ctx)
	if err != nil {
		return // Redis is down: keep the current state; readiness fails on the redis check
	}
	if present {
		// Also the way out when another replica finished the recovery first.
		g.recovering, g.lastOK = false, g.now()
		g.set(true)
		return
	}
	if !g.recovering {
		g.recovering, g.lossFloor = true, g.lastOK
		g.set(false)
		if g.lastOK.IsZero() {
			g.logger.Info("no dispatch state in Redis; rebuilding it from Kafka")
		} else {
			g.logger.Warn("Redis lost dispatch's state; dispatch paused until it is rebuilt", "last_seen", g.lastOK)
		}
	}
	rebuiltAt, ok, err := g.store.FleetRebuiltAt(ctx)
	if err != nil || !ok || rebuiltAt.Before(g.lossFloor.Truncate(time.Second)) {
		return // fleet-state has not refilled the geo sets since the loss yet
	}
	started := g.now()
	if err := g.replay(ctx); err != nil {
		g.logger.Warn("recovery replay failed; retrying", "err", err)
		return
	}
	if err := g.store.SetSentinel(ctx, started); err != nil {
		return
	}
	g.recovering, g.lastOK = false, g.now()
	g.set(true)
	g.metrics.recoveries.Inc()
	g.logger.Info("dispatch state rebuilt; dispatching", "took", g.now().Sub(started).String())
}

// Replayer rebuilds dispatch's Redis state from the recent past of its input and output topics.
// It reads every partition (not just the owned ones), because the replica that notices a loss
// first repairs the state for everyone. Applying an event twice changes nothing, so two replicas
// replaying at once only cost time.
//
// Limits: orders confirmed before RECOVERY_WINDOW are not restored (orders cancels unassigned
// ones after 15 minutes anyway); open offers are lost and their orders offered afresh; events
// still in the Redis outbox at the moment of the loss are lost.
type Replayer struct {
	kafka         kit.KafkaConfig
	ordersTopic   string
	dispatchTopic string
	window        time.Duration
	consumer      *Consumer
	store         *Store
	metrics       *metrics
	logger        *slog.Logger
	now           func() time.Time
}

// Run replays both topics from now - window up to the end offsets they have when it starts.
func (r *Replayer) Run(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, replayTimeout)
	defer cancel()
	admin, err := r.kafka.NewClient()
	if err != nil {
		return err
	}
	defer admin.Close()
	adm := kadm.NewClient(admin)
	from := r.now().Add(-r.window)
	starts, err := adm.ListOffsetsAfterMilli(ctx, from.UnixMilli(), r.ordersTopic, r.dispatchTopic)
	if err != nil {
		return fmt.Errorf("list start offsets: %w", err)
	}
	ends, err := adm.ListEndOffsets(ctx, r.ordersTopic, r.dispatchTopic)
	if err != nil {
		return fmt.Errorf("list end offsets: %w", err)
	}
	catchUp := kit.NewCatchUp(starts, ends)
	if catchUp.Caught() {
		return nil // nothing happened in the window
	}
	cl, err := r.kafka.NewClient(kgo.ConsumePartitions(catchUp.Offsets()))
	if err != nil {
		return err
	}
	defer cl.Close()
	replayCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		select {
		case <-catchUp.Done():
			stop()
		case <-replayCtx.Done():
		}
	}()
	replayed := 0
	err = kit.Consume(replayCtx, cl, r.logger, func(ctx context.Context, records []*kgo.Record) error {
		for _, rec := range records {
			if err := r.apply(ctx, rec); err != nil {
				return err
			}
			catchUp.Observe(rec)
			replayed++
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !catchUp.Caught() {
		if ctx.Err() != nil {
			return ctx.Err() // cancelled or timed out
		}
		return errors.New("replay stopped before catching up")
	}
	r.logger.Info("replayed dispatch state from kafka", "records", replayed, "window", r.window.String())
	return nil
}

func (r *Replayer) apply(ctx context.Context, rec *kgo.Record) error {
	if rec.Topic == r.ordersTopic {
		return r.consumer.apply(ctx, rec, false)
	}
	e, err := events.DecodeDispatchEvent(rec.Value)
	if err != nil {
		r.consumer.invalid(rec, err)
		return nil
	}
	_, err = r.store.Restore(ctx, e)
	return err
}
