package state

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/kit"
)

// Rebuilder refills Redis from the log. Redis is a cache derived from courier.locations
// (design §7.2 step 5), so after Redis loses its data, replaying the last few minutes of the
// topic restores every live courier in seconds.
type Rebuilder struct {
	kafka   kit.KafkaConfig
	topic   string
	window  time.Duration // REBUILD_WINDOW; 0 disables replay
	store   *Store
	proc    *Processor // a non-live processor: no history, no freshness samples
	logger  *slog.Logger
	metrics *metrics
	now     func() time.Time

	ready   atomic.Bool
	trigger chan struct{}
}

// Run rebuilds once at start and again whenever Trigger is called. It retries while Kafka or
// Redis is unavailable and returns when ctx is cancelled.
func (r *Rebuilder) Run(ctx context.Context) error {
	for {
		if err := kit.Retry(ctx, r.logger, "rebuild redis from kafka", r.rebuild); err != nil {
			return nil // cancelled
		}
		r.ready.Store(true)
		select {
		case <-ctx.Done():
			return nil
		case <-r.trigger:
			r.ready.Store(false)
		}
	}
}

// Trigger asks for another rebuild. It never blocks; a pending request absorbs new ones.
func (r *Rebuilder) Trigger() {
	select {
	case r.trigger <- struct{}{}:
	default:
	}
}

// Ready reports whether the last rebuild has finished.
func (r *Rebuilder) Ready() bool { return r.ready.Load() }

func (r *Rebuilder) rebuild(ctx context.Context) error {
	started := r.now()
	if r.window > 0 {
		replayed, err := r.replay(ctx, started.Add(-r.window))
		if err != nil {
			return err
		}
		r.logger.Info("rebuilt redis from kafka", "window", r.window.String(), "records", replayed,
			"took", time.Since(started).String())
	}
	if err := r.store.MarkRebuilt(ctx, started); err != nil {
		return err
	}
	r.metrics.rebuilds.Inc()
	r.metrics.rebuildDuration.Set(time.Since(started).Seconds())
	return nil
}

// replay reads every record written since from, up to the end offsets of the moment it starts,
// and applies them. Records arriving meanwhile are the live consumer's job.
func (r *Rebuilder) replay(ctx context.Context, from time.Time) (int, error) {
	admin, err := r.kafka.NewClient()
	if err != nil {
		return 0, err
	}
	defer admin.Close()
	adm := kadm.NewClient(admin)
	starts, err := adm.ListOffsetsAfterMilli(ctx, from.UnixMilli(), r.topic)
	if err != nil {
		return 0, fmt.Errorf("list start offsets: %w", err)
	}
	ends, err := adm.ListEndOffsets(ctx, r.topic)
	if err != nil {
		return 0, fmt.Errorf("list end offsets: %w", err)
	}
	catchUp := kit.NewCatchUp(starts, ends)
	if catchUp.Caught() {
		return 0, nil // nothing was written in the window
	}

	consumer, err := r.kafka.NewClient(kgo.ConsumePartitions(catchUp.Offsets()))
	if err != nil {
		return 0, err
	}
	defer consumer.Close()
	replayCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		select {
		case <-catchUp.Done():
			stop() // ends kit.Consume below
		case <-replayCtx.Done():
		}
	}()
	replayed := 0
	err = kit.Consume(replayCtx, consumer, r.logger, func(ctx context.Context, records []*kgo.Record) error {
		if err := r.proc.Handle(ctx, records); err != nil {
			return err
		}
		replayed += len(records)
		for _, rec := range records {
			catchUp.Observe(rec)
		}
		return nil
	})
	if err != nil {
		return replayed, err
	}
	if !catchUp.Caught() {
		return replayed, ctx.Err() // cancelled before the replay finished
	}
	return replayed, nil
}

// Sweeper removes couriers that went quiet from the geo sets, and notices when Redis has lost
// its data so the Rebuilder can refill it.
type Sweeper struct {
	store      *Store
	rebuilder  *Rebuilder
	interval   time.Duration // SWEEP_INTERVAL
	seenWindow time.Duration // SEEN_WINDOW: the contract's "seen in the last 60 s"
	logger     *slog.Logger
	metrics    *metrics
	now        func() time.Time
}

// Run sweeps every interval until ctx is cancelled. Several replicas may sweep at once; the
// sweep script is atomic and idempotent, so that only costs a little Redis time.
func (s *Sweeper) Run(ctx context.Context) error {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			s.tick(ctx)
		}
	}
}

func (s *Sweeper) tick(ctx context.Context) {
	if n, err := s.store.Sweep(ctx, s.now().Add(-s.seenWindow)); err != nil {
		s.logger.Warn("sweep failed", "err", err)
	} else {
		s.metrics.swept.Add(float64(n))
	}
	if !s.rebuilder.Ready() {
		return // a rebuild is already running
	}
	if ok, err := s.store.Rebuilt(ctx); err == nil && !ok {
		s.logger.Warn("redis lost its data; rebuilding it from kafka")
		s.rebuilder.Trigger()
	}
}
