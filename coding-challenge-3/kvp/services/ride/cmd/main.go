// Command ride-service owns rides, passengers and the ride state machine.
//
// main is the composition root (Clean Architecture): it builds the driven
// adapters (Postgres, outbox, inbox), injects them into the use cases, and
// connects the driving adapters (HTTP, Redpanda consumer).
package main

import (
	"context"
	"io/fs"

	"ridehail/pkg/broker"
	"ridehail/pkg/messaging"
	"ridehail/pkg/pgutil"
	"ridehail/pkg/svc"
	rhttp "ridehail/services/ride/internal/adapter/http"
	rmsg "ridehail/services/ride/internal/adapter/messaging"
	"ridehail/services/ride/internal/adapter/postgres"
	"ridehail/services/ride/internal/usecase"
)

func main() {
	svc.Run("ride-service", []fs.FS{postgres.Migrations}, func(_ context.Context, rt *svc.Runtime) error {
		repos := &postgres.Repos{Pool: rt.Pool}
		rides := &usecase.Rides{
			Tx:          pgutil.TxManager{Pool: rt.Pool},
			Rides:       repos,
			Passengers:  repos,
			Idempotency: repos,
			Publisher:   broker.Publisher{},
			Inbox:       broker.Inbox{Consumer: rmsg.ConsumerName},
		}
		(&rhttp.Handler{Rides: rides, Ready: repos.Ready}).Routes(rt.Mux)
		rt.Consume(rmsg.ConsumerName, []string{messaging.TopicRideCommands}, rmsg.CommandHandler{Rides: rides}.Handle)
		return nil
	})
}
