// Package metrics defines the simulator's Prometheus metrics. They describe Tasuki as its
// clients see it: how many orders went in, how long the APIs took from the outside, how many
// GPS batches were shed and how many deliveries finished.
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics holds every collector. Create it once with New and share it between agents.
type Metrics struct {
	OrdersAttempted *prometheus.CounterVec // ward
	OrdersCreated   *prometheus.CounterVec // ward
	OrdersFailed    *prometheus.CounterVec // stage (quote, order), reason
	OrdersReplayed  prometheus.Counter
	Requests        *prometheus.HistogramVec // call, outcome
	Retries         *prometheus.CounterVec   // call
	LocationBatches *prometheus.CounterVec   // result
	LocationPoints  *prometheus.CounterVec   // result
	Offers          *prometheus.CounterVec   // outcome
	Deliveries      prometheus.Counter
	JobsAbandoned   *prometheus.CounterVec // reason
	CouriersActive  prometheus.Gauge
	CourierStates   *prometheus.GaugeVec   // state
	Routes          *prometheus.CounterVec // source
	DemandRate      *prometheus.GaugeVec   // ward
}

// New creates the metrics and registers them with reg.
func New(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		OrdersAttempted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "simulator_orders_attempted_total", Help: "Orders the simulated customers tried to place.",
		}, []string{"ward"}),
		OrdersCreated: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "simulator_orders_created_total", Help: "Orders the orders API confirmed (201).",
		}, []string{"ward"}),
		OrdersFailed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "simulator_orders_failed_total", Help: "Orders that failed, by stage (quote, order) and reason.",
		}, []string{"stage", "reason"}),
		OrdersReplayed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "simulator_orders_replayed_total",
			Help: "Order creations answered from the idempotency store (Idempotent-Replayed: true) after a retry.",
		}),
		Requests: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "simulator_client_request_duration_seconds",
			Help: "Latency of every HTTP call as the simulator saw it, by call and outcome (status code, timeout or network).",
			// Finer around the 300 ms order-latency SLO (docs/design.md §9).
			Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.2, 0.3, 0.5, 1, 2.5, 5, 10},
		}, []string{"call", "outcome"}),
		Retries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "simulator_request_retries_total", Help: "Requests retried after a timeout or 5xx (orders retry with the same Idempotency-Key).",
		}, []string{"call"}),
		LocationBatches: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "simulator_location_batches_total",
			Help: "GPS batches posted to fleet-ingest: sent, shed (503, dropped), failed (kept for the next try) or rejected (4xx).",
		}, []string{"result"}),
		LocationPoints: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "simulator_location_points_total",
			Help: "GPS points: accepted or dropped by fleet-ingest, or lost by the simulator (shed batches, full buffer).",
		}, []string{"result"}),
		Offers: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "simulator_offers_total",
			Help: "Dispatch offers by outcome: received, accepted, declined, lost (409 expired or taken), failed (accept errored), cancelled.",
		}, []string{"outcome"}),
		Deliveries: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "simulator_deliveries_completed_total", Help: "Orders the simulated couriers delivered.",
		}),
		JobsAbandoned: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "simulator_jobs_abandoned_total", Help: "Accepted jobs a courier gave up, by reason.",
		}, []string{"reason"}),
		CouriersActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "simulator_couriers_active", Help: "Couriers on shift and online.",
		}),
		CourierStates: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "simulator_couriers", Help: "Online couriers by state (idle, to_store, at_store, to_dropoff).",
		}, []string{"state"}),
		Routes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "simulator_routes_total", Help: "Routes planned, by source: osrm, or straight_line when OSRM was unavailable.",
		}, []string{"source"}),
		DemandRate: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "simulator_demand_orders_per_hour", Help: "The scenario's current expected order rate per ward (curve × weather).",
		}, []string{"ward"}),
	}
	reg.MustRegister(m.OrdersAttempted, m.OrdersCreated, m.OrdersFailed, m.OrdersReplayed, m.Requests, m.Retries,
		m.LocationBatches, m.LocationPoints, m.Offers, m.Deliveries, m.JobsAbandoned, m.CouriersActive,
		m.CourierStates, m.Routes, m.DemandRate)
	return m
}

// ObserveRequest records one HTTP call. It makes Metrics usable as the clients' observer.
func (m *Metrics) ObserveRequest(call, outcome string, d time.Duration) {
	m.Requests.WithLabelValues(call, outcome).Observe(d.Seconds())
}

// Retried records that a call is being retried.
func (m *Metrics) Retried(call string) { m.Retries.WithLabelValues(call).Inc() }

// MoveCourier updates the per-state courier gauge when a courier changes state. An empty
// state means "not online".
func (m *Metrics) MoveCourier(from, to string) {
	if from != "" {
		m.CourierStates.WithLabelValues(from).Dec()
	}
	if to != "" {
		m.CourierStates.WithLabelValues(to).Inc()
	}
}
