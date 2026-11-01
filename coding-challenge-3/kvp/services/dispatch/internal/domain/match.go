// Matching rules: select the driver for a ride (AC-04, ADR-001).
//
// It is a pure function over candidates. Redis GEO only supplies candidates;
// the final choice and its rules live here, so they are unit-tested.

package domain

import (
	"sort"

	"ridehail/pkg/geo"
)

// DefaultRadiusM is the default maximum pickup distance (2 km).
const DefaultRadiusM = 2000.0

// Candidate is a driver the index believes is near the pickup.
type Candidate struct {
	DriverID string
	Location geo.Point
	Status   string // last known status; only "AVAILABLE" is eligible
}

// Ranked is an eligible candidate with its distance to the pickup.
type Ranked struct {
	Candidate
	DistanceM float64
}

// Rank returns eligible candidates ordered by (distance, driverId).
// Excluded drivers, non-AVAILABLE drivers and drivers beyond radiusM are dropped.
func Rank(pickup geo.Point, cands []Candidate, radiusM float64, excluded map[string]bool) []Ranked {
	out := make([]Ranked, 0, len(cands))
	for _, c := range cands {
		if excluded[c.DriverID] || c.Status != "AVAILABLE" {
			continue
		}
		d := geo.HaversineM(pickup, c.Location)
		if d > radiusM {
			continue
		}
		out = append(out, Ranked{Candidate: c, DistanceM: d})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DistanceM != out[j].DistanceM {
			return out[i].DistanceM < out[j].DistanceM
		}
		return out[i].DriverID < out[j].DriverID // equidistant: smallest driverId wins
	})
	return out
}

// SelectNearest returns the best candidate, or ok=false when none is eligible.
func SelectNearest(pickup geo.Point, cands []Candidate, radiusM float64, excluded map[string]bool) (Ranked, bool) {
	r := Rank(pickup, cands, radiusM, excluded)
	if len(r) == 0 {
		return Ranked{}, false
	}
	return r[0], true
}
