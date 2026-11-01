// Package usecase is ride-service's application layer (Clean Architecture).
//
// It holds the use cases (create a ride, change its status, handle saga
// commands) and declares the ports it needs as interfaces. Adapters in
// internal/adapter implement them. Dependencies point inward only:
// adapter → usecase → domain.
package usecase

import (
	"context"
	"errors"

	"ridehail/pkg/messaging"
	"ridehail/services/ride/internal/domain"
)

// Application errors, mapped to HTTP/problem codes by the HTTP adapter.
var (
	ErrRideNotFound     = errors.New("ride not found")
	ErrUnknownPassenger = errors.New("passengerId does not reference a known passenger")
	ErrKeyReused        = errors.New("Idempotency-Key was already used with a different request")
)

// TxManager runs fn in one transaction; repositories, the outbox and the
// inbox called with the ctx passed to fn all join it.
type TxManager interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// RideRepository persists rides and their status history.
type RideRepository interface {
	Get(ctx context.Context, id string) (domain.Ride, error) // ErrRideNotFound
	// GetForUpdate loads the ride and locks it until the transaction ends.
	GetForUpdate(ctx context.Context, id string) (domain.Ride, error)
	Insert(ctx context.Context, r domain.Ride) (domain.Ride, error)
	// Update stores status/assignment and increments the version.
	Update(ctx context.Context, r domain.Ride) (domain.Ride, error)
	AddTransition(ctx context.Context, t domain.Transition) error
	History(ctx context.Context, rideID string) ([]domain.Transition, error)
}

// PassengerRepository reads seeded passengers.
type PassengerRepository interface {
	Exists(ctx context.Context, id string) (bool, error)
}

// IdempotencyRecord is what a previous request with the same key produced.
type IdempotencyRecord struct {
	RequestHash string
	Ride        domain.Ride // snapshot of the original response
}

// IdempotencyStore keeps Idempotency-Keys with the original result.
type IdempotencyStore interface {
	// Reserve claims key for a new ride. If the key exists it returns the
	// earlier record instead (a concurrent caller waits for the first to commit).
	Reserve(ctx context.Context, key, requestHash, rideID string) (*IdempotencyRecord, error)
	SaveResult(ctx context.Context, key string, r domain.Ride) error
}

// Publisher writes messages to the transactional outbox (same transaction).
type Publisher interface {
	Publish(ctx context.Context, topic, key string, env messaging.Envelope) (messaging.Envelope, error)
}

// Inbox records processed message ids; Claim returns false for a duplicate.
type Inbox interface {
	Claim(ctx context.Context, eventID string) (bool, error)
}
