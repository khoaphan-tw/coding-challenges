package usecase

import (
	"context"
	"errors"
	"log/slog"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"

	"ridehail/pkg/geo"
	"ridehail/pkg/messaging"
	"ridehail/pkg/obs"
	"ridehail/services/driver/internal/domain"
)

// Drivers implements driver-service's use cases.
type Drivers struct {
	Tx        TxManager
	Drivers   DriverRepository
	Publisher Publisher
	Inbox     Inbox

	conflicts metric.Int64Counter
}

// Init sets up metrics.
func (u *Drivers) Init() *Drivers {
	u.conflicts, _ = otel.Meter("driver-service").Int64Counter("driver_reservation_conflicts_total")
	return u
}

// DriverEvent is the payload of DriverStatusChanged and DriverLocationUpdated.
// Version lets consumers drop out-of-order or duplicate events.
type DriverEvent struct {
	Status         domain.Status `json:"status"`
	Latitude       float64       `json:"latitude"`
	Longitude      float64       `json:"longitude"`
	Version        int64         `json:"version"`
	CurrentRideID  *string       `json:"currentRideId,omitempty"`
	PreviousStatus domain.Status `json:"previousStatus,omitempty"`
}

func (u *Drivers) emit(ctx context.Context, typ string, d domain.Driver, prev domain.Status) error {
	ride := ""
	if d.CurrentRideID != nil {
		ride = *d.CurrentRideID
	}
	_, err := u.Publisher.Publish(ctx, messaging.TopicDriverEvents, d.ID, messaging.New(typ, ride, d.ID, "",
		DriverEvent{Status: d.Status, Latitude: d.Latitude, Longitude: d.Longitude, Version: d.Version,
			CurrentRideID: d.CurrentRideID, PreviousStatus: prev}))
	return err
}

// change runs one atomic compare-and-set. If it was not applied, the pure
// domain rule classifies the current row: a duplicate (no-op, nil error) or a
// rejection (the rule's error). Must run inside a transaction.
func (u *Drivers) change(ctx context.Context, id string,
	cas func() (domain.Driver, bool, error),
	rule func(domain.Driver) (domain.Driver, bool, error),
) (domain.Driver, bool, error) {
	prev, err := u.Drivers.Get(ctx, id)
	if err != nil {
		return prev, false, err
	}
	d, applied, err := cas()
	if err != nil {
		return d, false, err
	}
	if applied {
		if err := u.emit(ctx, messaging.DriverStatusChanged, d, prev.Status); err != nil {
			return d, false, err
		}
		slog.InfoContext(obs.With(ctx, obs.DriverID, d.ID), "driver status changed",
			"from", prev.Status, "to", d.Status, "current_ride_id", deref(d.CurrentRideID))
		return d, true, nil
	}
	cur, err := u.Drivers.Get(ctx, id) // re-read after the lost compare-and-set
	if err != nil {
		return cur, false, err
	}
	_, changed, ruleErr := rule(cur)
	if ruleErr != nil {
		return cur, false, ruleErr
	}
	if changed { // the row moved between our statements: treat as a lost race
		return cur, false, domain.ErrNotAvailable
	}
	return cur, false, nil
}

// SetAvailability handles OFFLINE ⇄ AVAILABLE (AC-02).
func (u *Drivers) SetAvailability(ctx context.Context, id string, target domain.Status) (domain.Driver, error) {
	if target != domain.Available && target != domain.Offline {
		return domain.Driver{}, ErrInvalidStatus
	}
	from := domain.Offline
	if target == domain.Offline {
		from = domain.Available
	}
	var out domain.Driver
	err := u.Tx.WithinTx(ctx, func(ctx context.Context) error {
		d, _, err := u.change(ctx, id,
			func() (domain.Driver, bool, error) { return u.Drivers.SetStatusIf(ctx, id, from, target) },
			func(d domain.Driver) (domain.Driver, bool, error) { return domain.SetAvailability(d, target) })
		out = d
		if errors.Is(err, domain.ErrInvalidTransition) || errors.Is(err, domain.ErrNotAvailable) {
			slog.InfoContext(ctx, "availability change rejected", "outcome", "invalid_transition", "status", d.Status, "target", target)
			return domain.ErrInvalidTransition
		}
		return err
	})
	return out, err
}

// UpdateLocation stores the latest location (any status) and emits an event (AC-03).
func (u *Drivers) UpdateLocation(ctx context.Context, id string, p geo.Point) (domain.Driver, error) {
	if err := p.Validate(); err != nil {
		return domain.Driver{}, err
	}
	var out domain.Driver
	err := u.Tx.WithinTx(ctx, func(ctx context.Context) error {
		d, err := u.Drivers.UpdateLocation(ctx, id, p)
		if err != nil {
			return err
		}
		out = d
		slog.DebugContext(ctx, "driver location updated", "latitude", p.Latitude, "longitude", p.Longitude)
		return u.emit(ctx, messaging.DriverLocationUpdated, d, "")
	})
	return out, err
}

// Get returns one driver.
func (u *Drivers) Get(ctx context.Context, id string) (domain.Driver, error) {
	return u.Drivers.Get(ctx, id)
}

// List returns drivers, optionally filtered by status (GEO index rebuild).
func (u *Drivers) List(ctx context.Context, status string) ([]domain.Driver, error) {
	return u.Drivers.List(ctx, status)
}

// Upsert creates or resets a driver (development/test fixture; registration is out of scope).
func (u *Drivers) Upsert(ctx context.Context, id string, status domain.Status, p geo.Point) (domain.Driver, error) {
	if status != domain.Available && status != domain.Offline {
		return domain.Driver{}, ErrInvalidStatus
	}
	if err := p.Validate(); err != nil {
		return domain.Driver{}, err
	}
	var out domain.Driver
	err := u.Tx.WithinTx(ctx, func(ctx context.Context) error {
		d, applied, err := u.Drivers.Upsert(ctx, id, status, p)
		if err != nil {
			return err
		}
		if !applied {
			return domain.ErrInvalidTransition // OFFERED/BUSY drivers cannot be reset
		}
		out = d
		return u.emit(ctx, messaging.DriverStatusChanged, d, "")
	})
	return out, err
}

// Command is a saga command from dispatch-service.
type Command struct {
	EventID  string
	Type     string // ReserveDriver, MarkDriverBusy, ReleaseDriver
	RideID   string
	DriverID string
	OfferID  string
}

// HandleCommand applies a saga command and replies to dispatch-service in the
// same transaction (inbox → compare-and-set → events → reply).
func (u *Drivers) HandleCommand(ctx context.Context, c Command) error {
	return u.Tx.WithinTx(ctx, func(ctx context.Context) error {
		fresh, err := u.Inbox.Claim(ctx, c.EventID)
		if err != nil {
			return err
		}
		if !fresh {
			slog.InfoContext(ctx, "duplicate message skipped", "outcome", "duplicate_skipped")
			return nil
		}

		var d domain.Driver
		var changed bool
		switch c.Type {
		case messaging.CmdReserveDriver:
			d, changed, err = u.change(ctx, c.DriverID,
				func() (domain.Driver, bool, error) { return u.Drivers.ReserveIfAvailable(ctx, c.DriverID, c.RideID) },
				func(d domain.Driver) (domain.Driver, bool, error) { return domain.Reserve(d, c.RideID) })
		case messaging.CmdMarkDriverBusy:
			d, changed, err = u.change(ctx, c.DriverID,
				func() (domain.Driver, bool, error) { return u.Drivers.MarkBusyIfOffered(ctx, c.DriverID, c.RideID) },
				func(d domain.Driver) (domain.Driver, bool, error) { return domain.MarkBusy(d, c.RideID) })
		case messaging.CmdReleaseDriver:
			d, changed, err = u.change(ctx, c.DriverID,
				func() (domain.Driver, bool, error) { return u.Drivers.ReleaseIfHeld(ctx, c.DriverID, c.RideID) },
				func(d domain.Driver) (domain.Driver, bool, error) { return domain.Release(d, c.RideID) })
		default:
			slog.WarnContext(ctx, "unknown command ignored")
			return nil
		}

		result := messaging.CommandResult{Command: c.Type, CommandEventID: c.EventID}
		var reply string
		switch {
		case err == nil:
			reply = messaging.ReplyCommandSucceeded
			if c.Type == messaging.CmdReserveDriver {
				reply = messaging.ReplyDriverReserved
			}
			if !changed {
				slog.InfoContext(ctx, "command was a no-op (duplicate or stale)", "outcome", "duplicate", "status", d.Status)
			}
		case errors.Is(err, domain.ErrNotAvailable), errors.Is(err, domain.ErrStale), errors.Is(err, ErrDriverNotFound):
			reply = messaging.ReplyCommandRejected
			result.Reason = "DRIVER_NOT_AVAILABLE"
			if errors.Is(err, ErrDriverNotFound) {
				result.Reason = "DRIVER_NOT_FOUND"
			}
			if c.Type == messaging.CmdReserveDriver {
				reply = messaging.ReplyDriverReservationFailed
				if u.conflicts != nil {
					u.conflicts.Add(ctx, 1)
				}
				slog.InfoContext(ctx, "driver reservation failed: driver is not available",
					"outcome", "reservation_conflict", "status", d.Status, "current_ride_id", deref(d.CurrentRideID))
			} else {
				slog.InfoContext(ctx, "command rejected", "outcome", "stale_command", "status", d.Status)
			}
		default:
			return err
		}
		_, err = u.Publisher.Publish(ctx, messaging.TopicDispatchReplies, c.RideID,
			messaging.New(reply, c.RideID, c.DriverID, c.OfferID, result))
		return err
	})
}

// deref returns *p or "" (pointers print as addresses in text logs).
func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
