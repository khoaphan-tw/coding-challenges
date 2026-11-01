// Package domain is dispatch-service's innermost layer (Clean Architecture):
// the dispatch Saga and RideOffer entities, their statuses, and the pure
// matching rules (match.go). No I/O, no imports of outer layers.
package domain

import (
	"time"

	"ridehail/pkg/geo"
)

// Saga statuses.
const (
	Started          = "STARTED"
	Matching         = "MATCHING"
	Reserving        = "RESERVING"
	Offered          = "OFFERED"
	Accepted         = "ACCEPTED"
	WaitingForDriver = "WAITING_FOR_DRIVER"
	Completed        = "COMPLETED"
	Cancelled        = "CANCELLED"
)

// Offer statuses.
const (
	OfferPending   = "PENDING"
	OfferAccepted  = "ACCEPTED"
	OfferRejected  = "REJECTED"
	OfferExpired   = "EXPIRED"
	OfferCancelled = "CANCELLED"
)

// Saga is one ride's dispatch state (the orchestrator's aggregate).
type Saga struct {
	RideID            string
	Status            string
	Pickup            geo.Point
	RadiusM           float64
	ReservingDriverID *string // driver asked to reserve (RESERVING)
	ReservingOfferID  *string // offer id pre-allocated for that reservation
	CurrentOfferID    *string // pending/accepted offer (OFFERED, ACCEPTED)
	Round             int
	RoundSkips        []string // drivers lost to another ride this round (not exclusions)
	NextRetryAt       *time.Time
	Version           int64
	TraceParent       string // the ride's trace (one trace per ride)
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// Terminal reports whether the saga is finished.
func (s Saga) Terminal() bool { return s.Status == Completed || s.Status == Cancelled }

// Offer is a ride offered to one driver (RideOffer).
type Offer struct {
	ID          string
	RideID      string
	DriverID    string
	Status      string
	CreatedAt   time.Time
	ExpiresAt   time.Time
	RespondedAt *time.Time
}

// Step is one audit row of a saga ("what happened to ride X?").
type Step struct {
	RideID   string
	Name     string
	Detail   string
	DriverID string
	OfferID  string
	EventID  string
	At       time.Time
}

// Ptr returns &s, or nil for "".
func Ptr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Val returns *p, or "" for nil.
func Val(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
