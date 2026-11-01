// Package messaging is driver-service's driving adapter for Redpanda.
package messaging

import (
	"context"

	"ridehail/pkg/messaging"
	"ridehail/services/driver/internal/usecase"
)

// ConsumerName is the consumer group and inbox name of driver-service.
const ConsumerName = "driver-service"

// CommandHandler consumes driver.commands.v1.
type CommandHandler struct {
	Drivers *usecase.Drivers
}

// Handle implements messaging.Handler.
func (h CommandHandler) Handle(ctx context.Context, env messaging.Envelope) error {
	return h.Drivers.HandleCommand(ctx, usecase.Command{
		EventID: env.EventID, Type: env.Type, RideID: env.RideID, DriverID: env.DriverID, OfferID: env.OfferID})
}
