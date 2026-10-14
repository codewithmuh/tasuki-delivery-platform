// Package geo has the small amount of geometry the simulator needs: distances on the Earth,
// random points near a ward centre, Google polyline decoding and moving along a route.
package geo

import (
	"math"
	"math/rand/v2"
)

// Point is a WGS 84 coordinate in decimal degrees.
type Point struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

// earthRadiusM is the mean Earth radius in metres. A sphere is accurate to about 0.5 %, which
// is plenty for a simulation.
const earthRadiusM = 6_371_008.8

// Distance returns the great-circle distance between a and b in metres (the haversine formula).
func Distance(a, b Point) float64 {
	lat1, lat2 := radians(a.Lat), radians(b.Lat)
	sinLat := math.Sin((lat2 - lat1) / 2)
	sinLng := math.Sin(radians(b.Lng-a.Lng) / 2)
	h := sinLat*sinLat + math.Cos(lat1)*math.Cos(lat2)*sinLng*sinLng
	return 2 * earthRadiusM * math.Asin(math.Sqrt(h))
}

// Bearing returns the compass direction from a to b in degrees: 0 is north, 90 is east.
// The result is always in [0, 360), as the location contract requires.
func Bearing(a, b Point) float64 {
	lat1, lat2 := radians(a.Lat), radians(b.Lat)
	dLng := radians(b.Lng - a.Lng)
	y := math.Sin(dLng) * math.Cos(lat2)
	x := math.Cos(lat1)*math.Sin(lat2) - math.Sin(lat1)*math.Cos(lat2)*math.Cos(dLng)
	deg := math.Mod(degrees(math.Atan2(y, x))+360, 360)
	if deg >= 360 { // guards against rounding up to exactly 360
		deg = 0
	}
	return deg
}

// Offset moves p by the given distances north and east, in metres. It treats the Earth as
// flat around p, which is accurate to well under a metre over the few kilometres of a ward.
func Offset(p Point, northM, eastM float64) Point {
	dLat := northM / earthRadiusM
	dLng := eastM / (earthRadiusM * math.Cos(radians(p.Lat)))
	return Point{Lat: p.Lat + degrees(dLat), Lng: p.Lng + degrees(dLng)}
}

// RandomInDisc returns a point spread evenly over the disc of radiusM metres around centre.
func RandomInDisc(r *rand.Rand, centre Point, radiusM float64) Point {
	// The square root keeps the density even; without it, points bunch up near the centre.
	dist := radiusM * math.Sqrt(r.Float64())
	angle := 2 * math.Pi * r.Float64()
	return Offset(centre, dist*math.Cos(angle), dist*math.Sin(angle))
}

// Noisy returns p moved by Gaussian noise with a standard deviation of sigmaM metres in each
// direction, the way a phone's GPS fix wanders around the true position.
func Noisy(r *rand.Rand, p Point, sigmaM float64) Point {
	return Offset(p, r.NormFloat64()*sigmaM, r.NormFloat64()*sigmaM)
}

// BBox is a latitude/longitude rectangle.
type BBox struct {
	MinLat float64 `json:"min_lat"`
	MaxLat float64 `json:"max_lat"`
	MinLng float64 `json:"min_lng"`
	MaxLng float64 `json:"max_lng"`
}

// Contains reports whether p lies inside the box (edges included).
func (b BBox) Contains(p Point) bool {
	return p.Lat >= b.MinLat && p.Lat <= b.MaxLat && p.Lng >= b.MinLng && p.Lng <= b.MaxLng
}

// Clamp returns the point inside the box that is closest to p.
func (b BBox) Clamp(p Point) Point {
	return Point{
		Lat: math.Min(math.Max(p.Lat, b.MinLat), b.MaxLat),
		Lng: math.Min(math.Max(p.Lng, b.MinLng), b.MaxLng),
	}
}

func radians(deg float64) float64 { return deg * math.Pi / 180 }
func degrees(rad float64) float64 { return rad * 180 / math.Pi }
