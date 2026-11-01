// Package obs holds cross-cutting observability helpers used by every layer:
// correlation fields in the context (ride_id, driver_id, event_id, …), the
// slog handler that adds them plus trace_id/span_id to every line, request
// scope, payload bodies, and "one trace per ride" (trace.go).
//
// It depends only on the OpenTelemetry API, not the SDK or exporters; those
// are wired by pkg/obs/otelsetup from the composition roots.
package obs

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

type ctxKey struct{}

// Field names used across all services. Keep them stable: runbooks query them.
const (
	RideID      = "ride_id"
	DriverID    = "driver_id"
	OfferID     = "offer_id"
	EventID     = "event_id"
	CausationID = "causation_id"
	MessageType = "message_type"
	SagaStep    = "saga_step"
)

// With returns a context whose logs include the given key/value pairs.
// Later values for the same key win. Empty string values are skipped.
func With(ctx context.Context, kv ...string) context.Context {
	prev, _ := ctx.Value(ctxKey{}).([]slog.Attr)
	attrs := make([]slog.Attr, 0, len(prev)+len(kv)/2)
	attrs = append(attrs, prev...)
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == "" {
			continue
		}
		replaced := false
		for j := range attrs {
			if attrs[j].Key == kv[i] {
				attrs[j] = slog.String(kv[i], kv[i+1])
				replaced = true
			}
		}
		if !replaced {
			attrs = append(attrs, slog.String(kv[i], kv[i+1]))
		}
	}
	return context.WithValue(ctx, ctxKey{}, attrs)
}

// Field returns a correlation field stored in ctx, or "".
func Field(ctx context.Context, key string) string {
	attrs, _ := ctx.Value(ctxKey{}).([]slog.Attr)
	for _, a := range attrs {
		if a.Key == key {
			return a.Value.String()
		}
	}
	return ""
}

// NewHandler returns a slog handler that adds context fields and trace ids to
// each record and fans out to handlers (e.g. stdout + OpenTelemetry).
func NewHandler(level slog.Level, handlers ...slog.Handler) slog.Handler {
	return ctxHandler{level: level, handlers: handlers}
}

// ctxHandler adds context fields and trace ids to each record, then fans out.
type ctxHandler struct {
	level    slog.Level
	handlers []slog.Handler
}

func (h ctxHandler) Enabled(ctx context.Context, l slog.Level) bool {
	if l < h.level {
		return false
	}
	for _, x := range h.handlers {
		if x.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (h ctxHandler) Handle(ctx context.Context, r slog.Record) error {
	if attrs, ok := ctx.Value(ctxKey{}).([]slog.Attr); ok {
		r.AddAttrs(attrs...)
	}
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	for _, x := range h.handlers {
		if x.Enabled(ctx, r.Level) {
			_ = x.Handle(ctx, r.Clone())
		}
	}
	return nil
}

func (h ctxHandler) WithAttrs(as []slog.Attr) slog.Handler {
	out := ctxHandler{level: h.level}
	for _, x := range h.handlers {
		out.handlers = append(out.handlers, x.WithAttrs(as))
	}
	return out
}

func (h ctxHandler) WithGroup(name string) slog.Handler {
	out := ctxHandler{level: h.level}
	for _, x := range h.handlers {
		out.handlers = append(out.handlers, x.WithGroup(name))
	}
	return out
}
