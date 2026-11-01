// Package messaging is dispatch-service's driving adapter for Redpanda: it
// decodes envelopes from ride.events, dispatch.replies and driver.events and
// calls the use cases.
package messaging

import (
	"context"
	"fmt"

	"ridehail/pkg/geo"
	"ridehail/pkg/messaging"
	"ridehail/services/dispatch/internal/usecase"
)

// ConsumerName is the inbox name of the orchestrator (consumer groups add a suffix).
const ConsumerName = "dispatch-service"

// Handlers adapts envelopes to use-case calls.
type Handlers struct {
	Orchestrator *usecase.Orchestrator
	Projection   *usecase.Projection
}

// RideEvent handles ride.events.v1.
func (h Handlers) RideEvent(ctx context.Context, env messaging.Envelope) error {
	ev := usecase.RideEvent{EventID: env.EventID, Type: env.Type, RideID: env.RideID}
	if env.Type == messaging.RideRequested {
		var p struct {
			Pickup geo.Point `json:"pickup"`
		}
		if err := env.Decode(&p); err != nil {
			return fmt.Errorf("decode RideRequested: %w", err)
		}
		ev.Pickup = p.Pickup
	}
	return h.Orchestrator.HandleRideEvent(ctx, ev)
}

// Reply handles dispatch.replies.v1.
func (h Handlers) Reply(ctx context.Context, env messaging.Envelope) error {
	var res messaging.CommandResult
	_ = env.Decode(&res)
	return h.Orchestrator.HandleReply(ctx, usecase.Reply{EventID: env.EventID, Type: env.Type, RideID: env.RideID,
		DriverID: env.DriverID, OfferID: env.OfferID, Command: res.Command, Reason: res.Reason})
}

// DriverEvent handles driver.events.v1 (GEO projection).
func (h Handlers) DriverEvent(ctx context.Context, env messaging.Envelope) error {
	var p struct {
		Status    string  `json:"status"`
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
		Version   int64   `json:"version"`
	}
	if err := env.Decode(&p); err != nil {
		return fmt.Errorf("decode driver event: %w", err)
	}
	return h.Projection.Apply(ctx, usecase.DriverEvent{Type: env.Type, DriverID: env.DriverID, Status: p.Status,
		Location: geo.Point{Latitude: p.Latitude, Longitude: p.Longitude}, Version: p.Version})
}
