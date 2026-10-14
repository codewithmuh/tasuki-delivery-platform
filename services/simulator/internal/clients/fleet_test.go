package clients

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSendLocations(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/locations" || r.Header.Get("Authorization") != "Bearer jwt" {
			t.Errorf("got %s with %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		points := decodeBody(t, r)["points"].([]any)
		idle, busy := points[0].(map[string]any), points[1].(map[string]any)
		if _, ok := idle["order_id"]; ok {
			t.Error("an idle point should not carry order_id")
		}
		if busy["order_id"] != "ord_1" || busy["status"] != "to_store" || busy["device_time"] != "2026-10-14T03:00:04.123Z" {
			t.Errorf("busy point = %v", busy)
		}
		for _, field := range []string{"lat", "lng", "accuracy_m", "speed_mps", "heading_deg", "device_time", "status"} {
			if _, ok := idle[field]; !ok {
				t.Errorf("point is missing %q", field)
			}
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"accepted": 2, "dropped": 0}`)
	}))
	defer srv.Close()

	at := time.Date(2026, 10, 14, 3, 0, 4, 123_000_000, time.UTC)
	res, err := NewFleet(srv.URL, nil, nil).SendLocations(context.Background(), "jwt", []Location{
		{Lat: 35.66, Lng: 139.70, DeviceTime: at, Status: "idle"},
		{Lat: 35.66, Lng: 139.70, SpeedMPS: 4.2, HeadingDeg: 90, DeviceTime: at, Status: "to_store", OrderID: "ord_1"},
	})
	if err != nil || res.Accepted != 2 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestSendLocationsShedIsNotRetried(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.Header().Set("Retry-After", "7")
		writeError(w, http.StatusServiceUnavailable, "overloaded")
	}))
	defer srv.Close()

	_, err := NewFleet(srv.URL, nil, nil).SendLocations(context.Background(), "jwt", []Location{{Status: "idle"}})
	wait, shed := IsLoadShed(err)
	if !shed || wait != 7*time.Second || attempts != 1 {
		t.Fatalf("shed=%v wait=%v attempts=%d err=%v", shed, wait, attempts, err)
	}
}

func TestIsLoadShedOnlyForServiceUnavailable(t *testing.T) {
	for _, err := range []error{nil, errors.New("eof"), &APIError{Status: 500}, &APIError{Status: 401}} {
		if _, shed := IsLoadShed(err); shed {
			t.Errorf("%v is not load shedding", err)
		}
	}
}
