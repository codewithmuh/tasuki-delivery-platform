package clients

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/codewithmuh/tasuki-delivery-platform/services/simulator/internal/geo"
)

// Routing asks OSRM (services/routing) for routes on Tokyo's real streets.
type Routing struct {
	endpoint
}

// NewRouting returns a client for the OSRM server at baseURL (for example http://127.0.0.1:5000).
func NewRouting(baseURL string, httpClient *http.Client, observer Observer) *Routing {
	// OSRM answers a route across the 23 wards in milliseconds; waiting longer than this
	// means it is in trouble and a straight line is the better choice.
	return &Routing{endpoint: newEndpoint(baseURL, httpClient, 3*time.Second, observer)}
}

// Route returns the road geometry of the bicycle route from a to b.
func (r *Routing) Route(ctx context.Context, from, to geo.Point) ([]geo.Point, error) {
	// OSRM wants longitude first. overview=full returns the whole geometry, not a simplified one.
	path := "/route/v1/bicycle/" + lngLat(from) + ";" + lngLat(to) + "?overview=full&geometries=polyline&steps=false"
	var resp struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Routes  []struct {
			Geometry string  `json:"geometry"`
			Distance float64 `json:"distance"` // metres
			Duration float64 `json:"duration"` // seconds
		} `json:"routes"`
	}
	if _, err := r.do(ctx, request{call: "route", method: http.MethodGet, path: path}, &resp); err != nil {
		return nil, err
	}
	if resp.Code != "Ok" || len(resp.Routes) == 0 {
		return nil, fmt.Errorf("osrm: %s %s", resp.Code, resp.Message)
	}
	points, err := geo.DecodePolyline(resp.Routes[0].Geometry, 5)
	if err != nil {
		return nil, fmt.Errorf("osrm geometry: %w", err)
	}
	if len(points) == 0 {
		return nil, errors.New("osrm: empty geometry")
	}
	return points, nil
}

func lngLat(p geo.Point) string {
	return strconv.FormatFloat(p.Lng, 'f', 6, 64) + "," + strconv.FormatFloat(p.Lat, 'f', 6, 64)
}
