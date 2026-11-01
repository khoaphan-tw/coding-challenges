package domain

import (
	"testing"

	"ridehail/pkg/geo"
)

var pickup = geo.Point{Latitude: 10.7769, Longitude: 106.7009}

// north returns a point ~metres north of the pickup (1° latitude ≈ 111.2 km).
func north(m float64) geo.Point {
	return geo.Point{Latitude: pickup.Latitude + m/111195.0, Longitude: pickup.Longitude}
}

func c(id string, m float64, status string) Candidate {
	return Candidate{DriverID: id, Location: north(m), Status: status}
}

func TestNearestDriverSelected(t *testing.T) {
	// Example from the challenge: A 0.8 km, B 1.5 km, C 3.2 km, radius 2 km.
	cands := []Candidate{c("C", 3200, "AVAILABLE"), c("B", 1500, "AVAILABLE"), c("A", 800, "AVAILABLE")}
	got, ok := SelectNearest(pickup, cands, DefaultRadiusM, nil)
	if !ok || got.DriverID != "A" {
		t.Fatalf("want A, got %+v ok=%v", got, ok)
	}
	ranked := Rank(pickup, cands, DefaultRadiusM, nil)
	if len(ranked) != 2 || ranked[0].DriverID != "A" || ranked[1].DriverID != "B" {
		t.Fatalf("want [A B] ordered by distance, got %+v", ranked)
	}
}

func TestDriverOutsideRadiusExcluded(t *testing.T) {
	_, ok := SelectNearest(pickup, []Candidate{c("C", 3200, "AVAILABLE")}, DefaultRadiusM, nil)
	if ok {
		t.Fatal("driver at 3.2 km must be excluded with a 2 km radius")
	}
	if _, ok := SelectNearest(pickup, []Candidate{c("C", 3200, "AVAILABLE")}, 5000, nil); !ok {
		t.Fatal("radius must be configurable")
	}
}

func TestOfflineDriverExcluded(t *testing.T) {
	got, ok := SelectNearest(pickup, []Candidate{c("D1", 100, "OFFLINE"), c("D2", 900, "AVAILABLE")}, DefaultRadiusM, nil)
	if !ok || got.DriverID != "D2" {
		t.Fatalf("offline driver selected: %+v", got)
	}
}

func TestBusyAndOfferedDriversExcluded(t *testing.T) {
	cands := []Candidate{c("D1", 100, "BUSY"), c("D2", 200, "OFFERED"), c("D3", 900, "AVAILABLE")}
	got, ok := SelectNearest(pickup, cands, DefaultRadiusM, nil)
	if !ok || got.DriverID != "D3" {
		t.Fatalf("busy/offered driver selected: %+v", got)
	}
}

func TestNoDriverAvailable(t *testing.T) {
	if _, ok := SelectNearest(pickup, nil, DefaultRadiusM, nil); ok {
		t.Fatal("no candidates must select nobody")
	}
	cands := []Candidate{c("D1", 100, "OFFLINE"), c("D2", 100, "BUSY"), c("D3", 2500, "AVAILABLE")}
	if _, ok := SelectNearest(pickup, cands, DefaultRadiusM, nil); ok {
		t.Fatal("no suitable driver must select nobody")
	}
}

func TestEquidistantPicksSmallestDriverID(t *testing.T) {
	cands := []Candidate{c("D009", 500, "AVAILABLE"), c("D002", 500, "AVAILABLE"), c("D005", 500, "AVAILABLE")}
	got, _ := SelectNearest(pickup, cands, DefaultRadiusM, nil)
	if got.DriverID != "D002" {
		t.Fatalf("want D002, got %s", got.DriverID)
	}
}

func TestExcludedDriverSkipped(t *testing.T) {
	cands := []Candidate{c("D1", 100, "AVAILABLE"), c("D2", 300, "AVAILABLE")}
	got, ok := SelectNearest(pickup, cands, DefaultRadiusM, map[string]bool{"D1": true})
	if !ok || got.DriverID != "D2" {
		t.Fatalf("rejected driver must be skipped, got %+v", got)
	}
	if _, ok := SelectNearest(pickup, cands, DefaultRadiusM, map[string]bool{"D1": true, "D2": true}); ok {
		t.Fatal("all excluded must select nobody")
	}
}
