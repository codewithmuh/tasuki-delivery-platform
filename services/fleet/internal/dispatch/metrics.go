package dispatch

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/wards"
)

// Label values.
const (
	outcomeAssigned  = "assigned"   // dispatch_assignment_duration_seconds: a courier accepted
	outcomeNoCourier = "no_courier" // ... or dispatch reported NoCourierAvailable first

	sourceOSRM     = "osrm"     // dispatch_eta_requests_total: road times from OSRM
	sourceFallback = "fallback" // ... or straight line x detour factor at bicycle speed

	wardOther = "other" // dispatch_pending_orders for a store outside the 23 wards
)

type metrics struct {
	assignment   *prometheus.HistogramVec
	offers       *prometheus.CounterVec
	etaRequests  *prometheus.CounterVec
	pending      *prometheus.GaugeVec
	couriers     *prometheus.GaugeVec
	outbox       prometheus.Gauge
	streams      prometheus.Gauge
	paused       prometheus.Gauge
	recoveries   prometheus.Counter
	orderEvents  *prometheus.CounterVec
	invalid      *prometheus.CounterVec
	publishFails prometheus.Counter
}

func newMetrics(reg prometheus.Registerer) *metrics {
	m := &metrics{
		// The dispatch speed SLI (design §9): share of orders with an accepted courier within 60 s of
		// OrderConfirmed. Observed once per order, so orders that never get a courier count as bad
		// (outcome="no_courier") instead of disappearing. The SLO's good events are
		// bucket{outcome="assigned",le="60"} (observability/slo/specs/dispatch.yaml).
		assignment: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "dispatch_assignment_duration_seconds",
			Help: "Time from OrderConfirmed.occurred_at to CourierAssigned (outcome=assigned) or to the first " +
				"NoCourierAvailable (outcome=no_courier). Observed once per order.",
			Buckets: []float64{5, 10, 15, 20, 30, 45, 60, 90, 120, 180, 300, 600, 900},
		}, []string{"outcome"}),
		offers: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dispatch_offers_total",
			Help: "Offers by outcome: sent, accepted, declined, expired, withdrawn (the order was cancelled or closed).",
		}, []string{"outcome"}),
		etaRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dispatch_eta_requests_total",
			Help: "Road-time lookups (matching and GET /v1/eta) by source: osrm, or fallback (straight line x 1.35).",
		}, []string{"source"}),
		pending: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "dispatch_pending_orders",
			Help: "Orders owned by this replica that have no accepted courier yet, by ward. Sum over replicas.",
		}, []string{"ward"}),
		couriers: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "dispatch_couriers",
			Help: "Couriers in fleet-state's geo sets: available (idle) or online (seen in the last 60 s). " +
				"The same global value on every replica: use max(), not sum().",
		}, []string{"state"}),
		outbox: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "dispatch_outbox_events",
			Help: "dispatch.events waiting in the Redis outbox for a Kafka acknowledgement (global; use max()).",
		}),
		streams: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "dispatch_offer_streams", Help: "Open courier offer streams (server-sent events) on this replica.",
		}),
		paused: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "dispatch_paused",
			Help: "1 while this replica waits for fleet-state to rebuild Redis and replays its own state after a Redis loss.",
		}),
		recoveries: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "dispatch_recoveries_total",
			Help: "Times this replica rebuilt dispatch state in Redis from Kafka (first start, or after a Redis loss).",
		}),
		orderEvents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dispatch_order_events_total",
			Help: "orders.events records applied, by type and result: applied, duplicate (event_id seen before), ignored.",
		}, []string{"type", "result"}),
		invalid: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dispatch_records_invalid_total", Help: "Kafka records that could not be decoded or used, by topic.",
		}, []string{"topic"}),
		publishFails: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "dispatch_publish_failures_total",
			Help: "dispatch.events records Kafka did not acknowledge; the outbox relay sends them again.",
		}),
	}
	reg.MustRegister(m.assignment, m.offers, m.etaRequests, m.pending, m.couriers, m.outbox, m.streams,
		m.paused, m.recoveries, m.orderEvents, m.invalid, m.publishFails)
	// Create every series up front, so dashboards and rate() see zeros instead of gaps.
	for _, o := range []string{outcomeAssigned, outcomeNoCourier} {
		m.assignment.WithLabelValues(o)
	}
	for _, o := range []string{"sent", "accepted", "declined", "expired", "withdrawn"} {
		m.offers.WithLabelValues(o)
	}
	for _, s := range []string{sourceOSRM, sourceFallback} {
		m.etaRequests.WithLabelValues(s)
	}
	m.setPending(nil)
	return m
}

// setPending replaces the pending-orders gauge with counts by ward. Every ward is set, zeros
// included, so a ward that empties drops to 0 instead of keeping its last value.
func (m *metrics) setPending(byWard map[string]int) {
	known := 0
	for _, w := range wards.All() {
		m.pending.WithLabelValues(w.ID).Set(float64(byWard[w.ID]))
		known += byWard[w.ID]
	}
	total := 0
	for _, n := range byWard {
		total += n
	}
	m.pending.WithLabelValues(wardOther).Set(float64(total - known))
}
