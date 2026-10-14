package scenarios

import (
	"slices"
	"testing"
)

func TestNamesAndGet(t *testing.T) {
	names := Names()
	for _, want := range []string{"normal-weekday", "rainy-friday", "small"} {
		if !slices.Contains(names, want) {
			t.Errorf("Names() = %v, missing %q", names, want)
		}
		if data, ok := Get(want); !ok || len(data) == 0 {
			t.Errorf("Get(%q) returned nothing", want)
		}
	}
	if _, ok := Get("does-not-exist"); ok {
		t.Error("Get returned a scenario that does not exist")
	}
}
