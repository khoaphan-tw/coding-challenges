// Package geo holds coordinates, validation and Haversine distance.
package geo

import (
	"errors"
	"math"
)

// EarthRadiusM is the mean Earth radius (IUGG) in metres.
const EarthRadiusM = 6371008.8

// Point is a WGS84 coordinate. Pointer fields in requests are used to detect
// "missing" values; Point itself is always complete.
type Point struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

// ErrInvalidCoordinates is returned for out-of-range or non-finite values.
var ErrInvalidCoordinates = errors.New("invalid coordinates")

// Validate checks latitude ∈ [-90, 90] and longitude ∈ [-180, 180].
func (p Point) Validate() error {
	if math.IsNaN(p.Latitude) || math.IsNaN(p.Longitude) ||
		math.IsInf(p.Latitude, 0) || math.IsInf(p.Longitude, 0) {
		return ErrInvalidCoordinates
	}
	if p.Latitude < -90 || p.Latitude > 90 || p.Longitude < -180 || p.Longitude > 180 {
		return ErrInvalidCoordinates
	}
	return nil
}

// PointInput is the JSON shape of a coordinate where both fields are required.
type PointInput struct {
	Latitude  *float64 `json:"latitude"`
	Longitude *float64 `json:"longitude"`
}

// ToPoint converts and validates; missing fields are invalid.
func (in *PointInput) ToPoint() (Point, error) {
	if in == nil || in.Latitude == nil || in.Longitude == nil {
		return Point{}, ErrInvalidCoordinates
	}
	p := Point{Latitude: *in.Latitude, Longitude: *in.Longitude}
	return p, p.Validate()
}

// HaversineM returns the great-circle distance between a and b in metres.
func HaversineM(a, b Point) float64 {
	const rad = math.Pi / 180
	lat1, lat2 := a.Latitude*rad, b.Latitude*rad
	dLat := (b.Latitude - a.Latitude) * rad
	dLng := (b.Longitude - a.Longitude) * rad
	h := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1)*math.Cos(lat2)*math.Sin(dLng/2)*math.Sin(dLng/2)
	return 2 * EarthRadiusM * math.Asin(math.Min(1, math.Sqrt(h)))
}
