// Package state implements fleet-state (design §7.2 step 3): it consumes courier.locations and
// keeps the newest point per courier in Redis, maintains the geo sets dispatch searches, sweeps
// couriers that went quiet, rebuilds Redis from the log when needed, and appends history.
package state

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/events"
	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/history"
	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/kit"
)

// Service is the binary name, used in logs and as the Kafka client ID.
const Service = "fleet-state"

// Config holds fleet-state's own settings.
type Config struct {
	Topic         string        // KAFKA_TOPIC_LOCATIONS
	Group         string        // KAFKA_GROUP: replicas share the partitions of this consumer group
	RedisURL      string        // REDIS_URL, e.g. redis://localhost:6379/0 (rediss:// for TLS)
	CourierTTL    time.Duration // COURIER_TTL: courier hashes expire this long after their last point
	SeenWindow    time.Duration // SEEN_WINDOW: couriers quiet for longer leave the geo sets
	SweepInterval time.Duration // SWEEP_INTERVAL
	RebuildWindow time.Duration // REBUILD_WINDOW: how much of the log a rebuild replays; 0 = none
	HistoryDir    string        // HISTORY_DIR: local history root; empty disables history
	HistoryRoll   time.Duration // HISTORY_ROLL_INTERVAL: close history files this often
}

// LoadConfig reads fleet-state's settings.
func LoadConfig(e *kit.Env) Config {
	cfg := Config{
		Topic:         e.String("KAFKA_TOPIC_LOCATIONS", events.TopicCourierLocations),
		Group:         e.String("KAFKA_GROUP", Service),
		RedisURL:      e.String("REDIS_URL", "redis://localhost:6379/0"),
		CourierTTL:    e.Duration("COURIER_TTL", 10*time.Minute),
		SeenWindow:    e.Duration("SEEN_WINDOW", 60*time.Second),
		SweepInterval: e.Duration("SWEEP_INTERVAL", 5*time.Second),
		RebuildWindow: e.Duration("REBUILD_WINDOW", 5*time.Minute),
		HistoryDir:    e.String("HISTORY_DIR", ""),
		HistoryRoll:   e.Duration("HISTORY_ROLL_INTERVAL", 5*time.Minute),
	}
	if cfg.SweepInterval <= 0 || cfg.CourierTTL < time.Second || cfg.SeenWindow <= 0 {
		e.Fail(errors.New("SWEEP_INTERVAL and SEEN_WINDOW must be > 0 and COURIER_TTL >= 1s"))
	}
	return cfg
}

// Run wires and runs fleet-state until ctx is cancelled.
func Run(ctx context.Context, getenv func(string) string, logger *slog.Logger, version string) error {
	env := kit.NewEnv(getenv)
	base := kit.LoadConfig(env, Service, version)
	kafka := kit.LoadKafkaConfig(env, Service)
	cfg := LoadConfig(env)
	if err := env.Err(); err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	redisOpts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return fmt.Errorf("configuration: REDIS_URL: %w", err)
	}

	var sink history.Sink = history.Discard{}
	if cfg.HistoryDir != "" {
		if sink, err = history.NewLocal(cfg.HistoryDir, cfg.HistoryRoll); err != nil {
			return err
		}
	}
	// A brand-new consumer group starts where a rebuild starts, so the two overlap instead of
	// leaving a gap. Afterwards the group resumes from its committed offsets.
	group, err := kafka.NewClient(
		kgo.ConsumerGroup(cfg.Group),
		kgo.ConsumeTopics(cfg.Topic),
		kgo.ConsumeResetOffset(kgo.NewOffset().AfterMilli(time.Now().Add(-cfg.RebuildWindow).UnixMilli())),
		kgo.DisableAutoCommit(),    // commit only after Redis has the points: at-least-once
		kgo.BlockRebalanceOnPoll(), // and before a rebalance can hand the partition to another replica
	)
	if err != nil {
		return err
	}
	redis.SetLogger(kit.RedisLogger{Logger: logger}) // JSON at debug, not plain text on stderr
	rdb := redis.NewClient(redisOpts)

	srv := kit.New(base, logger)
	m := newMetrics(srv.Registry())
	store := NewStore(rdb, cfg.CourierTTL)
	live := &Processor{store: store, sink: sink, logger: logger, metrics: m, now: time.Now, live: true}
	rebuilder := &Rebuilder{
		kafka: kafka, topic: cfg.Topic, window: cfg.RebuildWindow, store: store, logger: logger, metrics: m,
		now:     time.Now,
		proc:    &Processor{store: store, sink: history.Discard{}, logger: logger, metrics: m, now: time.Now},
		trigger: make(chan struct{}, 1),
	}
	sweeper := &Sweeper{store: store, rebuilder: rebuilder, interval: cfg.SweepInterval,
		seenWindow: cfg.SeenWindow, logger: logger, metrics: m, now: time.Now}

	srv.AddReadinessCheck("rebuild", func(context.Context) error {
		if !rebuilder.Ready() {
			return errors.New("rebuilding redis from kafka")
		}
		return nil
	})
	srv.AddReadinessCheck("redis", store.Ping)
	// OnStop steps run last-registered first: consumer, then history, then Redis.
	srv.OnStop("redis", func(context.Context) error { return rdb.Close() })
	srv.OnStop("history", func(context.Context) error { return sink.Close() })
	srv.OnStop("kafka consumer", func(ctx context.Context) error {
		// Leave the group within the shutdown budget, so the partitions move to another replica
		// at once instead of after the session timeout, and a dead Kafka cannot block the exit.
		group.AllowRebalance()
		err := group.LeaveGroupContext(ctx)
		group.Close()
		return err
	})

	logger.Info("starting", "brokers", kafka.Brokers, "topic", cfg.Topic, "group", cfg.Group,
		"rebuild_window", cfg.RebuildWindow.String(), "history", cfg.HistoryDir != "")
	return srv.Run(ctx,
		kit.Worker{Name: "rebuild", Run: rebuilder.Run},
		kit.Worker{Name: "consume", Run: func(ctx context.Context) error { return consume(ctx, group, live, logger) }},
		kit.Worker{Name: "sweep", Run: sweeper.Run},
	)
}

// consume is the live loop: apply a batch, then commit it.
func consume(ctx context.Context, cl *kgo.Client, proc *Processor, logger *slog.Logger) error {
	return kit.Consume(ctx, cl, logger, func(ctx context.Context, records []*kgo.Record) error {
		if err := proc.Handle(ctx, records); err != nil {
			return err
		}
		if err := cl.CommitUncommittedOffsets(ctx); err != nil {
			// Not fatal: the next commit covers these offsets, and replaying them is harmless.
			logger.Warn("offset commit failed", "err", err)
		}
		return nil
	})
}
