// Package tracking implements the tracking service (design §7.2 step 4): live courier positions
// and order status pushed to browsers as server-sent events. It reads Kafka directly, never
// Redis, so a Redis outage cannot stop tracking (design §10).
package tracking

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/auth"
	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/events"
	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/kit"
)

// Service is the binary name, used in logs and as the Kafka client ID.
const Service = "tracking"

// Config holds the tracking service's own settings.
type Config struct {
	LocationsTopic   string        // KAFKA_TOPIC_LOCATIONS
	OrdersTopic      string        // KAFKA_TOPIC_ORDERS
	DispatchTopic    string        // KAFKA_TOPIC_DISPATCH
	LocationLookback time.Duration // TRACKING_LOCATION_LOOKBACK: positions to load at start
	SeenWindow       time.Duration // SEEN_WINDOW: couriers quiet for longer get "courier_gone"
	OrderTTL         time.Duration // ORDER_STATE_TTL: forget orders with no event for this long
	Hub              HubConfig
	Streams          StreamConfig
}

// LoadConfig reads the tracking service's settings.
func LoadConfig(e *kit.Env) Config {
	cfg := Config{
		LocationsTopic:   e.String("KAFKA_TOPIC_LOCATIONS", events.TopicCourierLocations),
		OrdersTopic:      e.String("KAFKA_TOPIC_ORDERS", events.TopicOrders),
		DispatchTopic:    e.String("KAFKA_TOPIC_DISPATCH", events.TopicDispatch),
		LocationLookback: e.Duration("TRACKING_LOCATION_LOOKBACK", time.Minute),
		SeenWindow:       e.Duration("SEEN_WINDOW", 60*time.Second),
		OrderTTL:         e.Duration("ORDER_STATE_TTL", 48*time.Hour),
		Hub: HubConfig{
			ViewerBuffer: e.Int("TRACKING_VIEWER_BUFFER", 64, 1),
			MaxViewers:   e.Int("TRACKING_MAX_VIEWERS", 5000, 1),
			MinInterval:  e.Duration("TRACKING_MIN_INTERVAL", 0),
		},
		Streams: StreamConfig{
			Heartbeat:        e.Duration("TRACKING_HEARTBEAT", 15*time.Second),
			WriteTimeout:     e.Duration("TRACKING_WRITE_TIMEOUT", 10*time.Second),
			UnknownOrderWait: e.Duration("TRACKING_UNKNOWN_ORDER_WAIT", 3*time.Second),
			FleetToken:       e.String("FLEET_STREAM_TOKEN", ""),
			FleetPublic:      e.Bool("FLEET_STREAM_PUBLIC", false),
			CORSOrigins:      e.List("TRACKING_CORS_ORIGINS", nil),
		},
	}
	if cfg.Streams.Heartbeat <= 0 || cfg.Streams.WriteTimeout <= 0 || cfg.SeenWindow <= 0 {
		e.Fail(errors.New("TRACKING_HEARTBEAT, TRACKING_WRITE_TIMEOUT and SEEN_WINDOW must be > 0"))
	}
	return cfg
}

// Run wires and runs the tracking service until ctx is cancelled.
func Run(ctx context.Context, getenv func(string) string, logger *slog.Logger, version string) error {
	env := kit.NewEnv(getenv)
	base := kit.LoadConfig(env, Service, version)
	kafka := kit.LoadKafkaConfig(env, Service)
	cfg := LoadConfig(env)
	var verifier *auth.Verifier
	if secret := env.Required("TRACKING_TOKEN_SECRET"); secret != "" {
		var err error
		if verifier, err = auth.NewVerifier(secret, auth.RoleTracking, events.PrefixOrder); err != nil {
			env.Fail(err)
		}
	}
	if err := env.Err(); err != nil {
		return fmt.Errorf("configuration: %w", err)
	}

	srv := kit.New(base, logger)
	m := newMetrics(srv.Registry())
	orders := NewOrders()
	hub := NewHub(cfg.Hub, orders, m)
	feed := &OrderFeed{kafka: kafka, ordersTopic: cfg.OrdersTopic, dispatchTopic: cfg.DispatchTopic,
		orders: orders, hub: hub, metrics: m, logger: logger}
	streams := &Streams{cfg: cfg.Streams, hub: hub, orders: orders, verifier: verifier,
		stopping: srv.Stopping(), logger: logger, metrics: m, now: time.Now}
	streams.Register(srv)
	srv.AddReadinessCheck("order replay", func(context.Context) error {
		if !feed.Ready() {
			return errors.New("replaying orders.events and dispatch.events")
		}
		return nil
	})

	logger.Info("starting", "brokers", kafka.Brokers, "min_interval", cfg.Hub.MinInterval.String(),
		"fleet_stream_token", cfg.Streams.FleetToken != "", "fleet_stream_public", cfg.Streams.FleetPublic)
	return srv.Run(ctx,
		kit.Worker{Name: "locations", Run: func(ctx context.Context) error {
			return followLocations(ctx, kafka, cfg.LocationsTopic, cfg.LocationLookback, hub, m, logger)
		}},
		kit.Worker{Name: "orders", Run: feed.Run},
		kit.Worker{Name: "maintenance", Run: func(ctx context.Context) error {
			return maintain(ctx, hub, orders, m, cfg.SeenWindow, cfg.OrderTTL, 5*time.Second)
		}},
	)
}

// maintain periodically announces couriers that went quiet and forgets old orders.
func maintain(ctx context.Context, hub *Hub, orders *Orders, m *metrics, seen, ttl, every time.Duration) error {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			now := time.Now()
			hub.SweepGone(now.Add(-seen))
			orders.Evict(now.Add(-ttl))
			m.couriers.Set(float64(hub.Couriers()))
			m.orders.Set(float64(orders.Len()))
		}
	}
}
