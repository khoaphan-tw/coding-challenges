package usecase

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"ridehail/pkg/messaging"
	"ridehail/pkg/obs"
	"ridehail/services/dispatch/internal/domain"
)

// OfferResult is the outcome of accept/reject.
type OfferResult struct {
	Offer    domain.Offer
	Replayed bool // a repeat of an earlier identical response (AC-08)
}

// Accept is the saga's pivot (AC-05). The compare-and-set on the offer decides
// the race between accept, reject, timeout and cancel: exactly one wins.
// Repeating an accept returns the same result.
func (o *Orchestrator) Accept(ctx context.Context, rideID, driverID string) (OfferResult, error) {
	var out OfferResult
	ctx = obs.With(ctx, obs.RideID, rideID, obs.DriverID, driverID)
	err := o.Tx.WithinTx(ctx, func(ctx context.Context) error {
		s, ok, err := o.lockSaga(ctx, rideID)
		if err != nil {
			return err
		}
		if !ok {
			return ErrOfferNotFound
		}
		ctx, span := obs.ContinueTrace(ctx, s.TraceParent, "offer.accept", obs.RideAttr(rideID))
		defer span.End()
		off, applied, err := o.Offers.RespondPending(ctx, rideID, driverID, domain.OfferAccepted, true)
		if err != nil {
			return err
		}
		if !applied {
			prev, err := o.Offers.Latest(ctx, rideID, driverID)
			if err != nil {
				return err
			}
			out.Offer = prev
			switch prev.Status {
			case domain.OfferAccepted:
				out.Replayed = true
				return o.step(ctx, s, "ACCEPT_DUPLICATE", "duplicate accept ignored, offer already accepted", driverID, prev.ID)
			case domain.OfferPending:
				return ErrOfferExpired // expired; the sweeper will rematch
			default:
				return ErrOfferNotCurrent
			}
		}
		out.Offer = off
		s.Status = domain.Accepted
		if err := o.Sagas.Save(ctx, s); err != nil {
			return err
		}
		o.countOffer(ctx, "accepted")
		if err := o.step(ctx, s, domain.Accepted, "driver accepted the offer (pivot): marking driver BUSY and ride DRIVER_ACCEPTED",
			driverID, off.ID); err != nil {
			return err
		}
		if err := o.send(ctx, messaging.CmdMarkDriverBusy, rideID, driverID, off.ID); err != nil {
			return err
		}
		return o.send(ctx, messaging.CmdMarkAccepted, rideID, driverID, off.ID)
	})
	return out, err
}

// Reject ends the offer, permanently excludes the driver for this ride and
// rematches (AC-05, AC-07). Repeating a reject returns the same result.
func (o *Orchestrator) Reject(ctx context.Context, rideID, driverID string) (OfferResult, error) {
	var out OfferResult
	rematch := false
	ctx = obs.With(ctx, obs.RideID, rideID, obs.DriverID, driverID)
	err := o.Tx.WithinTx(ctx, func(ctx context.Context) error {
		s, ok, err := o.lockSaga(ctx, rideID)
		if err != nil {
			return err
		}
		if !ok {
			return ErrOfferNotFound
		}
		ctx, span := obs.ContinueTrace(ctx, s.TraceParent, "offer.reject", obs.RideAttr(rideID))
		defer span.End()
		off, applied, err := o.Offers.RespondPending(ctx, rideID, driverID, domain.OfferRejected, false)
		if err != nil {
			return err
		}
		if !applied {
			prev, err := o.Offers.Latest(ctx, rideID, driverID)
			if err != nil {
				return err
			}
			out.Offer = prev
			if prev.Status == domain.OfferRejected {
				out.Replayed = true
				return nil
			}
			return ErrOfferNotCurrent
		}
		out.Offer = off
		rematch = true
		return o.endOffer(ctx, s, off, domain.OfferRejected)
	})
	if err == nil && rematch {
		o.Match(ctx, rideID)
	}
	return out, err
}

// endOffer is the shared path of rejection and timeout: exclude the driver,
// release them, unassign the ride and go back to MATCHING.
func (o *Orchestrator) endOffer(ctx context.Context, s domain.Saga, off domain.Offer, outcome string) error {
	if err := o.Exclusions.Add(ctx, s.RideID, off.DriverID, outcome); err != nil {
		return err
	}
	if s.Terminal() || s.Status == domain.Accepted {
		return nil
	}
	s.Status = domain.Matching
	s.CurrentOfferID = nil
	s.Round++
	s.RoundSkips = []string{}
	if err := o.Sagas.Save(ctx, s); err != nil {
		return err
	}
	detail := "driver rejected the offer: excluded for this ride, released, ride back to MATCHING"
	if outcome == domain.OfferExpired {
		detail = "offer timed out: driver excluded for this ride, released, ride back to MATCHING"
		o.countOffer(ctx, "expired")
	} else {
		o.countOffer(ctx, "rejected")
	}
	if err := o.step(ctx, s, domain.Matching, detail, off.DriverID, off.ID); err != nil {
		return err
	}
	if err := o.send(ctx, messaging.CmdReleaseDriver, s.RideID, off.DriverID, off.ID); err != nil {
		return err
	}
	return o.send(ctx, messaging.CmdUnassignDriver, s.RideID, off.DriverID, off.ID)
}

// PendingOffer returns the driver's open offer, if any.
func (o *Orchestrator) PendingOffer(ctx context.Context, driverID string) (*domain.Offer, error) {
	return o.Offers.PendingForDriver(ctx, driverID)
}

// Offer returns one offer by id (ErrOfferNotFound if unknown).
func (o *Orchestrator) Offer(ctx context.Context, id string) (domain.Offer, error) {
	return o.Offers.Get(ctx, id)
}

// View is the dispatch state of a ride (GET /rides/{id}/dispatch).
type View struct {
	Saga       domain.Saga
	Offers     []domain.Offer
	Exclusions map[string]string
	Steps      []domain.Step
}

// Describe returns the saga, its offers, exclusions and steps.
func (o *Orchestrator) Describe(ctx context.Context, rideID string) (View, error) {
	var v View
	var err error
	if v.Saga, err = o.Sagas.Get(ctx, rideID); err != nil {
		return v, err
	}
	if v.Offers, err = o.Offers.ListByRide(ctx, rideID); err != nil {
		return v, err
	}
	if v.Exclusions, err = o.Exclusions.List(ctx, rideID); err != nil {
		return v, err
	}
	v.Steps, err = o.Sagas.Steps(ctx, rideID)
	return v, err
}

// ---- workers ----

// SweepExpired expires every PENDING offer past its deadline (AC-05 timeout).
// A timeout follows exactly the rejection path.
func (o *Orchestrator) SweepExpired(ctx context.Context) {
	due, err := o.Offers.DueExpired(ctx, 100)
	if err != nil {
		logErr(ctx, "offer sweep failed", err)
		return
	}
	for _, d := range due {
		ctx := obs.With(ctx, obs.RideID, d.RideID, obs.OfferID, d.ID)
		expired := false
		err := o.Tx.WithinTx(ctx, func(ctx context.Context) error {
			// Lock order is always saga → offer (same as accept/reject); SKIP LOCKED
			// lets several dispatch replicas sweep without blocking each other.
			s, ok, err := o.Sagas.TryGetForUpdate(ctx, d.RideID)
			if err != nil || !ok {
				return err // busy elsewhere; next tick
			}
			ctx, span := obs.ContinueTrace(ctx, s.TraceParent, "offer.timeout", obs.RideAttr(d.RideID))
			defer span.End()
			off, applied, err := o.Offers.ExpireIfDue(ctx, d.ID)
			if err != nil || !applied {
				return err // accepted/rejected just before the deadline
			}
			expired = true
			return o.endOffer(obs.With(ctx, obs.DriverID, off.DriverID), s, off, domain.OfferExpired)
		})
		if err != nil {
			logErr(ctx, "offer expiry failed", err)
			continue
		}
		if expired {
			o.Match(ctx, d.RideID)
		}
	}
}

// RetryAndResume runs one pass of the retry/resume worker:
//   - WAITING_FOR_DRIVER past next_retry_at → MATCHING and match again
//   - MATCHING untouched for stuckAfter (crash after commit) → match again
//   - STARTED / RESERVING untouched for stuckAfter (lost reply) → re-send the
//     last command; participants are idempotent, so this is safe.
func (o *Orchestrator) RetryAndResume(ctx context.Context, stuckAfter time.Duration) {
	ids, err := o.Sagas.DueForRetry(ctx, stuckAfter, 100)
	if err != nil {
		logErr(ctx, "retry scan failed", err)
		return
	}
	for _, id := range ids {
		ctx := obs.With(ctx, obs.RideID, id)
		match := false
		err := o.Tx.WithinTx(ctx, func(ctx context.Context) error {
			s, ok, err := o.Sagas.TryGetForUpdate(ctx, id)
			if err != nil || !ok {
				return err
			}
			ctx, span := obs.ContinueTrace(ctx, s.TraceParent, "saga.retry", obs.RideAttr(id))
			defer span.End()
			switch s.Status {
			case domain.WaitingForDriver:
				if s.NextRetryAt != nil && s.NextRetryAt.After(time.Now()) {
					return nil
				}
				s.Status, s.NextRetryAt = domain.Matching, nil
				match = true
				if err := o.Sagas.Save(ctx, s); err != nil {
					return err
				}
				return o.step(ctx, s, domain.Matching, "retrying matching", "", "")
			case domain.Matching:
				match = true
				return nil
			case domain.Started:
				if err := o.Sagas.Save(ctx, s); err != nil { // touch updated_at
					return err
				}
				if err := o.step(ctx, s, "RESUME", "no reply to StartMatching, re-sending", "", ""); err != nil {
					return err
				}
				return o.send(ctx, messaging.CmdStartMatching, s.RideID, "", "")
			case domain.Reserving:
				if err := o.Sagas.Save(ctx, s); err != nil {
					return err
				}
				if err := o.step(ctx, s, "RESUME", "no reply to ReserveDriver, re-sending",
					domain.Val(s.ReservingDriverID), domain.Val(s.ReservingOfferID)); err != nil {
					return err
				}
				return o.send(ctx, messaging.CmdReserveDriver, s.RideID, domain.Val(s.ReservingDriverID), domain.Val(s.ReservingOfferID))
			}
			return nil
		})
		if err != nil {
			logErr(ctx, "saga retry failed", err)
			continue
		}
		if match {
			o.Match(ctx, id)
		}
	}
}

// WakeWaiting makes waiting sagas retry now (a driver became AVAILABLE).
func (o *Orchestrator) WakeWaiting(ctx context.Context) {
	if err := o.Sagas.WakeWaiting(ctx); err != nil {
		logErr(ctx, "wake waiting sagas failed", err)
	}
}

func logErr(ctx context.Context, msg string, err error) {
	if ctx.Err() == nil && !errors.Is(err, context.Canceled) {
		slog.ErrorContext(ctx, msg, "error", err)
	}
}
