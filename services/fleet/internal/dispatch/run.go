// Package dispatch implements the dispatch service (design §7.3, P1 greedy): it consumes
// orders.events, offers each new order to the nearest free courier by road time (OSRM), streams
// offers to couriers over server-sent events, takes their answers, publishes dispatch.events, and
// answers ETA questions for quotes. Several replicas share the work through a Kafka consumer group
// and coordinate through Redis.
package dispatch

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/auth"
	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/events"
	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/kit"
)

// Service is the binary name, used in logs and as the Kafka client ID.
const Service = "dispatch"

// Config holds dispatch's own settings.
type Config struct {
	OrdersTopic    string        // KAFKA_TOPIC_ORDERS
	DispatchTopic  string        // KAFKA_TOPIC_DISPATCH
	Group          string        // KAFKA_GROUP: replicas share the orders.events partitions
	RedisURL       string        // REDIS_URL
	RoutingURL     string        // ROUTING_URL: OSRM base URL; "" when set to "none"
	RecoveryWindow time.Duration // RECOVERY_WINDOW: how much history a recovery replays
	MaxStreams     int           // OFFER_MAX_STREAMS: courier streams per replica
	Match          MatchConfig
	API            APIConfig
	Store          StoreConfig
}

// LoadConfig reads dispatch's settings.
func LoadConfig(e *kit.Env) Config {
	offerTTL := e.Duration("OFFER_TTL", 30*time.Second)
	maxRadius := float64(e.Int("SEARCH_MAX_RADIUS_M", 6000, 1))
	cfg := Config{
		OrdersTopic:    e.String("KAFKA_TOPIC_ORDERS", events.TopicOrders),
		DispatchTopic:  e.String("KAFKA_TOPIC_DISPATCH", events.TopicDispatch),
		Group:          e.String("KAFKA_GROUP", Service),
		RedisURL:       e.String("REDIS_URL", "redis://localhost:6379/0"),
		RoutingURL:     e.Required("ROUTING_URL"),
		RecoveryWindow: e.Duration("RECOVERY_WINDOW", 30*time.Minute),
		MaxStreams:     e.Int("OFFER_MAX_STREAMS", 5000, 1),
		Match: MatchConfig{
			Interval:         e.Duration("MATCH_INTERVAL", time.Second),
			OfferTTL:         offerTTL,
			SearchRadiusM:    float64(e.Int("SEARCH_RADIUS_M", 1500, 1)),
			SearchMaxRadiusM: maxRadius,
			MaxCandidates:    e.Int("SEARCH_MAX_CANDIDATES", 10, 1),
			MaxAttempts:      e.Int("MAX_OFFER_ATTEMPTS", 3, 1),
			NoCourierAfter:   e.Duration("NO_COURIER_AFTER", 60*time.Second),
			ReportInterval:   e.Duration("NO_COURIER_REPORT_INTERVAL", 60*time.Second),
			RetryInterval:    e.Duration("RETRY_INTERVAL", 5*time.Second),
			RoutingTimeout:   e.Duration("ROUTING_TIMEOUT", time.Second),
		},
		API: APIConfig{
			Heartbeat:         e.Duration("OFFER_HEARTBEAT", 15*time.Second),
			WriteTimeout:      e.Duration("OFFER_WRITE_TIMEOUT", 10*time.Second),
			SearchMaxRadiusM:  maxRadius,
			ETABudget:         e.Duration("ETA_BUDGET", 250*time.Millisecond),
			ETARoutingTimeout: e.Duration("ETA_ROUTING_TIMEOUT", 150*time.Millisecond),
			SupplyLowBelow:    e.Int("SUPPLY_LOW_BELOW", 3, 1),
		},
		Store: StoreConfig{
			OfferTTL:    offerTTL,
			OfferKeep:   offerTTL + 10*time.Minute,
			OrderTTL:    e.Duration("ORDER_STATE_TTL", 24*time.Hour),
			BusyTTL:     e.Duration("COURIER_BUSY_TTL", 3*time.Hour),
			OutboxLease: e.Duration("OUTBOX_LEASE", 10*time.Second),
		},
	}
	if strings.EqualFold(cfg.RoutingURL, "none") {
		cfg.RoutingURL = ""
	} else if u, err := url.Parse(cfg.RoutingURL); cfg.RoutingURL != "" &&
		(err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "") {
		e.Fail(fmt.Errorf("ROUTING_URL must be an http(s) URL or \"none\", got %q", cfg.RoutingURL))
	}
	for name, d := range map[string]time.Duration{
		"OFFER_TTL": offerTTL, "MATCH_INTERVAL": cfg.Match.Interval, "RETRY_INTERVAL": cfg.Match.RetryInterval,
		"ROUTING_TIMEOUT": cfg.Match.RoutingTimeout, "OFFER_HEARTBEAT": cfg.API.Heartbeat,
		"OFFER_WRITE_TIMEOUT": cfg.API.WriteTimeout, "ETA_BUDGET": cfg.API.ETABudget,
		"ETA_ROUTING_TIMEOUT": cfg.API.ETARoutingTimeout, "OUTBOX_LEASE": cfg.Store.OutboxLease,
		"ORDER_STATE_TTL": cfg.Store.OrderTTL, "COURIER_BUSY_TTL": cfg.Store.BusyTTL,
	} {
		if d <= 0 {
			e.Fail(fmt.Errorf("%s must be > 0", name))
		}
	}
	if cfg.Match.SearchMaxRadiusM < cfg.Match.SearchRadiusM {
		e.Fail(errors.New("SEARCH_MAX_RADIUS_M must be >= SEARCH_RADIUS_M"))
	}
	if cfg.API.ETARoutingTimeout >= cfg.API.ETABudget {
		e.Fail(errors.New("ETA_ROUTING_TIMEOUT must be shorter than ETA_BUDGET"))
	}
	return cfg
}

// WithHTTPPort makes HTTP_PORT the listen port. The other fleet binaries read PORT through kit,
// which stays as a fallback; wrapping getenv keeps Run and the healthcheck subcommand in step.
func WithHTTPPort(getenv func(string) string) func(string) string {
	return func(key string) string {
		if key == "PORT" {
			return cmp.Or(getenv("HTTP_PORT"), getenv("PORT"))
		}
		return getenv(key)
	}
}

// Run wires and runs dispatch until ctx is cancelled.
func Run(ctx context.Context, getenv func(string) string, logger *slog.Logger, version string) error {
	env := kit.NewEnv(WithHTTPPort(getenv))
	base := kit.LoadConfig(env, Service, version)
	kafka := kit.LoadKafkaConfig(env, Service)
	cfg := LoadConfig(env)
	var verifier *auth.Verifier
	if secret := env.Required("COURIER_TOKEN_SECRET"); secret != "" {
		var err error
		if verifier, err = auth.NewVerifier(secret, auth.RoleCourier, events.PrefixCourier); err != nil {
			env.Fail(err)
		}
	}
	if err := env.Err(); err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	redisOpts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return fmt.Errorf("configuration: REDIS_URL: %w", err)
	}
	// Records Kafka has not acknowledged within the lease fail, and the outbox relay sends them
	// again, so an outage cannot pile up copies of the same event in the producer's buffer.
	prod, err := kafka.NewClient(kgo.RecordDeliveryTimeout(cfg.Store.OutboxLease), kgo.MaxBufferedRecords(10000))
	if err != nil {
		return err
	}
	redis.SetLogger(kit.RedisLogger{Logger: logger}) // JSON at debug, not plain text on stderr
	rdb := redis.NewClient(redisOpts)
	srv := kit.New(base, logger)
	m := newMetrics(srv.Registry())
	store := NewStore(rdb, cfg.Store)
	owner := NewOwnership()
	notify := &Notifier{rdb: rdb, logger: logger}
	router := NewRouter(cfg.RoutingURL, m)
	outbox := &Outbox{store: store, producer: prod, topic: cfg.DispatchTopic, interval: time.Second, batch: 500,
		logger: logger, metrics: m, now: time.Now}
	guard := &Guard{store: store, interval: time.Second, metrics: m, logger: logger, now: time.Now}
	matcher := &Matcher{cfg: cfg.Match, store: store, router: router, outbox: outbox, notify: notify, owner: owner,
		gate: guard, metrics: m, logger: logger, now: time.Now, wake: make(chan struct{}, 1)}
	consumer := &Consumer{store: store, notify: notify, wake: matcher.Wake, metrics: m, logger: logger}
	replayer := &Replayer{kafka: kafka, ordersTopic: cfg.OrdersTopic, dispatchTopic: cfg.DispatchTopic,
		window: cfg.RecoveryWindow, consumer: consumer, store: store, metrics: m, logger: logger, now: time.Now}
	guard.replay = replayer.Run

	group, err := kafka.NewClient(
		kgo.ConsumerGroup(cfg.Group),
		kgo.ConsumeTopics(cfg.OrdersTopic),
		// A brand-new group starts where a recovery replay starts, so they overlap instead of
		// leaving a gap; afterwards the group resumes from its committed offsets.
		kgo.ConsumeResetOffset(kgo.NewOffset().AfterMilli(time.Now().Add(-cfg.RecoveryWindow).UnixMilli())),
		kgo.DisableAutoCommit(),    // commit only after Redis has the events: at-least-once
		kgo.BlockRebalanceOnPoll(), // and before a rebalance can move the partition elsewhere
		kgo.OnPartitionsAssigned(func(_ context.Context, _ *kgo.Client, assigned map[string][]int32) {
			owner.Assign(assigned[cfg.OrdersTopic])
			logger.Info("partitions assigned", "partitions", owner.Partitions())
			matcher.Wake()
		}),
		kgo.OnPartitionsRevoked(func(ctx context.Context, cl *kgo.Client, revoked map[string][]int32) {
			owner.Revoke(revoked[cfg.OrdersTopic])
			if err := cl.CommitUncommittedOffsets(ctx); err != nil {
				logger.Warn("offset commit before revoke failed", "err", err)
			}
		}),
		kgo.OnPartitionsLost(func(_ context.Context, _ *kgo.Client, lost map[string][]int32) {
			owner.Revoke(lost[cfg.OrdersTopic])
		}),
	)
	if err != nil {
		prod.Close()
		return err
	}

	hub := NewHub(cfg.MaxStreams, m)
	api := &API{cfg: cfg.API, store: store, hub: hub, outbox: outbox, router: router, verifier: verifier,
		stopping: srv.Stopping(), metrics: m, logger: logger, now: time.Now}
	api.Register(srv)
	srv.AddReadinessCheck("redis", store.Ping)
	srv.AddReadinessCheck("dispatch state", guard.Ready)
	// OnStop steps run last-registered first: leave the group, flush the producer (its
	// acknowledgements still clear the outbox in Redis), then close Redis.
	srv.OnStop("redis", func(context.Context) error { return rdb.Close() })
	srv.OnStop("kafka producer", func(ctx context.Context) error {
		err := prod.Flush(ctx)
		prod.Close()
		return err
	})
	srv.OnStop("kafka consumer", func(ctx context.Context) error {
		group.AllowRebalance()
		err := group.LeaveGroupContext(ctx) // partitions move to the other replicas at once
		group.Close()
		return err
	})

	logger.Info("starting", "brokers", kafka.Brokers, "group", cfg.Group, "routing", cfg.RoutingURL != "",
		"offer_ttl", cfg.Match.OfferTTL.String(), "recovery_window", cfg.RecoveryWindow.String())
	stopping := srv.Stopping()
	return srv.Run(ctx,
		// On SIGTERM these two stop at once (no new orders, no new offers), while HTTP drains.
		kit.Worker{Name: "consume", Run: untilStopping(stopping, func(ctx context.Context) error {
			return kit.Consume(ctx, group, logger, func(ctx context.Context, records []*kgo.Record) error {
				if err := consumer.Handle(ctx, records); err != nil {
					return err
				}
				if err := group.CommitUncommittedOffsets(ctx); err != nil {
					logger.Warn("offset commit failed", "err", err) // the next commit covers it; replays are deduped
				}
				return nil
			})
		})},
		kit.Worker{Name: "match", Run: untilStopping(stopping, matcher.Run)},
		kit.Worker{Name: "outbox", Run: outbox.Run},
		kit.Worker{Name: "guard", Run: guard.Run},
		kit.Worker{Name: "offer streams", Run: func(ctx context.Context) error { return hub.Follow(ctx, rdb, logger) }},
		kit.Worker{Name: "gauges", Run: func(ctx context.Context) error { return sampleGauges(ctx, store, m, 5*time.Second) }},
	)
}

// untilStopping runs fn with a context that also ends when shutdown begins.
func untilStopping(stopping <-chan struct{}, fn func(context.Context) error) func(context.Context) error {
	return func(ctx context.Context) error {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		go func() {
			select {
			case <-stopping:
				cancel()
			case <-ctx.Done():
			}
		}()
		return fn(ctx)
	}
}

// sampleGauges refreshes the courier and outbox gauges from Redis every interval, so a scrape
// never waits on Redis.
func sampleGauges(ctx context.Context, store *Store, m *metrics, every time.Duration) error {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			available, online, outbox, err := store.Counts(ctx)
			if err != nil {
				continue // Redis is down; readiness reports it
			}
			m.couriers.WithLabelValues("available").Set(float64(available))
			m.couriers.WithLabelValues("online").Set(float64(online))
			m.outbox.Set(float64(outbox))
		}
	}
}
