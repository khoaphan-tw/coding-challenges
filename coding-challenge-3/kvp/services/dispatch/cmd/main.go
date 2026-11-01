// Command dispatch-service runs the dispatch saga: matching, offers,
// timeouts and the Redis GEO projection.
//
// main is the composition root (Clean Architecture): it builds the driven
// adapters (Postgres, Redis, driver-service client, outbox, inbox), injects
// them into the use cases, and connects the driving adapters (HTTP,
// Redpanda consumers, background workers).
package main

import (
	"context"
	"io/fs"
	"log/slog"
	"strconv"
	"time"

	"github.com/redis/go-redis/extra/redisotel/v9"
	goredis "github.com/redis/go-redis/v9"

	"ridehail/pkg/broker"
	"ridehail/pkg/messaging"
	"ridehail/pkg/pgutil"
	"ridehail/pkg/svc"
	"ridehail/services/dispatch/internal/adapter/driverclient"
	dhttp "ridehail/services/dispatch/internal/adapter/http"
	dmsg "ridehail/services/dispatch/internal/adapter/messaging"
	"ridehail/services/dispatch/internal/adapter/postgres"
	"ridehail/services/dispatch/internal/adapter/redis"
	"ridehail/services/dispatch/internal/usecase"
)

func main() {
	svc.Run("dispatch-service", []fs.FS{postgres.Migrations}, func(ctx context.Context, rt *svc.Runtime) error {
		// Driven adapters.
		rdb := goredis.NewClient(&goredis.Options{Addr: svc.Env("REDIS_ADDR", "localhost:6379")})
		_ = redisotel.InstrumentTracing(rdb)
		index := &redis.Index{R: rdb}
		sagas := &postgres.Sagas{Pool: rt.Pool}

		// Use cases.
		radius, _ := strconv.ParseFloat(svc.Env("MATCH_RADIUS_M", "2000"), 64)
		orch := (&usecase.Orchestrator{
			Tx:            pgutil.TxManager{Pool: rt.Pool},
			Sagas:         sagas,
			Offers:        &postgres.Offers{Pool: rt.Pool},
			Exclusions:    &postgres.Exclusions{Pool: rt.Pool},
			Candidates:    index,
			Publisher:     broker.Publisher{},
			Inbox:         broker.Inbox{Consumer: dmsg.ConsumerName},
			RadiusM:       radius,
			OfferTimeout:  svc.Duration("OFFER_TIMEOUT", 30*time.Second),
			RetryInterval: svc.Duration("DISPATCH_RETRY_INTERVAL", 10*time.Second),
		}).Init()
		proj := &usecase.Projection{Index: index, OnAvailable: orch.WakeWaiting,
			Directory: &driverclient.Client{BaseURL: svc.Env("DRIVER_SERVICE_URL", "http://localhost:8082")}}

		// Driving adapters: HTTP.
		(&dhttp.Handler{Orchestrator: orch, Projection: proj, Ready: func(ctx context.Context) error {
			if err := rt.Pool.Ping(ctx); err != nil {
				return err
			}
			return rdb.Ping(ctx).Err()
		}}).Routes(rt.Mux)

		// Driving adapters: Redpanda consumers.
		h := dmsg.Handlers{Orchestrator: orch, Projection: proj}
		rt.Consume(dmsg.ConsumerName+"-rides", []string{messaging.TopicRideEvents}, h.RideEvent)
		rt.Consume(dmsg.ConsumerName+"-replies", []string{messaging.TopicDispatchReplies}, h.Reply)
		rt.Consume(dmsg.ConsumerName+"-geo", []string{messaging.TopicDriverEvents}, h.DriverEvent)

		// Driving adapters: background workers.
		rt.Go("geo bootstrap", func(ctx context.Context) error {
			for {
				err := proj.RebuildIfEmpty(ctx)
				if err == nil || ctx.Err() != nil {
					return nil
				}
				slog.WarnContext(ctx, "geo rebuild failed, retrying", "error", err)
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(3 * time.Second):
				}
			}
		})
		rt.Go("offer timeout sweeper", func(ctx context.Context) error {
			return every(ctx, svc.Duration("SWEEPER_INTERVAL", time.Second), orch.SweepExpired)
		})
		stuckAfter := svc.Duration("SAGA_STUCK_AFTER", 30*time.Second)
		rt.Go("saga retrier", func(ctx context.Context) error {
			return every(ctx, time.Second, func(ctx context.Context) { orch.RetryAndResume(ctx, stuckAfter) })
		})
		return nil
	})
}

// every runs fn on a fixed interval until ctx is cancelled.
func every(ctx context.Context, d time.Duration, fn func(context.Context)) error {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			fn(ctx)
		}
	}
}
