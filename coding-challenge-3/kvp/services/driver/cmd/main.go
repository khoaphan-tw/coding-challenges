// Command driver-service owns driver status, location and the atomic reservation.
//
// main is the composition root (Clean Architecture).
package main

import (
	"context"
	"io/fs"

	"ridehail/pkg/broker"
	"ridehail/pkg/messaging"
	"ridehail/pkg/pgutil"
	"ridehail/pkg/svc"
	dhttp "ridehail/services/driver/internal/adapter/http"
	dmsg "ridehail/services/driver/internal/adapter/messaging"
	"ridehail/services/driver/internal/adapter/postgres"
	"ridehail/services/driver/internal/usecase"
)

func main() {
	svc.Run("driver-service", []fs.FS{postgres.Migrations}, func(_ context.Context, rt *svc.Runtime) error {
		repo := &postgres.Drivers{Pool: rt.Pool}
		drivers := (&usecase.Drivers{
			Tx:        pgutil.TxManager{Pool: rt.Pool},
			Drivers:   repo,
			Publisher: broker.Publisher{},
			Inbox:     broker.Inbox{Consumer: dmsg.ConsumerName},
		}).Init()
		(&dhttp.Handler{Drivers: drivers, Ready: repo.Ready}).Routes(rt.Mux)
		rt.Consume(dmsg.ConsumerName, []string{messaging.TopicDriverCommands}, dmsg.CommandHandler{Drivers: drivers}.Handle)
		return nil
	})
}
