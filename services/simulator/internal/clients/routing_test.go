package clients

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/geo"
)

func TestRouteDecodesTheGeometry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if want := "/route/v1/bicycle/139.701600,35.658000;139.700500,35.690000"; r.URL.Path != want {
			t.Errorf("path = %s, want %s (longitude first)", r.URL.Path, want)
		}
		q := r.URL.Query()
		if q.Get("overview") != "full" || q.Get("geometries") != "polyline" {
			t.Errorf("query = %v", q)
		}
		// Google's documented example polyline: three points.
		_, _ = io.WriteString(w, `{"code": "Ok", "routes": [{"geometry": "_p~iF~ps|U_ulLnnqC_mqNvxq`+"`"+`@", "distance": 1, "duration": 1}]}`)
	}))
	defer srv.Close()

	points, err := NewRouting(srv.URL, nil, nil).Route(context.Background(),
		geo.Point{Lat: 35.658, Lng: 139.7016}, geo.Point{Lat: 35.690, Lng: 139.7005})
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 3 || points[0] != (geo.Point{Lat: 38.5, Lng: -120.2}) {
		t.Fatalf("points = %v", points)
	}
}

func TestRouteErrors(t *testing.T) {
	bodies := map[string]struct {
		status int
		body   string
	}{
		"http error":     {http.StatusBadRequest, `{"code": "NoRoute", "message": "Impossible route between points"}`},
		"not ok":         {http.StatusOK, `{"code": "NoSegment", "message": "Could not find a matching segment"}`},
		"no routes":      {http.StatusOK, `{"code": "Ok", "routes": []}`},
		"bad geometry":   {http.StatusOK, `{"code": "Ok", "routes": [{"geometry": "_p~i"}]}`},
		"empty geometry": {http.StatusOK, `{"code": "Ok", "routes": [{"geometry": ""}]}`},
	}
	for name, resp := range bodies {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(resp.status)
				_, _ = io.WriteString(w, resp.body)
			}))
			defer srv.Close()
			if _, err := NewRouting(srv.URL, nil, nil).Route(context.Background(), geo.Point{}, geo.Point{}); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}
