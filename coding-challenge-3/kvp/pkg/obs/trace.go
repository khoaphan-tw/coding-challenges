package obs

import (
	"context"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// One ride = one trace.
//
// A ride's journey lasts minutes and is driven by many separate triggers:
// HTTP calls (accept, reject, cancel, complete) and background workers (offer
// timeout, retries). Each would normally start its own trace. Instead, the
// trace context of the request that created the ride is saved with the ride
// (rides.trace_parent, dispatch_sagas.trace_parent), and every later step for
// that ride continues it with ContinueTrace. The span of the HTTP call that
// triggered the step is kept as a span link.

// TraceParent returns ctx's W3C traceparent header value, or "".
func TraceParent(ctx context.Context) string {
	c := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, c)
	return c["traceparent"]
}

// TraceID extracts the trace id from a traceparent ("00-<traceId>-<spanId>-<flags>").
func TraceID(traceparent string) string {
	parts := strings.Split(traceparent, "-")
	if len(parts) == 4 {
		return parts[1]
	}
	return ""
}

// ContinueTrace starts a span inside the trace identified by traceparent.
//   - If ctx is already in that trace, it is a normal child span.
//   - Otherwise the span's parent is the saved ride context, and the current
//     span (e.g. the HTTP request) is attached as a link.
//   - With an empty traceparent it is a normal child span of ctx.
func ContinueTrace(ctx context.Context, traceparent, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	tracer := otel.Tracer("ridehail")
	opts := []trace.SpanStartOption{trace.WithAttributes(attrs...)}
	if traceparent == "" {
		return tracer.Start(ctx, name, opts...)
	}
	remote := trace.SpanContextFromContext(
		otel.GetTextMapPropagator().Extract(context.Background(), propagation.MapCarrier{"traceparent": traceparent}))
	cur := trace.SpanContextFromContext(ctx)
	if !remote.IsValid() || (cur.IsValid() && cur.TraceID() == remote.TraceID()) {
		return tracer.Start(ctx, name, opts...)
	}
	if cur.IsValid() {
		opts = append(opts, trace.WithLinks(trace.Link{SpanContext: cur}))
	}
	ctx, span := tracer.Start(trace.ContextWithRemoteSpanContext(ctx, remote), name, opts...)
	setScopeTrace(ctx, span.SpanContext()) // the request's access-log line joins the ride's trace
	return ctx, span
}

// RideAttr is the span attribute used to search a ride's spans in Tempo:
// TraceQL { span.ride.id = "<rideId>" }.
func RideAttr(rideID string) attribute.KeyValue { return attribute.String("ride.id", rideID) }
