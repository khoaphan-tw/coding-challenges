// Package messaging is ride-service's driving adapter for Redpanda: it turns
// saga command envelopes into use-case calls.
package messaging

import (
	"context"

	"ridehail/pkg/messaging"
	"ridehail/services/ride/internal/usecase"
)

// ConsumerName is the consumer group and inbox name of ride-service.
const ConsumerName = "ride-service"

// CommandHandler consumes ride.commands.v1.
type CommandHandler struct {
	Rides *usecase.Rides
}

// Handle implements messaging.Handler.
func (h CommandHandler) Handle(ctx context.Context, env messaging.Envelope) error {
	return h.Rides.HandleCommand(ctx, usecase.Command{
		EventID: env.EventID, Type: env.Type, RideID: env.RideID, DriverID: env.DriverID, OfferID: env.OfferID})
}
