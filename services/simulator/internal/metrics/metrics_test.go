package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestHelpers(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg)

	m.ObserveRequest("order", "201", 120*time.Millisecond)
	m.Retried("order")
	m.MoveCourier("", "idle")
	m.MoveCourier("idle", "to_store")
	m.MoveCourier("to_store", "")

	if got := testutil.CollectAndCount(m.Requests, "simulator_client_request_duration_seconds"); got != 1 {
		t.Errorf("request series = %d, want 1", got)
	}
	if got := testutil.ToFloat64(m.Retries.WithLabelValues("order")); got != 1 {
		t.Errorf("retries = %v, want 1", got)
	}
	for state, want := range map[string]float64{"idle": 0, "to_store": 0} {
		if got := testutil.ToFloat64(m.CourierStates.WithLabelValues(state)); got != want {
			t.Errorf("couriers{state=%q} = %v, want %v", state, got, want)
		}
	}
	// Every metric is registered under the simulator_ prefix.
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if name := f.GetName(); len(name) < 10 || name[:10] != "simulator_" {
			t.Errorf("metric %q lacks the simulator_ prefix", name)
		}
	}
}
