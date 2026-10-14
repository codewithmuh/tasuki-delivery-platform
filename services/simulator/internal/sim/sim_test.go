package sim

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/clients"
	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/geo"
	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/scenario"
)

var quiet = slog.New(slog.DiscardHandler)

// e2eScenario is a tiny, very fast city: 3 couriers moving 200× real speed, 2 orders a second.
const e2eScenario = `
name: e2e
seed: 7
clock: { start: "12:00", speedup: 200 }
wards: [shibuya]
merchants: 1
stores_per_ward: 2
couriers:
  count: 3
  think_time: { min: 1ms, max: 5ms }
demand:
  curve: [{ at: "00:00", per_hour: 7200 }]
  prep_time: { min: 10s, max: 20s }
gps: { moving_interval: 50ms, idle_interval: 200ms, noise_m: 3 }
`

func freePort(t *testing.T) string {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func testConfig(t *testing.T, f *fakeTasuki) Config {
	return Config{
		OrdersURL: f.srv.URL, FleetIngestURL: f.srv.URL, DispatchURL: f.srv.URL, RoutingURL: f.srv.URL,
		AdminToken: "admin", Addr: freePort(t), StateFile: filepath.Join(t.TempDir(), "state", "sim.json"),
		ShutdownTimeout: 2 * time.Second, DrainTimeout: 5 * time.Second, SeedWait: 5 * time.Second,
	}
}

// testClient does not keep connections alive, so it never leaves a spare connection that
// would make the server's graceful shutdown wait.
var testClient = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	resp, err := testClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// The whole simulator against a fake Tasuki: seeding, token refresh, orders, offers over SSE,
// OSRM routes, pickups, deliveries, GPS, probes, metrics and a clean drain.
func TestRunEndToEnd(t *testing.T) {
	f := newFakeTasuki(t)
	f.tokenTTL = 30 * time.Minute // expires within the hour: the simulator must re-issue them
	cfg := testConfig(t, f)
	sc, err := scenario.Parse([]byte(e2eScenario))
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), cfg, sc, Options{Duration: 4 * time.Second}, quiet) }()

	base := "http://" + cfg.Addr
	deadline := time.Now().Add(3 * time.Second)
	for {
		if code, _ := get(t, base+"/readyz"); code == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the simulator never became ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if code, _ := get(t, base+"/healthz"); code != http.StatusOK {
		t.Errorf("/healthz = %d", code)
	}
	time.Sleep(2 * time.Second)
	code, body := get(t, base+"/metrics")
	if code != http.StatusOK || !strings.Contains(body, "simulator_orders_created_total") ||
		!strings.Contains(body, `simulator_client_request_duration_seconds_count{call="order",outcome="201"}`) {
		t.Errorf("/metrics = %d, missing simulator series", code)
	}

	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	merchants, couriers, orders, delivered, points, reissued, routes, badProofs := f.counts()
	t.Logf("merchants=%d couriers=%d orders=%d delivered=%d points=%d reissued=%d routes=%d",
		merchants, couriers, orders, delivered, points, reissued, routes)
	if merchants != 1 || couriers != 3 {
		t.Errorf("seeded %d merchants and %d couriers, want 1 and 3", merchants, couriers)
	}
	if orders < 3 || delivered < 1 || points < 10 || routes < 2 {
		t.Errorf("too little happened: %d orders, %d delivered, %d GPS points, %d routes", orders, delivered, points, routes)
	}
	if badProofs != 0 {
		t.Errorf("%d deliveries had proof too far from the drop-off", badProofs)
	}
	if reissued != 3 {
		t.Errorf("re-issued %d tokens, want all 3", reissued)
	}

	info, err := os.Stat(cfg.StateFile)
	if err != nil {
		t.Fatalf("state file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("state file mode %o, want 600", perm)
	}
	st, _ := loadState(cfg.StateFile)
	if exp, ok := tokenExpiry(st.Couriers[0].Token); !ok || time.Until(exp) < 23*time.Hour {
		t.Error("the state file should hold the re-issued (long-lived) tokens")
	}
	if code, _ := get(t, base+"/healthz"); code != 0 {
		t.Error("the server should be stopped after Run returns")
	}
}

func TestPrepareReusesAMatchingStateFile(t *testing.T) {
	f := newFakeTasuki(t)
	cfg := testConfig(t, f)
	sc, _ := scenario.Parse([]byte(e2eScenario))
	orders := clients.NewOrders(f.srv.URL, "admin", nil, nil)

	first, err := Prepare(context.Background(), cfg, sc, orders, false, quiet)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Prepare(context.Background(), cfg, sc, orders, false, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if merchants, couriers, _, _, _, _, _, _ := f.counts(); merchants != 1 || couriers != 3 {
		t.Fatalf("seeded %d merchants and %d couriers; the second run should have reused them", merchants, couriers)
	}
	if again.Couriers[2].Token != first.Couriers[2].Token {
		t.Error("reused state should keep the same tokens")
	}

	// A different seed means a different city: seed again.
	sc.Seed++
	if _, err := Prepare(context.Background(), cfg, sc, orders, false, quiet); err != nil {
		t.Fatal(err)
	}
	// -fresh seeds again even when the file matches.
	if _, err := Prepare(context.Background(), cfg, sc, orders, true, quiet); err != nil {
		t.Fatal(err)
	}
	if merchants, _, _, _, _, _, _, _ := f.counts(); merchants != 3 {
		t.Errorf("merchants = %d, want 3 after a new seed and a fresh run", merchants)
	}
}

func TestPrepareErrors(t *testing.T) {
	sc, _ := scenario.Parse([]byte(e2eScenario))
	dir := t.TempDir()

	// A corrupt state file is ignored (and replaced) rather than fatal.
	corrupt := filepath.Join(dir, "corrupt.json")
	_ = os.WriteFile(corrupt, []byte("{"), 0o600)
	cfg := Config{StateFile: corrupt, SeedWait: 10 * time.Millisecond}
	if _, err := Prepare(context.Background(), cfg, sc, clients.NewOrders("http://127.0.0.1:1", "", nil, nil), false, quiet); err == nil ||
		!strings.Contains(err.Error(), "ADMIN_TOKEN") {
		t.Errorf("without a token: %v", err)
	}

	cfg.AdminToken = "admin"
	_, err := Prepare(context.Background(), cfg, sc, clients.NewOrders("http://127.0.0.1:1", "admin", nil, nil), false, quiet)
	if err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Errorf("with orders down: %v", err)
	}

	f := newFakeTasuki(t)
	cfg = testConfig(t, f)
	cfg.AdminToken = "wrong"
	if _, err := Prepare(context.Background(), cfg, sc, clients.NewOrders(f.srv.URL, "wrong", nil, nil), false, quiet); err == nil {
		t.Error("expected seeding to fail with a rejected admin token")
	}
}

func TestRunStopsCleanlyWhileWaitingToSeed(t *testing.T) {
	sc, _ := scenario.Parse([]byte(e2eScenario))
	cfg := Config{
		OrdersURL: "http://127.0.0.1:1", AdminToken: "admin", Addr: freePort(t), SeedWait: time.Minute,
		StateFile: filepath.Join(t.TempDir(), "s.json"), ShutdownTimeout: time.Second,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond) // SIGTERM during seeding
	defer cancel()
	if err := Run(ctx, cfg, sc, Options{}, quiet); err != nil {
		t.Fatalf("a stop before seeding finished should be clean, got %v", err)
	}
}

func TestRunFailsOnABadAddress(t *testing.T) {
	sc, _ := scenario.Parse([]byte(e2eScenario))
	if err := Run(context.Background(), Config{Addr: "bad-address"}, sc, Options{}, quiet); err == nil {
		t.Fatal("expected an error")
	}
}

func TestLoadScenario(t *testing.T) {
	if sc, err := LoadScenario("small"); err != nil || sc.Name != "small" {
		t.Errorf("built-in: %v, %v", sc, err)
	}
	path := filepath.Join(t.TempDir(), "mine.yaml")
	_ = os.WriteFile(path, []byte(e2eScenario), 0o600)
	if sc, err := LoadScenario(path); err != nil || sc.Name != "e2e" {
		t.Errorf("file: %v, %v", sc, err)
	}
	if _, err := LoadScenario("atlantis"); err == nil || !strings.Contains(err.Error(), "normal-weekday") {
		t.Errorf("unknown: %v", err)
	}
}

func TestPlanStores(t *testing.T) {
	sc, _ := scenario.Parse([]byte("name: s\nseed: 3\nwards: [shibuya, minato]\nmerchants: 3\nstores_per_ward: 4\ndemand: { curve: [{ at: \"00:00\", per_hour: 1 }] }\n"))
	plan := planStores(sc)
	if len(plan) != 3 {
		t.Fatalf("%d merchants, want 3", len(plan))
	}
	total := 0
	for m, stores := range plan {
		total += len(stores)
		if len(stores) == 0 {
			t.Errorf("merchant %d has no stores", m)
		}
		for _, s := range stores {
			ward, _ := scenario.LookupWard(s.Ward)
			if d := geo.Distance(ward.Centre(), geo.Point{Lat: s.Lat, Lng: s.Lng}); d > storeRadiusM+1 {
				t.Errorf("store %q is %.0f m from its ward centre", s.Name, d)
			}
		}
	}
	if total != 8 {
		t.Errorf("%d stores, want 8", total)
	}
	if again := planStores(sc); again[0][0] != plan[0][0] {
		t.Error("store placement is not deterministic")
	}
}

func TestPlanCourier(t *testing.T) {
	data, _ := os.ReadFile("../../scenarios/rainy-friday.yaml")
	sc, err := scenario.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	const n = 4000
	scooters, quitters, eligible := 0, 0, 0
	wards := map[string]int{}
	rain := 11*time.Hour + 30*time.Minute
	for i := range n {
		p := planCourier(sc, i)
		if p != planCourier(sc, i) {
			t.Fatal("planCourier is not deterministic")
		}
		if p.Vehicle == "scooter" {
			scooters++
		}
		wards[p.Ward]++
		if !scenario.CityBBox().Contains(p.Home) {
			t.Errorf("courier %d lives outside the city box", i)
		}
		if p.ShiftStart < rain && rain < originalEnd(sc, p) {
			eligible++
			if p.ShiftEnd == rain {
				quitters++
			}
		}
	}
	if share := float64(scooters) / n; math.Abs(share-0.3) > 0.03 {
		t.Errorf("scooter share %.3f, want about 0.3", share)
	}
	if share := float64(quitters) / float64(eligible); math.Abs(share-0.1) > 0.03 {
		t.Errorf("%.3f of couriers on shift at 11:30 log off in the rain, want about 0.1", share)
	}
	// Minato weighs 1.5 and Kita 1: Minato should have about half as many couriers again.
	if ratio := float64(wards["minato"]) / float64(wards["kita"]); ratio < 1.2 || ratio > 1.9 {
		t.Errorf("minato/kita courier ratio %.2f, want about 1.5", ratio)
	}
}

// originalEnd finds the end of the shift a courier plan started from.
func originalEnd(sc *scenario.Scenario, p courierPlan) time.Duration {
	for _, s := range sc.Couriers.Shifts {
		if s.Start.Duration() == p.ShiftStart {
			return s.End.Duration()
		}
	}
	return p.ShiftEnd
}

func TestStateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	if st, err := loadState(path); st != nil || err != nil {
		t.Fatalf("missing file: %v, %v", st, err)
	}
	want := &State{Fingerprint: "abc", Couriers: []SeededCourier{{Index: 0, ID: "cou_1", Token: "t", Vehicle: "bicycle"}}}
	if err := saveState(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := loadState(path)
	if err != nil || got.Fingerprint != "abc" || got.Couriers[0].ID != "cou_1" {
		t.Fatalf("round trip: %+v, %v", got, err)
	}
	if err := saveState("/dev/null/state.json", want); err == nil {
		t.Error("expected an error writing under /dev/null")
	}
	dir := t.TempDir()
	if _, err := loadState(dir); err == nil {
		t.Error("expected an error reading a directory")
	}
}

func TestFingerprint(t *testing.T) {
	sc, _ := scenario.Parse([]byte(e2eScenario))
	base := fingerprint(sc, "http://orders")
	if fingerprint(sc, "http://orders") != base {
		t.Error("fingerprint is not stable")
	}
	if fingerprint(sc, "http://other-orders") == base {
		t.Error("a different orders service must change the fingerprint")
	}
	sc.Couriers.Count++
	if fingerprint(sc, "http://orders") == base {
		t.Error("a different courier count must change the fingerprint")
	}
}

type fakeReissuer struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (f *fakeReissuer) ReissueCourierToken(_ context.Context, id string) (clients.Courier, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, id)
	return clients.Courier{ID: id, Token: "fresh-" + id}, f.err
}

func TestRefreshTokens(t *testing.T) {
	fake := &fakeTasuki{courierOf: map[string]string{}}
	fake.tokenTTL = 10 * time.Minute
	expiring := fake.jwt("cou_1")
	fake.tokenTTL = 10 * time.Hour
	valid := fake.jwt("cou_2")
	st := &State{Couriers: []SeededCourier{{ID: "cou_1", Token: expiring}, {ID: "cou_2", Token: valid}, {ID: "cou_3", Token: "opaque"}}}
	toks := newTokens(st.Couriers)

	r := &fakeReissuer{}
	if !refreshTokens(context.Background(), st, toks, r, time.Hour, quiet) {
		t.Fatal("expected a change")
	}
	if len(r.calls) != 1 || r.calls[0] != "cou_1" || toks.get("cou_1") != "fresh-cou_1" || st.Couriers[0].Token != "fresh-cou_1" {
		t.Errorf("calls %v, token %q", r.calls, toks.get("cou_1"))
	}
	if toks.source("cou_2")() != valid {
		t.Error("a valid token should be left alone")
	}

	failing := &fakeReissuer{err: errors.New("404")}
	st.Couriers[0].Token = expiring
	toks.set("cou_1", expiring)
	if refreshTokens(context.Background(), st, toks, failing, time.Hour, quiet) {
		t.Error("a failed re-issue changes nothing")
	}
}

func TestTokenExpiry(t *testing.T) {
	for _, bad := range []string{"", "a.b", "a.!!!.c", "a." + "eyJ4IjoxfQ" + ".c"} { // the last has no exp
		if _, ok := tokenExpiry(bad); ok {
			t.Errorf("tokenExpiry(%q) should fail", bad)
		}
	}
}

func TestKitchen(t *testing.T) {
	k := newKitchen()
	at := time.Now().Add(time.Minute)
	k.Prepare("ord_1", at)
	if got, ok := k.ReadyAt("ord_1"); !ok || !got.Equal(at) {
		t.Errorf("ReadyAt = %v, %v", got, ok)
	}
	if _, ok := k.ReadyAt("ord_2"); ok {
		t.Error("unknown order should not be found")
	}
	// Once the book is large, entries long past their ready time are dropped.
	old := time.Now().Add(-2 * time.Hour)
	for i := range 10_001 {
		k.Prepare(fmt.Sprintf("old_%d", i), old)
	}
	if _, ok := k.ReadyAt("ord_1"); !ok {
		t.Error("a recent order was pruned")
	}
	if len(k.ready) > 10 {
		t.Errorf("%d entries left after pruning, want only recent ones", len(k.ready))
	}
}

func TestConfigFromEnv(t *testing.T) {
	d := ConfigFromEnv(func(string) string { return "" })
	if d.OrdersURL != "http://127.0.0.1:8000" || d.Addr != ":8080" || d.DrainTimeout != time.Minute || d.RoutingURL == "" {
		t.Errorf("defaults: %+v", d)
	}
	env := map[string]string{
		"ORDERS_URL": "http://orders:8000/", "ROUTING_URL": "none", "PORT": "9090",
		"DRAIN_TIMEOUT": "5s", "SEED_WAIT": "nonsense", "ADMIN_TOKEN": "x",
	}
	c := ConfigFromEnv(func(k string) string { return env[k] })
	if c.OrdersURL != "http://orders:8000" || c.RoutingURL != "" || c.Addr != ":9090" || c.DrainTimeout != 5*time.Second ||
		c.SeedWait != 2*time.Minute || c.AdminToken != "x" {
		t.Errorf("overrides: %+v", c)
	}
	for in, want := range map[string]slog.Level{"debug": slog.LevelDebug, "WARN": slog.LevelWarn, "": slog.LevelInfo, "x": slog.LevelInfo} {
		if got := ParseLevel(in); got != want {
			t.Errorf("ParseLevel(%q) = %v", in, got)
		}
	}
}

func TestServerAndProbe(t *testing.T) {
	addr := freePort(t)
	srv, err := startServer(context.Background(), addr, prometheus.NewRegistry(), quiet)
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + addr
	if err := Probe(context.Background(), base+"/healthz"); err != nil {
		t.Errorf("healthz: %v", err)
	}
	if err := Probe(context.Background(), base+"/readyz"); err == nil {
		t.Error("readyz should fail before setReady")
	}
	srv.setReady(true)
	if err := Probe(context.Background(), base+"/readyz"); err != nil {
		t.Errorf("readyz after setReady: %v", err)
	}
	if err := srv.shutdown(3 * time.Second); err != nil {
		t.Fatal(err)
	}
	if err := Probe(context.Background(), "://bad"); err == nil {
		t.Error("expected an error for a bad URL")
	}
}

func TestWaitFor(t *testing.T) {
	var wg sync.WaitGroup
	if !waitFor(&wg, time.Second) {
		t.Error("an empty group finishes at once")
	}
	wg.Add(1)
	if waitFor(&wg, 10*time.Millisecond) {
		t.Error("a busy group should time out")
	}
	wg.Done()
}
