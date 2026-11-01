package usecase

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"ridehail/pkg/geo"
	"ridehail/pkg/messaging"
	"ridehail/pkg/obs"
	"ridehail/services/dispatch/internal/domain"
)

// Orchestrator runs dispatch sagas (ADR-003): it reacts to ride events and
// participant replies, matches drivers, and sends commands through the outbox.
type Orchestrator struct {
	Tx         TxManager
	Sagas      SagaRepository
	Offers     OfferRepository
	Exclusions ExclusionRepository
	Candidates CandidateSource
	Publisher  Publisher
	Inbox      Inbox

	RadiusM       float64       // default 2000
	OfferTimeout  time.Duration // default 30s
	RetryInterval time.Duration // WAITING_FOR_DRIVER retry, default 10s

	offers       metric.Int64Counter
	matchLatency metric.Float64Histogram
}

// Init sets defaults and metrics.
func (o *Orchestrator) Init() *Orchestrator {
	if o.RadiusM == 0 {
		o.RadiusM = domain.DefaultRadiusM
	}
	if o.OfferTimeout == 0 {
		o.OfferTimeout = 30 * time.Second
	}
	if o.RetryInterval == 0 {
		o.RetryInterval = 10 * time.Second
	}
	m := otel.Meter("dispatch-service")
	o.offers, _ = m.Int64Counter("offers_total", metric.WithDescription("Offers by outcome"))
	o.matchLatency, _ = m.Float64Histogram("dispatch_match_duration_seconds",
		metric.WithDescription("Ride requested to offer created"), metric.WithUnit("s"))
	return o
}

func (o *Orchestrator) countOffer(ctx context.Context, outcome string) {
	o.offers.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}

// ---- inputs from the messaging adapter ----

// RideEvent is a ride.events.v1 message.
type RideEvent struct {
	EventID string
	Type    string // RideRequested, RideCancelled, RideCompleted, …
	RideID  string
	Pickup  geo.Point // RideRequested only
}

// Reply is a dispatch.replies.v1 message from a participant.
type Reply struct {
	EventID  string
	Type     string // CommandSucceeded, CommandRejected, DriverReserved, DriverReservationFailed
	RideID   string
	DriverID string
	OfferID  string
	Command  string // the command this replies to
	Reason   string
}

// ---- helpers ----

// inTx runs fn in a transaction that first claims eventID in the inbox.
// A duplicate message is skipped (logged) without calling fn.
func (o *Orchestrator) inTx(ctx context.Context, eventID string, fn func(ctx context.Context) error) error {
	return o.Tx.WithinTx(ctx, func(ctx context.Context) error {
		fresh, err := o.Inbox.Claim(ctx, eventID)
		if err != nil {
			return err
		}
		if !fresh {
			slog.InfoContext(ctx, "duplicate message skipped", "outcome", "duplicate_skipped")
			return nil
		}
		return fn(ctx)
	})
}

// step writes an audit row and one log line for a saga step.
func (o *Orchestrator) step(ctx context.Context, s domain.Saga, name, detail, driverID, offerID string) error {
	if err := o.Sagas.AddStep(ctx, domain.Step{RideID: s.RideID, Name: name, Detail: detail, DriverID: driverID,
		OfferID: offerID, EventID: obs.Field(ctx, obs.EventID)}); err != nil {
		return err
	}
	slog.InfoContext(obs.With(ctx, obs.RideID, s.RideID, obs.DriverID, driverID, obs.OfferID, offerID, obs.SagaStep, name),
		detail, "saga_status", s.Status, "round", s.Round)
	return nil
}

// send enqueues a saga command in the current transaction (outbox).
func (o *Orchestrator) send(ctx context.Context, typ, rideID, driverID, offerID string) error {
	topic, key := messaging.TopicRideCommands, rideID
	switch typ {
	case messaging.CmdReserveDriver, messaging.CmdReleaseDriver, messaging.CmdMarkDriverBusy:
		topic, key = messaging.TopicDriverCommands, driverID
	}
	_, err := o.Publisher.Publish(ctx, topic, key, messaging.New(typ, rideID, driverID, offerID, nil))
	return err
}

func (o *Orchestrator) offerDriver(ctx context.Context, offerID *string) (string, error) {
	if offerID == nil {
		return "", nil
	}
	off, err := o.Offers.Get(ctx, *offerID)
	if errors.Is(err, ErrOfferNotFound) {
		return "", nil
	}
	return off.DriverID, err
}

// lockSaga loads and locks the saga; ok=false when the ride has no saga.
func (o *Orchestrator) lockSaga(ctx context.Context, rideID string) (domain.Saga, bool, error) {
	s, err := o.Sagas.GetForUpdate(ctx, rideID)
	if errors.Is(err, ErrSagaNotFound) {
		return s, false, nil
	}
	return s, err == nil, err
}

// ---- ride events ----

// HandleRideEvent reacts to ride.events.v1.
func (o *Orchestrator) HandleRideEvent(ctx context.Context, ev RideEvent) error {
	switch ev.Type {
	case messaging.RideRequested:
		return o.inTx(ctx, ev.EventID, func(ctx context.Context) error {
			// ctx is already in the ride's trace (propagated through the event).
			created, err := o.Sagas.Create(ctx, domain.Saga{RideID: ev.RideID, Status: domain.Started,
				Pickup: ev.Pickup, RadiusM: o.RadiusM, TraceParent: obs.TraceParent(ctx)})
			if err != nil {
				return err
			}
			if !created { // same ride requested again under another event id
				slog.InfoContext(ctx, "saga already exists, RideRequested ignored", "outcome", "duplicate")
				return nil
			}
			s := domain.Saga{RideID: ev.RideID, Status: domain.Started}
			if err := o.step(ctx, s, domain.Started, "saga started, asking ride-service to start matching", "", ""); err != nil {
				return err
			}
			return o.send(ctx, messaging.CmdStartMatching, ev.RideID, "", "")
		})

	case messaging.RideCancelled:
		return o.inTx(ctx, ev.EventID, func(ctx context.Context) error {
			s, ok, err := o.lockSaga(ctx, ev.RideID)
			if !ok || err != nil {
				return err
			}
			return o.cancel(ctx, s, "ride cancelled by passenger")
		})

	case messaging.RideCompleted:
		return o.inTx(ctx, ev.EventID, func(ctx context.Context) error {
			s, ok, err := o.lockSaga(ctx, ev.RideID)
			if !ok || err != nil || s.Terminal() {
				return err
			}
			driverID, err := o.offerDriver(ctx, s.CurrentOfferID)
			if err != nil {
				return err
			}
			s.Status = domain.Completed
			if err := o.Sagas.Save(ctx, s); err != nil {
				return err
			}
			if err := o.step(ctx, s, domain.Completed, "ride completed, releasing driver", driverID, domain.Val(s.CurrentOfferID)); err != nil {
				return err
			}
			return o.send(ctx, messaging.CmdReleaseDriver, s.RideID, driverID, domain.Val(s.CurrentOfferID))
		})
	}
	return nil // RideStatusChanged etc. are informational
}

// cancel compensates whatever the saga holds: a pending offer is cancelled
// and any reserved/offered/busy driver is released. Idempotent.
func (o *Orchestrator) cancel(ctx context.Context, s domain.Saga, why string) error {
	if s.Status == domain.Cancelled {
		return nil
	}
	if s.Status == domain.Completed {
		slog.InfoContext(ctx, "cancel ignored: saga already completed", "outcome", "invalid_transition")
		return nil
	}
	var driverID, offerID string
	switch s.Status {
	case domain.Reserving:
		driverID, offerID = domain.Val(s.ReservingDriverID), domain.Val(s.ReservingOfferID)
	case domain.Offered, domain.Accepted:
		offerID = domain.Val(s.CurrentOfferID)
		d, err := o.offerDriver(ctx, s.CurrentOfferID)
		if err != nil {
			return err
		}
		driverID = d
		cancelled, err := o.Offers.CancelPending(ctx, offerID)
		if err != nil {
			return err
		}
		if cancelled {
			o.countOffer(ctx, "cancelled")
		}
	}
	s.Status = domain.Cancelled
	s.ReservingDriverID, s.ReservingOfferID, s.NextRetryAt = nil, nil, nil
	if err := o.Sagas.Save(ctx, s); err != nil {
		return err
	}
	if err := o.step(ctx, s, domain.Cancelled, why, driverID, offerID); err != nil {
		return err
	}
	if driverID != "" {
		return o.send(ctx, messaging.CmdReleaseDriver, s.RideID, driverID, offerID)
	}
	return nil
}

// ---- participant replies ----

// HandleReply reacts to dispatch.replies.v1.
func (o *Orchestrator) HandleReply(ctx context.Context, r Reply) error {
	needMatch := false
	err := o.inTx(ctx, r.EventID, func(ctx context.Context) error {
		s, ok, err := o.lockSaga(ctx, r.RideID)
		if !ok || err != nil {
			return err
		}
		switch r.Type {
		case messaging.ReplyCommandSucceeded:
			if r.Command == messaging.CmdStartMatching && s.Status == domain.Started {
				s.Status = domain.Matching
				if err := o.Sagas.Save(ctx, s); err != nil {
					return err
				}
				needMatch = true
				return o.step(ctx, s, domain.Matching, "ride is MATCHING, searching for drivers", "", "")
			}
			slog.DebugContext(ctx, "command confirmed", "command", r.Command)
			return nil

		case messaging.ReplyCommandRejected:
			return o.onRejected(ctx, s, r)

		case messaging.ReplyDriverReserved:
			return o.onReserved(ctx, s, r)

		case messaging.ReplyDriverReservationFailed:
			if s.Status != domain.Reserving || domain.Val(s.ReservingOfferID) != r.OfferID {
				return nil // stale reply
			}
			s.Status = domain.Matching
			s.RoundSkips = append(s.RoundSkips, r.DriverID)
			s.ReservingDriverID, s.ReservingOfferID = nil, nil
			if err := o.Sagas.Save(ctx, s); err != nil {
				return err
			}
			needMatch = true
			return o.step(ctx, s, domain.Matching, "driver was taken by another ride, trying the next candidate", r.DriverID, r.OfferID)
		}
		return nil
	})
	if err == nil && needMatch {
		o.Match(ctx, r.RideID)
	}
	return err
}

// onReserved: the driver is now OFFERED for this ride → create the offer and
// assign it on the ride. A reservation the saga no longer wants is released.
func (o *Orchestrator) onReserved(ctx context.Context, s domain.Saga, r Reply) error {
	if (s.Status == domain.Offered || s.Status == domain.Accepted) && domain.Val(s.CurrentOfferID) == r.OfferID {
		return nil // duplicate reply for the offer we already created
	}
	if s.Status != domain.Reserving || domain.Val(s.ReservingOfferID) != r.OfferID {
		if err := o.step(ctx, s, "COMPENSATE", "unwanted reservation, releasing driver", r.DriverID, r.OfferID); err != nil {
			return err
		}
		return o.send(ctx, messaging.CmdReleaseDriver, s.RideID, r.DriverID, r.OfferID)
	}
	if err := o.Offers.Create(ctx, r.OfferID, s.RideID, r.DriverID, o.OfferTimeout); err != nil {
		if errors.Is(err, ErrDuplicatePendingOffer) { // defence in depth (ADR-002)
			return fmt.Errorf("driver %s: %w", r.DriverID, err)
		}
		return err
	}
	s.Status = domain.Offered
	s.CurrentOfferID = domain.Ptr(r.OfferID)
	s.ReservingDriverID, s.ReservingOfferID = nil, nil
	s.RoundSkips = []string{}
	if err := o.Sagas.Save(ctx, s); err != nil {
		return err
	}
	o.countOffer(ctx, "created")
	o.matchLatency.Record(ctx, time.Since(s.CreatedAt).Seconds())
	if err := o.step(ctx, s, domain.Offered, fmt.Sprintf("offer created, driver has %s to respond", o.OfferTimeout),
		r.DriverID, r.OfferID); err != nil {
		return err
	}
	return o.send(ctx, messaging.CmdAssignDriver, s.RideID, r.DriverID, r.OfferID)
}

// onRejected handles a participant refusing a command. The ride refusing
// AssignDriver/MarkAccepted/StartMatching means it was cancelled meanwhile.
func (o *Orchestrator) onRejected(ctx context.Context, s domain.Saga, r Reply) error {
	switch r.Command {
	case messaging.CmdStartMatching, messaging.CmdAssignDriver, messaging.CmdMarkAccepted:
		if err := o.cancel(ctx, s, "ride refused "+r.Command+" ("+r.Reason+"), compensating"); err != nil {
			return err
		}
		if s.Status == domain.Cancelled && r.DriverID != "" { // already cancelled: make sure this driver is free
			return o.send(ctx, messaging.CmdReleaseDriver, s.RideID, r.DriverID, r.OfferID)
		}
		return nil
	default:
		slog.InfoContext(ctx, "participant rejected command", "command", r.Command, "reason", r.Reason, "outcome", "stale_command")
		return nil
	}
}

// ---- matching ----

// Match runs one matching attempt for a saga in MATCHING: pick the nearest
// eligible driver and ask driver-service to reserve it, or wait.
func (o *Orchestrator) Match(ctx context.Context, rideID string) {
	ctx = obs.With(ctx, obs.RideID, rideID)
	if err := o.match(ctx, rideID); err != nil && ctx.Err() == nil {
		slog.ErrorContext(ctx, "matching failed, the retry worker will try again", "error", err)
	}
}

func (o *Orchestrator) match(ctx context.Context, rideID string) error {
	s, err := o.Sagas.Get(ctx, rideID)
	if err != nil || s.Status != domain.Matching {
		return nil
	}
	ctx, span := obs.ContinueTrace(ctx, s.TraceParent, "match driver", obs.RideAttr(rideID))
	defer span.End()
	excluded, err := o.Exclusions.List(ctx, rideID)
	if err != nil {
		return err
	}
	skip := make(map[string]bool, len(excluded)+len(s.RoundSkips))
	for id := range excluded {
		skip[id] = true
	}
	for _, id := range s.RoundSkips {
		skip[id] = true
	}

	// Ask for enough candidates to survive the exclusions. If the list is
	// full, the cut may split a tie, so widen until it is not full.
	var best domain.Ranked
	found := false
	for count := 20 + len(skip); ; count *= 2 {
		cands, err := o.Candidates.Candidates(ctx, s.Pickup, s.RadiusM, count)
		if err != nil {
			slog.ErrorContext(ctx, "geo index unavailable, treating as no candidates", "error", err)
			break
		}
		best, found = domain.SelectNearest(s.Pickup, cands, s.RadiusM, skip)
		if len(cands) < count || count > 5000 {
			break
		}
	}

	return o.Tx.WithinTx(ctx, func(ctx context.Context) error {
		cur, err := o.Sagas.GetForUpdate(ctx, rideID)
		if err != nil {
			return err
		}
		if cur.Status != domain.Matching || cur.Version != s.Version {
			return nil // someone else moved the saga on
		}
		if !found {
			cur.Status = domain.WaitingForDriver
			next := time.Now().Add(o.RetryInterval)
			cur.NextRetryAt = &next
			cur.RoundSkips = []string{}
			if err := o.Sagas.Save(ctx, cur); err != nil {
				return err
			}
			return o.step(ctx, cur, domain.WaitingForDriver,
				fmt.Sprintf("no eligible driver within %.0f m (excluded: %d), ride stays MATCHING, retry in %s",
					cur.RadiusM, len(excluded), o.RetryInterval), "", "")
		}
		offerID := messaging.NewID()
		cur.Status = domain.Reserving
		cur.ReservingDriverID = domain.Ptr(best.DriverID)
		cur.ReservingOfferID = domain.Ptr(offerID)
		cur.NextRetryAt = nil
		if err := o.Sagas.Save(ctx, cur); err != nil {
			return err
		}
		if err := o.step(ctx, cur, domain.Reserving, fmt.Sprintf("nearest eligible driver selected at %.0f m, reserving",
			best.DistanceM), best.DriverID, offerID); err != nil {
			return err
		}
		return o.send(ctx, messaging.CmdReserveDriver, rideID, best.DriverID, offerID)
	})
}
