// Package sim runs a scenario: it seeds Tasuki through the admin API, starts one goroutine per
// courier and one per ward of customers, serves probes and metrics, and drains on shutdown.
package sim

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/clients"
	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/clock"
	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/courier"
	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/customer"
	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/metrics"
	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/rng"
	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/scenario"
	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/scenarios"
)

// tokenRefreshEvery and tokenRefreshAhead: every 10 minutes, re-issue courier tokens that
// expire within the next hour.
const (
	tokenRefreshEvery = 10 * time.Minute
	tokenRefreshAhead = time.Hour
)

// Options are per-run choices from the command line.
type Options struct {
	Fresh    bool          // seed new merchants and couriers even if the state file matches
	Duration time.Duration // overrides the scenario's duration when above 0
}

// LoadScenario loads a scenario from a file path, or by built-in name ("small").
func LoadScenario(nameOrPath string) (*scenario.Scenario, error) {
	if _, err := os.Stat(nameOrPath); err == nil {
		return scenario.Load(nameOrPath)
	}
	if data, ok := scenarios.Get(nameOrPath); ok {
		return scenario.Parse(data)
	}
	return nil, fmt.Errorf("scenario %q: no such file, and not a built-in scenario (%s)",
		nameOrPath, strings.Join(scenarios.Names(), ", "))
}

// Prepare returns the seeded merchants and couriers: from the state file when it was written
// for this scenario and orders service, otherwise by seeding them (and saving the file).
func Prepare(ctx context.Context, cfg Config, sc *scenario.Scenario, orders *clients.Orders, fresh bool, logger *slog.Logger) (*State, error) {
	fp := fingerprint(sc, cfg.OrdersURL)
	if !fresh {
		st, err := loadState(cfg.StateFile)
		switch {
		case err != nil:
			logger.Warn("ignoring the state file", "err", err)
		case st != nil && st.Fingerprint == fp:
			logger.Info("reusing seeded merchants and couriers", "state_file", cfg.StateFile,
				"merchants", len(st.Merchants), "couriers", len(st.Couriers))
			return st, nil
		case st != nil:
			logger.Info("the state file is for another scenario or orders service; seeding again", "state_file", cfg.StateFile)
		}
	}
	if cfg.AdminToken == "" {
		return nil, errors.New("ADMIN_TOKEN is required to seed merchants and couriers")
	}

	logger.Info("waiting for the orders service", "url", cfg.OrdersURL, "timeout", cfg.SeedWait.String())
	waitCtx, cancel := context.WithTimeout(ctx, cfg.SeedWait)
	defer cancel()
	if err := orders.WaitReady(waitCtx, 2*time.Second); err != nil {
		return nil, fmt.Errorf("orders service not ready: %w", err)
	}
	st, err := Seed(ctx, sc, orders)
	if err != nil {
		return nil, err
	}
	st.Fingerprint = fp
	if err := saveState(cfg.StateFile, st); err != nil {
		logger.Warn("could not save the state file; the next run will seed again", "err", err)
	}
	logger.Info("seeded", "merchants", len(st.Merchants), "couriers", len(st.Couriers), "state_file", cfg.StateFile)
	return st, nil
}

// Run runs the scenario until ctx ends or the scenario's duration passes, then drains: it
// stops placing orders, lets couriers finish the deliveries in hand (up to DrainTimeout)
// and waits for orders still in flight.
func Run(ctx context.Context, cfg Config, sc *scenario.Scenario, opts Options, logger *slog.Logger) error {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	m := metrics.New(reg)

	srv, err := startServer(ctx, cfg.Addr, reg, logger)
	if err != nil {
		return err
	}
	defer func() {
		if err := srv.shutdown(cfg.ShutdownTimeout); err != nil {
			logger.Error("server shutdown", "err", err)
		}
	}()

	httpClient := clients.NewHTTPClient()
	orders := clients.NewOrders(cfg.OrdersURL, cfg.AdminToken, httpClient, m)
	st, err := Prepare(ctx, cfg, sc, orders, opts.Fresh, logger)
	if err != nil {
		if ctx.Err() != nil { // stopped (SIGTERM) while waiting to seed: a clean exit
			logger.Info("stopped before seeding finished")
			return nil
		}
		return err
	}

	duration := sc.Duration
	if opts.Duration > 0 {
		duration = opts.Duration
	}
	runCtx, stopRun := context.WithCancel(ctx)
	if duration > 0 {
		stopRun() // replaced by a context that also ends when the duration is up
		runCtx, stopRun = context.WithTimeout(ctx, duration)
	}
	defer stopRun()

	now := time.Now()
	startOfDay := clock.SinceMidnight(now)
	if sc.Clock.Start != nil {
		startOfDay = sc.Clock.Start.Duration()
	}
	clk := clock.New(now, startOfDay, sc.Clock.Speedup)
	kit := newKitchen()
	toks := newTokens(st.Couriers)

	// Couriers get their own context: on shutdown they keep running (draining) after ctx ends,
	// until they finish or DrainTimeout passes.
	courierCtx, stopCouriers := context.WithCancel(context.WithoutCancel(ctx))
	defer stopCouriers()
	drain := make(chan struct{})

	var customersWG, couriersWG sync.WaitGroup
	for _, agent := range newCustomers(sc, st, clk, kit, orders, m, logger) {
		customersWG.Go(func() { agent.Run(runCtx) })
	}
	for _, c := range newCouriers(cfg, sc, st, clk, kit, toks, orders, httpClient, m, logger) {
		couriersWG.Go(func() { c.Run(courierCtx, drain) })
	}
	if cfg.AdminToken != "" {
		go refreshLoop(runCtx, cfg, st, toks, orders, logger)
	}

	srv.setReady(true)
	logger.Info("simulation running", "scenario", sc.Name, "seed", sc.Seed, "wards", len(sc.Wards),
		"couriers", len(st.Couriers), "time_of_day", scenario.TimeOfDay(startOfDay).String(),
		"speedup", sc.Clock.Speedup, "duration", duration.String(), "osrm", cfg.RoutingURL != "")
	<-runCtx.Done()

	srv.setReady(false)
	logger.Info("draining: no new orders; couriers finish the deliveries in hand", "timeout", cfg.DrainTimeout.String())
	close(drain)
	if !waitFor(&couriersWG, cfg.DrainTimeout) {
		logger.Warn("drain timeout: stopping couriers mid-delivery")
		stopCouriers()
		couriersWG.Wait()
	}
	customersWG.Wait() // their loops have stopped; this waits for orders still in flight
	logger.Info("simulation stopped")
	return nil
}

// newCustomers creates one customer agent per ward.
func newCustomers(sc *scenario.Scenario, st *State, clk clock.Clock, kit *kitchen, orders *clients.Orders,
	m *metrics.Metrics, logger *slog.Logger,
) []*customer.Agent {
	storesByWard := map[string][]customer.Store{}
	for _, merchant := range st.Merchants {
		for _, s := range merchant.Stores {
			storesByWard[s.Ward] = append(storesByWard[s.Ward], customer.Store{ID: s.ID, APIKey: merchant.APIKey})
		}
	}
	curve, _ := sc.Curve() // the scenario was validated
	deps := customer.Deps{
		Orders: orders, Kitchen: kit, Metrics: m, Logger: logger,
		InFlight: make(chan struct{}, sc.Demand.MaxInFlight),
	}
	var agents []*customer.Agent
	for _, wardID := range sc.Wards {
		stores := storesByWard[wardID]
		if len(stores) == 0 {
			logger.Warn("no stores in ward; no orders will be placed there", "ward", wardID)
			continue
		}
		ward, _ := scenario.LookupWard(wardID)
		agents = append(agents, customer.New(customer.Config{
			WardID: wardID, WardName: ward.NameEN, Centre: ward.Centre(), RadiusM: sc.WardRadiusM,
			BBox: scenario.CityBBox(), Share: sc.WardShare(wardID), Stores: stores,
			Curve: curve, Weather: sc.WeatherEvents(), Clock: clk,
			PrepMin: sc.Demand.PrepTime.Min, PrepMax: sc.Demand.PrepTime.Max,
		}, deps, rng.New(sc.Seed, "customers/"+wardID)))
	}
	return agents
}

// newCouriers creates one courier per seeded courier.
func newCouriers(cfg Config, sc *scenario.Scenario, st *State, clk clock.Clock, kit *kitchen, toks *tokens,
	orders *clients.Orders, httpClient *http.Client, m *metrics.Metrics, logger *slog.Logger,
) []*courier.Courier {
	deps := courier.Deps{
		Orders:   orders,
		Fleet:    clients.NewFleet(cfg.FleetIngestURL, httpClient, m),
		Dispatch: clients.NewDispatch(cfg.DispatchURL, httpClient, m, logger),
		Kitchen:  kit,
		Metrics:  m,
		Logger:   logger,
	}
	if cfg.RoutingURL != "" { // leave Router nil otherwise: couriers then ride in straight lines
		deps.Router = clients.NewRouting(cfg.RoutingURL, httpClient, m)
	}
	set := courier.Settings{
		Clock: clk, Weather: sc.WeatherEvents(),
		MovingInterval: sc.GPS.MovingInterval, IdleInterval: sc.GPS.IdleInterval, NoiseM: sc.GPS.NoiseM,
		AcceptProbability: sc.Couriers.Accept.Probability, HalvingDistanceM: sc.Couriers.Accept.HalvingDistanceM,
		ThinkMin: sc.Couriers.ThinkTime.Min, ThinkMax: sc.Couriers.ThinkTime.Max,
		DefaultPrep: (sc.Demand.PrepTime.Min + sc.Demand.PrepTime.Max) / 2,
	}
	couriers := make([]*courier.Courier, 0, len(st.Couriers))
	for _, seeded := range st.Couriers {
		plan := planCourier(sc, seeded.Index)
		speed := sc.Couriers.BicycleKmh
		if seeded.Vehicle == "scooter" {
			speed = sc.Couriers.ScooterKmh
		}
		couriers = append(couriers, courier.New(courier.Config{
			ID: seeded.ID, Token: toks.source(seeded.ID), SpeedKmh: speed, Home: plan.Home,
			ShiftStart: plan.ShiftStart, ShiftEnd: plan.ShiftEnd,
		}, set, deps,
			rng.New(sc.Seed, fmt.Sprintf("courier/%d/decide", seeded.Index)),
			rng.New(sc.Seed, fmt.Sprintf("courier/%d/gps", seeded.Index))))
	}
	return couriers
}

// refreshLoop keeps courier tokens fresh for runs longer than a token's lifetime.
func refreshLoop(ctx context.Context, cfg Config, st *State, toks *tokens, orders Reissuer, logger *slog.Logger) {
	ticker := time.NewTicker(tokenRefreshEvery)
	defer ticker.Stop()
	for {
		if refreshTokens(ctx, st, toks, orders, tokenRefreshAhead, logger) {
			if err := saveState(cfg.StateFile, st); err != nil {
				logger.Warn("could not save re-issued tokens", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// waitFor waits for wg up to timeout and reports whether it finished in time.
func waitFor(wg *sync.WaitGroup, timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}
