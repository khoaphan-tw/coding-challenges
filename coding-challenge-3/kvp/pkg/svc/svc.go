// Package svc is the common bootstrap of every service: config, telemetry,
// Postgres + migrations, Redpanda topics, outbox relay, HTTP server, the
// optional gRPC server and graceful shutdown.
package svc

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/grpc"

	"ridehail/pkg/broker"
	"ridehail/pkg/grpcx"
	"ridehail/pkg/httpx"
	"ridehail/pkg/messaging"
	"ridehail/pkg/obs/otelsetup"
	"ridehail/pkg/pgutil"
)

// Env returns the environment variable or def.
func Env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Duration parses an environment duration or returns def.
func Duration(key string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(key)); err == nil && d > 0 {
		return d
	}
	return def
}

// Int parses an environment integer or returns def.
func Int(key string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return n
	}
	return def
}

// Runtime is what a service gets to wire itself up.
type Runtime struct {
	Name     string
	Pool     *pgxpool.Pool
	Producer *kgo.Client
	Brokers  []string
	Mux      *http.ServeMux
	// GRPC serves on GRPC_ADDR (default :9090) when a service registers on it.
	GRPC *grpc.Server

	ctx context.Context
	wg  sync.WaitGroup
}

// Go runs fn in the background until shutdown.
func (rt *Runtime) Go(name string, fn func(ctx context.Context) error) {
	rt.wg.Add(1)
	go func() {
		defer rt.wg.Done()
		if err := fn(rt.ctx); err != nil && rt.ctx.Err() == nil {
			slog.Error("background worker stopped", "worker", name, "error", err)
			os.Exit(1)
		}
	}()
}

// Consume starts a consumer-group worker that calls h for each message.
func (rt *Runtime) Consume(group string, topics []string, h messaging.Handler) {
	c := &broker.Consumer{Name: group, Brokers: rt.Brokers, Topics: topics, Handler: h,
		DLQProducer: rt.Producer, MaxAttempts: Int("CONSUMER_MAX_ATTEMPTS", 5)}
	rt.Go("consumer "+group, c.Run)
}

// Run boots the service and blocks until SIGINT/SIGTERM.
func Run(name string, migrations []fs.FS, setup func(ctx context.Context, rt *Runtime) error) {
	// `app healthcheck` is used by Docker (the distroless image has no curl).
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		addr := Env("HTTP_ADDR", ":8080")
		if strings.HasPrefix(addr, ":") {
			addr = "localhost" + addr
		}
		resp, err := (&http.Client{Timeout: 2 * time.Second}).Get("http://" + addr + "/readyz")
		if err != nil || resp.StatusCode != 200 {
			os.Exit(1)
		}
		os.Exit(0)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	shutdown := otelsetup.Setup(ctx, name)
	defer shutdown(context.Background()) //nolint:errcheck

	fail := func(msg string, err error) {
		slog.Error(msg, "error", err)
		os.Exit(1)
	}

	pool, err := pgutil.Connect(ctx, Env("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/postgres"))
	if err != nil {
		fail("database connect failed", err)
	}
	defer pool.Close()
	for _, m := range append([]fs.FS{broker.Migrations}, migrations...) {
		if err := pgutil.Migrate(ctx, pool, m); err != nil {
			fail("migration failed", err)
		}
	}

	brokers := strings.Split(Env("KAFKA_BROKERS", "localhost:19092"), ",")
	producer, err := broker.NewProducer(brokers)
	if err != nil {
		fail("kafka client failed", err)
	}
	defer producer.Close()
	if err := broker.EnsureTopics(ctx, producer, int32(Int("TOPIC_PARTITIONS", 6)), messaging.AllTopics...); err != nil {
		fail("topic setup failed", err)
	}

	rt := &Runtime{Name: name, Pool: pool, Producer: producer, Brokers: brokers, Mux: http.NewServeMux(),
		GRPC: grpcx.NewServer(), ctx: ctx}
	relay := &broker.Relay{Pool: pool, Client: producer, Interval: Duration("OUTBOX_POLL_INTERVAL", 50*time.Millisecond)}
	rt.Go("outbox relay", func(ctx context.Context) error { relay.Run(ctx); return nil })

	if err := setup(ctx, rt); err != nil {
		fail("service setup failed", err)
	}

	if grpcx.HasServices(rt.GRPC) {
		grpcAddr := Env("GRPC_ADDR", ":9090")
		rt.Go("grpc server", func(ctx context.Context) error { return grpcx.Serve(ctx, rt.GRPC, grpcAddr) })
	}

	addr := Env("HTTP_ADDR", ":8080")
	srv := &http.Server{Addr: addr, Handler: httpx.Handler(name, rt.Mux), ReadHeaderTimeout: 5 * time.Second}
	slog.Info("service started", "addr", addr)
	if err := httpx.Serve(ctx, srv); err != nil && err != http.ErrServerClosed {
		fail("http server failed", err)
	}
	stop()
	rt.wg.Wait()
	slog.Info("service stopped")
}
