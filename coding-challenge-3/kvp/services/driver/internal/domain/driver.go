// Package domain is driver-service's innermost layer (Clean Architecture):
// the Driver entity and its status rules (AC-02). Pure Go, no I/O.
//
// The repository applies each transition atomically with a conditional UPDATE
// (ADR-002). These functions define the rules and classify the outcome when
// that UPDATE matched no row (duplicate vs. not allowed).
package domain

import (
	"errors"
	"time"

	"ridehail/pkg/geo"
)

// Status is the driver's dispatch status.
type Status string

const (
	Offline   Status = "OFFLINE"
	Available Status = "AVAILABLE"
	Offered   Status = "OFFERED"
	Busy      Status = "BUSY"
)

// Driver is the aggregate stored by driver-service.
type Driver struct {
	ID                string
	Status            Status
	Latitude          float64
	Longitude         float64
	CurrentRideID     *string
	Version           int64
	LocationUpdatedAt *time.Time
	UpdatedAt         time.Time
}

// Location returns the driver's position.
func (d Driver) Location() geo.Point { return geo.Point{Latitude: d.Latitude, Longitude: d.Longitude} }

var (
	// ErrInvalidTransition: the requested status change is not allowed.
	ErrInvalidTransition = errors.New("invalid driver transition")
	// ErrNotAvailable: the driver cannot take an offer (not AVAILABLE).
	ErrNotAvailable = errors.New("driver not available")
	// ErrStale: the command refers to a ride the driver no longer holds.
	ErrStale = errors.New("command does not match the driver's current ride")
)

// SetAvailability handles PUT /drivers/{id}/availability.
// Only OFFLINE ⇄ AVAILABLE is allowed; OFFERED and BUSY drivers cannot go OFFLINE.
func SetAvailability(d Driver, target Status) (Driver, bool, error) {
	if target != Available && target != Offline {
		return d, false, ErrInvalidTransition
	}
	if d.Status == target {
		return d, false, nil // duplicate
	}
	if (d.Status == Offline && target == Available) || (d.Status == Available && target == Offline) {
		d.Status = target
		return d, true, nil
	}
	return d, false, ErrInvalidTransition
}

// Reserve creates an offer hold: AVAILABLE → OFFERED for rideID.
func Reserve(d Driver, rideID string) (Driver, bool, error) {
	switch {
	case d.Status == Available:
		d.Status = Offered
		d.CurrentRideID = &rideID
		return d, true, nil
	case d.Status == Offered && holds(d, rideID):
		return d, false, nil // duplicate reserve for the same ride
	}
	return d, false, ErrNotAvailable
}

// MarkBusy is the accepted offer: OFFERED → BUSY for the same ride.
func MarkBusy(d Driver, rideID string) (Driver, bool, error) {
	switch {
	case d.Status == Offered && holds(d, rideID):
		d.Status = Busy
		return d, true, nil
	case d.Status == Busy && holds(d, rideID):
		return d, false, nil // duplicate
	}
	return d, false, ErrStale
}

// Release frees the driver after reject/timeout/cancel/complete:
// OFFERED or BUSY → AVAILABLE, only if the driver still holds rideID.
// A release for any other ride is a no-op, so it can never free a driver who
// already moved on to another ride.
func Release(d Driver, rideID string) (Driver, bool, error) {
	if (d.Status == Offered || d.Status == Busy) && holds(d, rideID) {
		d.Status = Available
		d.CurrentRideID = nil
		return d, true, nil
	}
	return d, false, nil
}

// CanReceiveOffer reports whether the driver is eligible for a new offer.
func CanReceiveOffer(d Driver) bool { return d.Status == Available }

func holds(d Driver, rideID string) bool { return d.CurrentRideID != nil && *d.CurrentRideID == rideID }
