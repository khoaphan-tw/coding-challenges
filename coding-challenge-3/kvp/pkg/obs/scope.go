package obs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"go.opentelemetry.io/otel/trace"
)

// Request scope: lets code deep inside a request add fields to, and choose the
// trace of, the single access-log line written when the request finishes.
//   - Annotate(ctx, "ride_id", id): e.g. POST /rides learns its ride id late.
//   - ContinueTrace records the ride's trace here, so the access-log line of
//     accept/reject/cancel/complete carries the ride's trace_id (one trace id
//     finds every log line of the journey, request/response bodies included).
type scope struct {
	mu     sync.Mutex
	fields []string
	sc     trace.SpanContext
}

type scopeKey struct{}

// WithRequestScope returns a context that collects request-level log data.
func WithRequestScope(ctx context.Context) context.Context {
	return context.WithValue(ctx, scopeKey{}, &scope{})
}

// Annotate adds key/value pairs to the request's access-log line (no-op outside a request).
func Annotate(ctx context.Context, kv ...string) {
	if s, ok := ctx.Value(scopeKey{}).(*scope); ok {
		s.mu.Lock()
		s.fields = append(s.fields, kv...)
		s.mu.Unlock()
	}
}

func setScopeTrace(ctx context.Context, sc trace.SpanContext) {
	if s, ok := ctx.Value(scopeKey{}).(*scope); ok {
		s.mu.Lock()
		s.sc = sc
		s.mu.Unlock()
	}
}

// ScopeLogContext returns the context to log the access line with: the
// annotated fields added and, if the request continued a ride's trace, that
// trace as the current span context. requestTraceID is the request's own trace.
func ScopeLogContext(ctx context.Context) (logCtx context.Context, requestTraceID string) {
	requestTraceID = trace.SpanContextFromContext(ctx).TraceID().String()
	s, ok := ctx.Value(scopeKey{}).(*scope)
	if !ok {
		return ctx, requestTraceID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	logCtx = With(ctx, s.fields...)
	if s.sc.IsValid() {
		logCtx = trace.ContextWithSpanContext(logCtx, s.sc)
	}
	return logCtx, requestTraceID
}

// MaxBodyLog is the largest body (bytes) written to a log line.
const MaxBodyLog = 4096

// Body is a request/response/message body for logs: a quoted one-line string
// in text logs and in OpenTelemetry/Loki; embedded JSON with LOG_FORMAT=json.
type Body struct {
	raw       []byte
	truncated int
}

// NewBody wraps b, keeping at most MaxBodyLog bytes.
func NewBody(b []byte) Body {
	if len(b) > MaxBodyLog {
		return Body{raw: b[:MaxBodyLog], truncated: len(b) - MaxBodyLog}
	}
	return Body{raw: b}
}

// MarshalJSON implements json.Marshaler.
func (b Body) MarshalJSON() ([]byte, error) {
	if len(b.raw) == 0 {
		return []byte("null"), nil
	}
	if b.truncated == 0 {
		var buf bytes.Buffer
		if err := json.Compact(&buf, b.raw); err == nil {
			return buf.Bytes(), nil
		}
	}
	return json.Marshal(b.String())
}

// String implements fmt.Stringer (used by the text log format and the
// OpenTelemetry log bridge). Valid JSON is compacted onto one line.
func (b Body) String() string {
	if b.truncated > 0 {
		return fmt.Sprintf("%s…(%d more bytes)", b.raw, b.truncated)
	}
	var buf bytes.Buffer
	if json.Compact(&buf, b.raw) == nil {
		return buf.String()
	}
	return string(b.raw)
}
