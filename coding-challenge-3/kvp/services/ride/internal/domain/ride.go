// Package domain is ride-service's innermost layer (Clean Architecture):
// the Ride entity, its explicit state machine (AC-06) and validation.
// It is pure Go: no I/O, no framework, no imports of outer layers.
package domain

import (
	"errors"
	"time"

	"ridehail/pkg/geo"
)

// Status is the ride lifecycle state.
type Status string

const (
	Requested      Status = "REQUESTED"
	Matching       Status = "MATCHING"
	DriverAssigned Status = "DRIVER_ASSIGNED"
	DriverAccepted Status = "DRIVER_ACCEPTED"
	Cancelled      Status = "CANCELLED"
	Completed      Status = "COMPLETED"
)

// Terminal reports whether no further transitions are allowed.
func (s Status) Terminal() bool { return s == Cancelled || s == Completed }

// Ride is the aggregate stored by ride-service.
type Ride struct {
	ID               string
	PassengerID      string
	Pickup           geo.Point
	Destination      geo.Point
	Status           Status
	AssignedDriverID *string
	CurrentOfferID   *string
	Version          int64
	TraceParent      string // W3C traceparent of the creating request (one trace per ride)
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// ActionKind names a state-machine input.
type ActionKind string

const (
	StartMatching ActionKind = "StartMatching"
	AssignDriver  ActionKind = "AssignDriver"
	Unassign      ActionKind = "UnassignDriver"
	MarkAccepted  ActionKind = "MarkAccepted"
	Cancel        ActionKind = "Cancel"
	Complete      ActionKind = "Complete"
)

// Action is a requested transition. DriverID/OfferID are used by the
// driver-related actions so duplicates and stale commands can be detected.
type Action struct {
	Kind     ActionKind
	DriverID string
	OfferID  string
}

var (
	// ErrInvalidTransition means the action is not allowed from the current state.
	ErrInvalidTransition = errors.New("invalid ride transition")
	// ErrPassengerRequired: a ride needs a passenger id.
	ErrPassengerRequired = errors.New("passengerId is required")
)

// NewRide creates a ride in REQUESTED after validating its inputs (AC-01).
func NewRide(id, passengerID string, pickup, destination geo.Point, now time.Time) (Ride, error) {
	if passengerID == "" {
		return Ride{}, ErrPassengerRequired
	}
	if err := pickup.Validate(); err != nil {
		return Ride{}, err
	}
	if err := destination.Validate(); err != nil {
		return Ride{}, err
	}
	return Ride{ID: id, PassengerID: passengerID, Pickup: pickup, Destination: destination,
		Status: Requested, Version: 1, CreatedAt: now, UpdatedAt: now}, nil
}

// Transition is one row of a ride's status history.
type Transition struct {
	RideID   string
	From     *Status
	To       Status
	Action   string
	DriverID string
	OfferID  string
	Cause    string // event id ("event:…") or HTTP request ("http:…")
	At       time.Time
}

// Apply returns the ride after the action.
//
//   - changed=true, err=nil: a valid transition; the caller persists it.
//   - changed=false, err=nil: a duplicate (already in the target state with the
//     same data) or a stale command for an older offer: a safe no-op.
//   - err=ErrInvalidTransition: the transition is not allowed.
func Apply(r Ride, a Action) (next Ride, changed bool, err error) {
	next = r
	switch a.Kind {
	case StartMatching:
		switch r.Status {
		case Requested:
			next.Status = Matching
			return next, true, nil
		case Matching:
			return r, false, nil // duplicate
		}

	case AssignDriver:
		switch {
		case r.Status == Matching:
			next.Status = DriverAssigned
			next.AssignedDriverID = strPtr(a.DriverID)
			next.CurrentOfferID = strPtr(a.OfferID)
			return next, true, nil
		case r.Status == DriverAssigned && eq(r.CurrentOfferID, a.OfferID):
			return r, false, nil // duplicate
		}

	case Unassign: // rejection or timeout of the current offer
		switch {
		case r.Status == DriverAssigned && eq(r.CurrentOfferID, a.OfferID):
			next.Status = Matching
			next.AssignedDriverID = nil
			next.CurrentOfferID = nil
			return next, true, nil
		case r.Status == Matching, r.Status == DriverAssigned:
			return r, false, nil // duplicate, or a stale command for an older offer
		}

	case MarkAccepted:
		switch {
		case r.Status == DriverAssigned && eq(r.CurrentOfferID, a.OfferID):
			next.Status = DriverAccepted
			return next, true, nil
		case r.Status == DriverAccepted && eq(r.CurrentOfferID, a.OfferID):
			return r, false, nil // duplicate
		}

	case Cancel:
		switch r.Status {
		case Requested, Matching, DriverAssigned, DriverAccepted:
			next.Status = Cancelled
			return next, true, nil
		case Cancelled:
			return r, false, nil // duplicate
		}

	case Complete:
		switch r.Status {
		case DriverAccepted:
			next.Status = Completed
			return next, true, nil
		case Completed:
			return r, false, nil // duplicate
		}
	}
	return r, false, ErrInvalidTransition
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func eq(p *string, s string) bool { return p != nil && *p == s }
