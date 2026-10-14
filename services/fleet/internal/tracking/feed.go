package tracking

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/events"
	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/kit"
)

type metrics struct {
	eventAge    *prometheus.HistogramVec
	connections *prometheus.GaugeVec
	dropped     *prometheus.CounterVec
	throttled   *prometheus.CounterVec
	invalid     *prometheus.CounterVec
	couriers    prometheus.Gauge
	orders      prometheus.Gauge
}

func newMetrics(reg prometheus.Registerer) *metrics {
	m := &metrics{
		// The tracking freshness SLI (design §9): share of position events younger than 10 s when
		// they reach the viewer = rate of the le="10" bucket / rate of the count.
		eventAge: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "tracking_event_age_seconds",
			Help:    "Age of a live position event (now - received_at) when it is written to a viewer.",
			Buckets: []float64{0.25, 0.5, 1, 2, 5, 10, 15, 30, 60},
		}, []string{"stream"}),
		connections: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "tracking_sse_connections", Help: "Open server-sent event streams.",
		}, []string{"stream"}),
		dropped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tracking_sse_dropped_events_total",
			Help: "Events dropped because a viewer read too slowly and its buffer was full (oldest dropped first).",
		}, []string{"stream"}),
		throttled: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tracking_sse_throttled_events_total",
			Help: "Position events skipped by the TRACKING_MIN_INTERVAL brownout.",
		}, []string{"stream"}),
		invalid: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tracking_records_invalid_total", Help: "Kafka records that could not be decoded, by topic.",
		}, []string{"topic"}),
		couriers: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "tracking_couriers", Help: "Couriers with a known position on this replica.",
		}),
		orders: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "tracking_orders", Help: "Orders known to this replica (active, plus finished ones until ORDER_STATE_TTL).",
		}),
	}
	reg.MustRegister(m.eventAge, m.connections, m.dropped, m.throttled, m.invalid, m.couriers, m.orders)
	for _, s := range []string{streamTrack, streamFleet} {
		m.connections.WithLabelValues(s)
		m.dropped.WithLabelValues(s)
		m.throttled.WithLabelValues(s)
	}
	return m
}

// followLocations reads courier.locations without a consumer group, so every replica sees every
// point (design §7.2 step 4: at 750 points/s a full copy per replica is cheaper and simpler than
// per-courier pub/sub, and it needs no Redis). It starts lookback in the past so the replica
// knows where couriers are before the first viewer connects.
func followLocations(ctx context.Context, kafka kit.KafkaConfig, topic string, lookback time.Duration,
	hub *Hub, m *metrics, logger *slog.Logger,
) error {
	cl, err := kafka.NewClient(
		kgo.ConsumeTopics(topic),
		kgo.ConsumeResetOffset(kgo.NewOffset().AfterMilli(time.Now().Add(-lookback).UnixMilli())),
	)
	if err != nil {
		return err
	}
	defer cl.Close()
	return kit.Consume(ctx, cl, logger, func(_ context.Context, records []*kgo.Record) error {
		for _, r := range records {
			loc, err := events.DecodeCourierLocation(r.Value)
			if err != nil {
				m.invalid.WithLabelValues(r.Topic).Inc()
				continue
			}
			hub.OnLocation(loc)
		}
		return nil
	})
}

// OrderFeed rebuilds the order -> courier view by replaying orders.events and dispatch.events
// from the earliest retained offset, then keeps following both topics. Like followLocations it
// uses no consumer group: every replica needs every order.
type OrderFeed struct {
	kafka         kit.KafkaConfig
	ordersTopic   string
	dispatchTopic string
	orders        *Orders
	hub           *Hub
	metrics       *metrics
	logger        *slog.Logger
	caughtUp      atomic.Bool
	replayStarted time.Time
}

// Ready reports whether the replay has reached the end offsets the topics had at start.
func (f *OrderFeed) Ready() bool { return f.caughtUp.Load() }

// Run replays and follows until ctx is cancelled.
func (f *OrderFeed) Run(ctx context.Context) error {
	cl, err := f.kafka.NewClient(
		kgo.ConsumeTopics(f.ordersTopic, f.dispatchTopic),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
	)
	if err != nil {
		return err
	}
	defer cl.Close()

	f.replayStarted = time.Now()
	var catchUp *kit.CatchUp
	err = kit.Retry(ctx, f.logger, "list order and dispatch offsets", func(ctx context.Context) error {
		adm := kadm.NewClient(cl)
		starts, err := adm.ListStartOffsets(ctx, f.ordersTopic, f.dispatchTopic)
		if err != nil {
			return err
		}
		ends, err := adm.ListEndOffsets(ctx, f.ordersTopic, f.dispatchTopic)
		if err != nil {
			return err
		}
		catchUp = kit.NewCatchUp(starts, ends)
		return nil
	})
	if err != nil {
		return nil // cancelled
	}
	go func() {
		select {
		case <-catchUp.Done():
			f.caughtUp.Store(true)
			f.logger.Info("order state rebuilt", "orders", f.orders.Len(), "took", time.Since(f.replayStarted).String())
		case <-ctx.Done():
		}
	}()

	return kit.Consume(ctx, cl, f.logger, func(_ context.Context, records []*kgo.Record) error {
		for _, r := range records {
			f.apply(r)
			catchUp.Observe(r)
		}
		return nil
	})
}

func (f *OrderFeed) apply(r *kgo.Record) {
	var (
		view    OrderView
		changed bool
	)
	switch r.Topic {
	case f.ordersTopic:
		e, err := events.DecodeOrderEvent(r.Value)
		if err != nil {
			f.metrics.invalid.WithLabelValues(r.Topic).Inc()
			return
		}
		view, changed = f.orders.ApplyOrderEvent(e)
	case f.dispatchTopic:
		e, err := events.DecodeDispatchEvent(r.Value)
		if err != nil {
			f.metrics.invalid.WithLabelValues(r.Topic).Inc()
			return
		}
		view, changed = f.orders.ApplyDispatchEvent(e)
	default:
		return
	}
	if changed {
		f.hub.OnOrderUpdate(view)
	}
}
