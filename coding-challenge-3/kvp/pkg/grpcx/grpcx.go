// Package grpcx is the shared gRPC server setup (an outer layer, like httpx):
// OpenTelemetry tracing, logging of every call and stream message with the
// trace id, the standard health service, server reflection and graceful stop.
package grpcx

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"ridehail/pkg/obs"
)

// NewServer returns a gRPC server with tracing and logging interceptors.
func NewServer() *grpc.Server {
	return grpc.NewServer(
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		grpc.ChainUnaryInterceptor(unaryLog),
		grpc.ChainStreamInterceptor(streamLog),
	)
}

// HasServices reports whether any application service is registered.
func HasServices(s *grpc.Server) bool { return len(s.GetServiceInfo()) > 0 }

// Serve registers health and reflection, listens on addr and serves until ctx
// is cancelled. Open streams get a short grace period, then are cancelled.
func Serve(ctx context.Context, s *grpc.Server, addr string) error {
	hs := health.NewServer()
	healthpb.RegisterHealthServer(s, hs)
	reflection.Register(s)

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	slog.Info("grpc server started", "addr", addr)
	go func() {
		<-ctx.Done()
		hs.Shutdown()
		done := make(chan struct{})
		go func() { s.GracefulStop(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			s.Stop() // long-lived streams: cancel them
		}
	}()
	if err := s.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return err
	}
	return nil
}

// body renders a protobuf message as JSON for the logs (truncated like HTTP bodies).
func body(m any) obs.Body {
	pm, ok := m.(proto.Message)
	if !ok {
		return obs.NewBody(nil)
	}
	b, _ := protojson.MarshalOptions{UseProtoNames: false}.Marshal(pm)
	return obs.NewBody(b)
}

// withDriver adds driver_id to the log context when the message carries one.
func withDriver(ctx context.Context, m any) context.Context {
	if d, ok := m.(interface{ GetDriverId() string }); ok && d.GetDriverId() != "" {
		return obs.With(ctx, obs.DriverID, d.GetDriverId())
	}
	return ctx
}

func unaryLog(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
	start := time.Now()
	resp, err := h(ctx, req)
	attrs := []any{"method", info.FullMethod, "code", status.Code(err).String(),
		"duration_ms", time.Since(start).Milliseconds(), "request_body", body(req)}
	if resp != nil {
		attrs = append(attrs, "response_body", body(resp))
	}
	if err != nil {
		attrs = append(attrs, "error", status.Convert(err).Message())
	}
	slog.InfoContext(withDriver(ctx, req), "grpc request", attrs...)
	return resp, err
}

// loggedStream logs every message received and sent on a stream.
type loggedStream struct {
	grpc.ServerStream
	method     string
	recv, sent atomic.Int64
	ctx        atomic.Value // context.Context, enriched with driver_id once known
}

func (s *loggedStream) logCtx() context.Context {
	if c, ok := s.ctx.Load().(context.Context); ok {
		return c
	}
	return s.ServerStream.Context()
}

func (s *loggedStream) RecvMsg(m any) error {
	if err := s.ServerStream.RecvMsg(m); err != nil {
		return err
	}
	s.recv.Add(1)
	ctx := withDriver(s.ServerStream.Context(), m)
	s.ctx.Store(ctx)
	slog.InfoContext(ctx, "grpc message", "direction", "recv", "method", s.method, "body", body(m))
	return nil
}

func (s *loggedStream) SendMsg(m any) error {
	if err := s.ServerStream.SendMsg(m); err != nil {
		return err
	}
	s.sent.Add(1)
	slog.InfoContext(withDriver(s.logCtx(), m), "grpc message", "direction", "send", "method", s.method, "body", body(m))
	return nil
}

func streamLog(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, h grpc.StreamHandler) error {
	if info.FullMethod == "/grpc.reflection.v1.ServerReflection/ServerReflectionInfo" ||
		info.FullMethod == "/grpc.reflection.v1alpha.ServerReflection/ServerReflectionInfo" {
		return h(srv, ss)
	}
	start := time.Now()
	ls := &loggedStream{ServerStream: ss, method: info.FullMethod}
	slog.InfoContext(ss.Context(), "grpc stream opened", "method", info.FullMethod)
	err := h(srv, ls)
	attrs := []any{"method", info.FullMethod, "code", status.Code(err).String(),
		"duration_ms", time.Since(start).Milliseconds(), "messages_received", ls.recv.Load(), "messages_sent", ls.sent.Load()}
	if err != nil {
		attrs = append(attrs, "error", status.Convert(err).Message())
	}
	slog.InfoContext(ls.logCtx(), "grpc stream closed", attrs...)
	return err
}
