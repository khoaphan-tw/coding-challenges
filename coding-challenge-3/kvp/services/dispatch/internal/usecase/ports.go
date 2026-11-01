// Package usecase is dispatch-service's application layer (Clean Architecture):
// the dispatch saga orchestrator (ADR-003), offer responses, the timeout and
// retry workers, and the GEO projection. It declares the ports it needs;
// adapters (Postgres, Redis, Redpanda, HTTP) implement or call them.
package usecase

import (
	"context"
	"errors"
	"time"

	"ridehail/pkg/geo"
	"ridehail/pkg/messaging"
	"ridehail/services/dispatch/internal/domain"
)

// Application errors, mapped to HTTP codes by the HTTP adapter.
var (
	ErrSagaNotFound          = errors.New("no dispatch for this ride")
	ErrOfferNotFound         = errors.New("no offer for this ride and driver")
	ErrOfferNotCurrent       = errors.New("this offer is no longer open")
	ErrOfferExpired          = errors.New("the offer expired before the response")
	ErrDuplicatePendingOffer = errors.New("driver or ride already has a pending offer")
)

// TxManager runs fn in one transaction shared by repositories, outbox and inbox.
type TxManager interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// SagaRepository persists sagas and their audit steps.
type SagaRepository interface {
	// Create inserts a new saga; created=false if one exists for the ride (ride id is the key).
	Create(ctx context.Context, s domain.Saga) (created bool, err error)
	Get(ctx context.Context, rideID string) (domain.Saga, error) // ErrSagaNotFound
	// GetForUpdate locks the saga row: every saga change is serialised per ride.
	GetForUpdate(ctx context.Context, rideID string) (domain.Saga, error)
	// TryGetForUpdate locks the row unless another transaction holds it (SKIP LOCKED).
	TryGetForUpdate(ctx context.Context, rideID string) (domain.Saga, bool, error)
	Save(ctx context.Context, s domain.Saga) error
	AddStep(ctx context.Context, st domain.Step) error
	Steps(ctx context.Context, rideID string) ([]domain.Step, error)
	// DueForRetry lists sagas waiting past next_retry_at, or stuck longer than stuckAfter.
	DueForRetry(ctx context.Context, stuckAfter time.Duration, limit int) ([]string, error)
	// WakeWaiting makes WAITING_FOR_DRIVER sagas due now.
	WakeWaiting(ctx context.Context) error
}

// OfferRepository persists offers. Responses are ATOMIC compare-and-set on
// status='PENDING' (ADR-002): accept, reject, timeout and cancel race on the
// same row and exactly one wins.
type OfferRepository interface {
	// Create inserts a PENDING offer expiring after timeout (ErrDuplicatePendingOffer).
	Create(ctx context.Context, id, rideID, driverID string, timeout time.Duration) error
	Get(ctx context.Context, id string) (domain.Offer, error) // ErrOfferNotFound
	// RespondPending sets the driver's PENDING offer for the ride to `to`;
	// notExpired additionally requires expires_at > now(). applied=false if nothing matched.
	RespondPending(ctx context.Context, rideID, driverID, to string, notExpired bool) (domain.Offer, bool, error)
	// ExpireIfDue sets a PENDING offer past its deadline to EXPIRED.
	ExpireIfDue(ctx context.Context, offerID string) (domain.Offer, bool, error)
	CancelPending(ctx context.Context, offerID string) (bool, error)
	Latest(ctx context.Context, rideID, driverID string) (domain.Offer, error) // ErrOfferNotFound
	PendingForDriver(ctx context.Context, driverID string) (*domain.Offer, error)
	ListByRide(ctx context.Context, rideID string) ([]domain.Offer, error)
	DueExpired(ctx context.Context, limit int) ([]domain.Offer, error)
}

// ExclusionRepository stores drivers permanently excluded for a ride (AC-07).
type ExclusionRepository interface {
	Add(ctx context.Context, rideID, driverID, reason string) error
	List(ctx context.Context, rideID string) (map[string]string, error) // driverId → reason
}

// CandidateSource finds AVAILABLE drivers near a pickup, nearest first (Redis GEO).
type CandidateSource interface {
	Candidates(ctx context.Context, pickup geo.Point, radiusM float64, count int) ([]domain.Candidate, error)
}

// GeoIndex is the writable side of the driver projection (Redis GEO).
type GeoIndex interface {
	// Apply records status/location at version; false if the version is not newer.
	Apply(ctx context.Context, driverID, status string, p geo.Point, version int64) (bool, error)
	Size(ctx context.Context) (int64, error)
}

// DriverSnapshot is a driver as listed by driver-service (for rebuilds).
type DriverSnapshot struct {
	ID       string
	Status   string
	Location geo.Point
	Version  int64
}

// DriverDirectory lists all drivers from driver-service (synchronous, rebuild only).
type DriverDirectory interface {
	ListDrivers(ctx context.Context) ([]DriverSnapshot, error)
}

// Publisher writes messages to the transactional outbox.
type Publisher interface {
	Publish(ctx context.Context, topic, key string, env messaging.Envelope) (messaging.Envelope, error)
}

// Inbox records processed message ids; Claim returns false for a duplicate.
type Inbox interface {
	Claim(ctx context.Context, eventID string) (bool, error)
}
