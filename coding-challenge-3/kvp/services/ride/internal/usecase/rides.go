package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/trace"

	"ridehail/pkg/geo"
	"ridehail/pkg/messaging"
	"ridehail/pkg/obs"
	"ridehail/services/ride/internal/domain"
)

// Rides implements ride-service's use cases.
type Rides struct {
	Tx          TxManager
	Rides       RideRepository
	Passengers  PassengerRepository
	Idempotency IdempotencyStore
	Publisher   Publisher
	Inbox       Inbox
	Now         func() time.Time
}

func (u *Rides) now() time.Time {
	if u.Now != nil {
		return u.Now()
	}
	return time.Now().UTC()
}

// CreateRideInput is the request to create a ride (AC-01).
type CreateRideInput struct {
	IdempotencyKey string
	PassengerID    string
	Pickup         geo.Point
	Destination    geo.Point
}

// CreateRideOutput is the created (or replayed) ride.
type CreateRideOutput struct {
	Ride     domain.Ride
	Replayed bool // same Idempotency-Key and same request: original result returned
}

// RequestHash fingerprints the normalised request, so formatting differences
// do not count as a different request for the same Idempotency-Key.
func RequestHash(in CreateRideInput) string {
	b, _ := json.Marshal(struct {
		P    string
		A, B geo.Point
	}{in.PassengerID, in.Pickup, in.Destination})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// CreateRide stores a ride in REQUESTED, its Idempotency-Key and a
// RideRequested event in one transaction (AC-01, AC-08, AC-10).
func (u *Rides) CreateRide(ctx context.Context, in CreateRideInput) (CreateRideOutput, error) {
	var out CreateRideOutput
	err := u.Tx.WithinTx(ctx, func(ctx context.Context) error {
		rideID := messaging.NewID()
		prior, err := u.Idempotency.Reserve(ctx, in.IdempotencyKey, RequestHash(in), rideID)
		if err != nil {
			return err
		}
		if prior != nil {
			if prior.RequestHash != RequestHash(in) {
				return ErrKeyReused
			}
			obs.Annotate(ctx, obs.RideID, prior.Ride.ID)
			out = CreateRideOutput{Ride: prior.Ride, Replayed: true}
			return nil
		}

		r, err := domain.NewRide(rideID, in.PassengerID, in.Pickup, in.Destination, u.now())
		if err != nil {
			return err
		}
		ok, err := u.Passengers.Exists(ctx, in.PassengerID)
		if err != nil {
			return err
		}
		if !ok {
			return ErrUnknownPassenger
		}
		// This request's trace becomes the ride's trace for its whole journey.
		r.TraceParent = obs.TraceParent(ctx)
		trace.SpanFromContext(ctx).SetAttributes(obs.RideAttr(r.ID))
		obs.Annotate(ctx, obs.RideID, r.ID)

		if r, err = u.Rides.Insert(ctx, r); err != nil {
			return err
		}
		if err := u.Rides.AddTransition(ctx, domain.Transition{RideID: r.ID, To: domain.Requested,
			Action: "Create", Cause: "http:idempotency-key=" + in.IdempotencyKey}); err != nil {
			return err
		}
		if err := u.Idempotency.SaveResult(ctx, in.IdempotencyKey, r); err != nil {
			return err
		}
		ctx = obs.With(ctx, obs.RideID, r.ID)
		ev, err := u.Publisher.Publish(ctx, messaging.TopicRideEvents, r.ID,
			messaging.New(messaging.RideRequested, r.ID, "", "", RideRequestedPayload{
				PassengerID: r.PassengerID, Pickup: r.Pickup, Destination: r.Destination}))
		if err != nil {
			return err
		}
		slog.InfoContext(obs.With(ctx, obs.EventID, ev.EventID), "ride created",
			"status", r.Status, "passenger_id", r.PassengerID, "idempotency_key", in.IdempotencyKey)
		out = CreateRideOutput{Ride: r}
		return nil
	})
	return out, err
}

// RideRequestedPayload is the payload of RideRequested.
type RideRequestedPayload struct {
	PassengerID string    `json:"passengerId"`
	Pickup      geo.Point `json:"pickup"`
	Destination geo.Point `json:"destination"`
}

// RideStatusPayload is the payload of RideStatusChanged / RideCancelled / RideCompleted.
type RideStatusPayload struct {
	From     domain.Status `json:"from"`
	To       domain.Status `json:"to"`
	Action   string        `json:"action"`
	DriverID string        `json:"driverId,omitempty"`
}

// GetRide returns one ride.
func (u *Rides) GetRide(ctx context.Context, id string) (domain.Ride, error) {
	return u.Rides.Get(ctx, id)
}

// History returns every status change of a ride, oldest first.
func (u *Rides) History(ctx context.Context, id string) ([]domain.Transition, error) {
	if _, err := u.Rides.Get(ctx, id); err != nil {
		return nil, err
	}
	return u.Rides.History(ctx, id)
}

// Cancel and Complete are the passenger/driver lifecycle actions (AC-05, AC-06).
func (u *Rides) Cancel(ctx context.Context, rideID, cause string) (domain.Ride, error) {
	return u.lifecycle(ctx, rideID, domain.Cancel, cause)
}

// Complete finishes an accepted ride.
func (u *Rides) Complete(ctx context.Context, rideID, cause string) (domain.Ride, error) {
	return u.lifecycle(ctx, rideID, domain.Complete, cause)
}

func (u *Rides) lifecycle(ctx context.Context, rideID string, kind domain.ActionKind, cause string) (domain.Ride, error) {
	// Continue the ride's trace; the HTTP request stays linked.
	if r, err := u.Rides.Get(ctx, rideID); err == nil {
		var span trace.Span
		ctx, span = obs.ContinueTrace(ctx, r.TraceParent, "ride."+string(kind), obs.RideAttr(rideID))
		defer span.End()
	}
	var out domain.Ride
	err := u.Tx.WithinTx(ctx, func(ctx context.Context) error {
		r, changed, err := u.apply(ctx, rideID, domain.Action{Kind: kind}, cause)
		out = r
		switch {
		case errors.Is(err, domain.ErrInvalidTransition):
			slog.InfoContext(ctx, "transition rejected", "outcome", "invalid_transition", "action", kind, "status", r.Status)
		case err == nil && !changed:
			slog.InfoContext(ctx, "duplicate transition ignored", "outcome", "duplicate", "action", kind, "status", r.Status)
		}
		return err
	})
	return out, err
}

// apply locks the ride, runs the state machine and persists the result with a
// history row and events. Must run inside a transaction.
func (u *Rides) apply(ctx context.Context, rideID string, a domain.Action, cause string) (domain.Ride, bool, error) {
	cur, err := u.Rides.GetForUpdate(ctx, rideID)
	if err != nil {
		return cur, false, err
	}
	next, changed, err := domain.Apply(cur, a)
	if err != nil || !changed {
		return cur, false, err
	}
	if next, err = u.Rides.Update(ctx, next); err != nil {
		return cur, false, err
	}
	driverID := a.DriverID
	if driverID == "" && cur.AssignedDriverID != nil {
		driverID = *cur.AssignedDriverID
	}
	offerID := a.OfferID
	if offerID == "" && cur.CurrentOfferID != nil {
		offerID = *cur.CurrentOfferID
	}
	from := cur.Status
	if err := u.Rides.AddTransition(ctx, domain.Transition{RideID: next.ID, From: &from, To: next.Status,
		Action: string(a.Kind), DriverID: driverID, OfferID: offerID, Cause: cause}); err != nil {
		return cur, false, err
	}
	payload := RideStatusPayload{From: cur.Status, To: next.Status, Action: string(a.Kind), DriverID: driverID}
	types := []string{messaging.RideStatusChanged}
	switch next.Status {
	case domain.Cancelled:
		types = append(types, messaging.RideCancelled)
	case domain.Completed:
		types = append(types, messaging.RideCompleted)
	}
	for _, typ := range types {
		if _, err := u.Publisher.Publish(ctx, messaging.TopicRideEvents, next.ID,
			messaging.New(typ, next.ID, driverID, "", payload)); err != nil {
			return cur, false, err
		}
	}
	slog.InfoContext(obs.With(ctx, obs.RideID, next.ID, obs.DriverID, driverID), "ride transitioned",
		"from", cur.Status, "to", next.Status, "action", a.Kind, "cause", cause)
	return next, true, nil
}

// Command is a saga command from dispatch-service (already decoded by the messaging adapter).
type Command struct {
	EventID  string
	Type     string // StartMatching, AssignDriver, UnassignDriver, MarkAccepted
	RideID   string
	DriverID string
	OfferID  string
}

var commandKinds = map[string]domain.ActionKind{
	messaging.CmdStartMatching:  domain.StartMatching,
	messaging.CmdAssignDriver:   domain.AssignDriver,
	messaging.CmdUnassignDriver: domain.Unassign,
	messaging.CmdMarkAccepted:   domain.MarkAccepted,
}

// HandleCommand applies a saga command and replies to the orchestrator.
// Inbox claim, state change, history, events and reply share one transaction,
// so a redelivered command changes nothing and is not re-replied (AC-08).
func (u *Rides) HandleCommand(ctx context.Context, c Command) error {
	kind, ok := commandKinds[c.Type]
	if !ok {
		slog.WarnContext(ctx, "unknown command ignored")
		return nil
	}
	return u.Tx.WithinTx(ctx, func(ctx context.Context) error {
		fresh, err := u.Inbox.Claim(ctx, c.EventID)
		if err != nil {
			return err
		}
		if !fresh {
			slog.InfoContext(ctx, "duplicate message skipped", "outcome", "duplicate_skipped")
			return nil
		}
		r, changed, err := u.apply(ctx, c.RideID, domain.Action{Kind: kind, DriverID: c.DriverID, OfferID: c.OfferID},
			"event:"+c.EventID)

		result := messaging.CommandResult{Command: c.Type, CommandEventID: c.EventID}
		reply := messaging.ReplyCommandSucceeded
		switch {
		case errors.Is(err, domain.ErrInvalidTransition), errors.Is(err, ErrRideNotFound):
			reply = messaging.ReplyCommandRejected
			result.Reason = "INVALID_TRANSITION"
			if r.Status.Terminal() {
				result.Reason = "RIDE_TERMINAL"
			}
			if errors.Is(err, ErrRideNotFound) {
				result.Reason = "RIDE_NOT_FOUND"
			}
			slog.InfoContext(ctx, "command rejected", "outcome", "invalid_transition", "status", r.Status, "reason", result.Reason)
		case err != nil:
			return err
		case !changed:
			slog.InfoContext(ctx, "command was a no-op (duplicate or stale)", "outcome", "duplicate", "status", r.Status)
		}
		_, err = u.Publisher.Publish(ctx, messaging.TopicDispatchReplies, c.RideID,
			messaging.New(reply, c.RideID, c.DriverID, c.OfferID, result))
		return err
	})
}
