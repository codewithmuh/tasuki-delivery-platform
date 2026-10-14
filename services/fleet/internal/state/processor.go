package state

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/events"
	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/history"
	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/kit"
)

type metrics struct {
	points          *prometheus.CounterVec
	freshness       prometheus.Histogram
	swept           prometheus.Counter
	rebuilds        prometheus.Counter
	rebuildDuration prometheus.Gauge
	historyErrors   prometheus.Counter
}

func newMetrics(reg prometheus.Registerer) *metrics {
	m := &metrics{
		points: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fleet_state_points_total",
			Help: "Location points processed, by result: applied, stale (a newer point was stored), invalid, failed.",
		}, []string{"result"}),
		// The location freshness SLI (design §9): share of points visible in Redis within 2 s of
		// receipt = rate of the le="2" bucket / rate of the count.
		freshness: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "fleet_state_freshness_seconds",
			Help:    "Time from received_at (fleet-ingest) to the point being written to Redis. Live points only, not rebuilds.",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60},
		}),
		swept: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "fleet_state_swept_couriers_total",
			Help: "Couriers removed from the geo sets because they were not seen within SEEN_WINDOW.",
		}),
		rebuilds: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "fleet_state_rebuilds_total",
			Help: "Completed rebuilds of Redis from courier.locations (one at start, more if Redis lost its data).",
		}),
		rebuildDuration: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "fleet_state_last_rebuild_duration_seconds",
			Help: "How long the last rebuild took.",
		}),
		historyErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "fleet_state_history_errors_total",
			Help: "Batches that could not be written to the history sink (best effort; Redis is unaffected).",
		}),
	}
	reg.MustRegister(m.points, m.freshness, m.swept, m.rebuilds, m.rebuildDuration, m.historyErrors)
	for _, r := range []string{"applied", "stale", "invalid", "failed"} {
		m.points.WithLabelValues(r)
	}
	return m
}

// Processor turns polled courier.locations records into Redis writes and history.
type Processor struct {
	store   *Store
	sink    history.Sink
	logger  *slog.Logger
	metrics *metrics
	now     func() time.Time
	live    bool // live consumption measures freshness and writes history; a rebuild does neither
}

// Handle processes one batch. It retries Redis until it succeeds, because skipping points would
// make Redis disagree with the log; meanwhile the consumer does not commit, so nothing is lost.
// It returns an error only when ctx is cancelled.
func (p *Processor) Handle(ctx context.Context, records []*kgo.Record) error {
	points := make([]events.CourierLocation, 0, len(records))
	for _, r := range records {
		loc, err := events.DecodeCourierLocation(r.Value)
		if err != nil {
			// A bad message must not block the partition forever: count it and move on.
			p.metrics.points.WithLabelValues("invalid").Inc()
			p.logger.Warn("skipping invalid location", "partition", r.Partition, "offset", r.Offset, "err", err)
			continue
		}
		points = append(points, loc)
	}
	if len(points) == 0 {
		return nil
	}

	var results []Result
	err := kit.Retry(ctx, p.logger, "write locations to redis", func(ctx context.Context) error {
		var err error
		results, err = p.store.Apply(ctx, points)
		return err
	})
	if err != nil {
		return err
	}
	now := p.now()
	for i, res := range results {
		switch res {
		case Applied:
			p.metrics.points.WithLabelValues("applied").Inc()
		case Stale:
			p.metrics.points.WithLabelValues("stale").Inc()
		case Failed:
			p.metrics.points.WithLabelValues("failed").Inc()
			continue
		}
		if p.live {
			p.metrics.freshness.Observe(now.Sub(points[i].ReceivedAt).Seconds())
		}
	}

	if p.live {
		// History has the lowest priority (design §10): a failing disk must not stop Redis updates.
		if err := p.sink.Write(ctx, points); err != nil {
			p.metrics.historyErrors.Inc()
			p.logger.Warn("history write failed", "err", err)
		}
	}
	return nil
}
