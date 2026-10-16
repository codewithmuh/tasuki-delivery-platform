// Package viewer simulates customers watching their delivery on the tracking page. Each viewer
// holds its order's tracking stream open until the server ends it, as a phone with the page
// open would, and measures how old every courier position is when it arrives: the freshness a
// customer actually sees, including the time tracking took to send it.
package viewer

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/clients"
	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/metrics"
)

// Tracker opens an order's tracking stream. *clients.Tracking implements it.
type Tracker interface {
	Watch(ctx context.Context, token string, handle func(clients.TrackEvent)) error
}

// Pool runs the viewers. Its capacity bounds how many streams are open at once, so a scenario
// cannot grow goroutines and connections without limit when deliveries slow down.
type Pool struct {
	ctx     context.Context
	tracker Tracker
	metrics *metrics.Metrics
	log     *slog.Logger
	slots   chan struct{}
	wg      sync.WaitGroup
	now     func() time.Time
}

// NewPool returns a pool of at most maxViewers open streams. Viewers stop when ctx ends.
func NewPool(ctx context.Context, tracker Tracker, maxViewers int, m *metrics.Metrics, logger *slog.Logger) *Pool {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Pool{
		ctx: ctx, tracker: tracker, metrics: m, log: logger,
		slots: make(chan struct{}, max(1, maxViewers)),
		now:   time.Now,
	}
}

// Watch starts following an order in the background and never blocks. An order whose
// tracking_url has no token, or that arrives while the pool is full, is counted and skipped.
func (p *Pool) Watch(orderID, trackingURL string) {
	token, ok := clients.TokenFromURL(trackingURL)
	if !ok {
		p.metrics.TrackingStreams.WithLabelValues("bad_url").Inc()
		p.log.Warn("tracking_url has no token; not watching", "order_id", orderID)
		return
	}
	select {
	case p.slots <- struct{}{}:
	default:
		p.metrics.TrackingStreams.WithLabelValues("capped").Inc()
		return
	}
	p.metrics.TrackingViewers.Inc()
	p.wg.Go(func() {
		defer func() {
			p.metrics.TrackingViewers.Dec()
			<-p.slots
		}()
		err := p.tracker.Watch(p.ctx, token, p.observe)
		result := "ended"
		switch {
		case err == nil:
		case p.ctx.Err() != nil:
			result = "stopped"
		default:
			result = clients.Reason(err)
			p.log.Debug("stopped watching", "order_id", orderID, "err", err)
		}
		p.metrics.TrackingStreams.WithLabelValues(result).Inc()
	})
}

// observe records one event from a stream.
func (p *Pool) observe(ev clients.TrackEvent) {
	switch {
	case ev.Position != nil:
		p.metrics.TrackingEvents.WithLabelValues("position").Inc()
		p.metrics.TrackingPositionAge.Observe(max(0, p.now().Sub(ev.Position.ReceivedAt).Seconds()))
	case ev.Status != nil:
		p.metrics.TrackingEvents.WithLabelValues("status").Inc()
	case ev.End != "":
		p.metrics.TrackingEvents.WithLabelValues("end").Inc()
	}
}

// Wait returns once every viewer has stopped: their streams ended, or the pool's context did.
func (p *Pool) Wait() { p.wg.Wait() }
