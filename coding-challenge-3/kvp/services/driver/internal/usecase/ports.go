// Package usecase is driver-service's application layer (Clean Architecture):
// availability, location, and the saga commands (reserve, busy, release).
// It declares the ports it needs; adapters implement them.
package usecase

import (
	"context"
	"errors"

	"ridehail/pkg/geo"
	"ridehail/pkg/messaging"
	"ridehail/services/driver/internal/domain"
)

// Application errors, mapped to HTTP codes by the HTTP adapter.
var (
	ErrDriverNotFound = errors.New("driver not found")
	ErrInvalidStatus  = errors.New("status must be AVAILABLE or OFFLINE")
)

// TxManager runs fn in one transaction shared by repositories, outbox and inbox.
type TxManager interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// DriverRepository persists drivers. Every status change is an ATOMIC
// compare-and-set (ADR-002): it changes the row only if the condition still
// holds and reports applied=false otherwise. Two concurrent reservations of
// one driver: exactly one is applied.
type DriverRepository interface {
	Get(ctx context.Context, id string) (domain.Driver, error) // ErrDriverNotFound
	List(ctx context.Context, status string) ([]domain.Driver, error)

	// SetStatusIf: from → to, only when the current status is `from` (availability API).
	SetStatusIf(ctx context.Context, id string, from, to domain.Status) (domain.Driver, bool, error)
	// ReserveIfAvailable: AVAILABLE → OFFERED for rideID.
	ReserveIfAvailable(ctx context.Context, id, rideID string) (domain.Driver, bool, error)
	// MarkBusyIfOffered: OFFERED → BUSY, only while holding rideID.
	MarkBusyIfOffered(ctx context.Context, id, rideID string) (domain.Driver, bool, error)
	// ReleaseIfHeld: OFFERED|BUSY → AVAILABLE, only while holding rideID.
	ReleaseIfHeld(ctx context.Context, id, rideID string) (domain.Driver, bool, error)

	UpdateLocation(ctx context.Context, id string, p geo.Point) (domain.Driver, error)
	// Upsert creates or resets a driver (fixture); applied=false if it is OFFERED/BUSY.
	Upsert(ctx context.Context, id string, status domain.Status, p geo.Point) (domain.Driver, bool, error)
}

// Publisher writes messages to the transactional outbox.
type Publisher interface {
	Publish(ctx context.Context, topic, key string, env messaging.Envelope) (messaging.Envelope, error)
}

// Inbox records processed message ids; Claim returns false for a duplicate.
type Inbox interface {
	Claim(ctx context.Context, eventID string) (bool, error)
}
