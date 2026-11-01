// Package httpx has small HTTP helpers shared by all services:
// JSON I/O, RFC 9457 problem responses, request logging and health checks.
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"ridehail/pkg/obs"
)

// Problem is an RFC 9457 problem document with a stable machine-readable code.
type Problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

// Error is an error that maps to an HTTP problem response.
type Error struct {
	Status int
	Code   string
	Detail string
}

func (e *Error) Error() string { return e.Code + ": " + e.Detail }

// NewError builds an *Error.
func NewError(status int, code, detail string) *Error {
	return &Error{Status: status, Code: code, Detail: detail}
}

// WriteJSON writes v as JSON with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// WriteError writes err as a problem document. Unknown errors become 500.
func WriteError(ctx context.Context, w http.ResponseWriter, err error) {
	var he *Error
	if !errors.As(err, &he) {
		slog.ErrorContext(ctx, "unhandled error", "error", err)
		he = &Error{Status: http.StatusInternalServerError, Code: "INTERNAL", Detail: "internal error"}
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(he.Status)
	_ = json.NewEncoder(w).Encode(Problem{
		Type: "about:blank", Title: http.StatusText(he.Status), Status: he.Status, Code: he.Code, Detail: he.Detail,
	})
}

// DecodeJSON reads a JSON body (max 64 KiB) into v. Unknown fields are rejected.
func DecodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return NewError(http.StatusBadRequest, "INVALID_JSON", err.Error())
	}
	return nil
}

// recorder captures the status and (up to obs.MaxBodyLog+1 bytes of) the response body.
type recorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (r *recorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if room := obs.MaxBodyLog + 1 - r.body.Len(); room > 0 {
		r.body.Write(b[:min(len(b), room)])
	}
	return r.ResponseWriter.Write(b)
}

// Handler wraps a mux with OpenTelemetry and writes one access-log line per
// request with method, path, query, status, duration, request body and
// response body (each truncated to obs.MaxBodyLog bytes).
//
// Path values rideId / driverId (and fields added with obs.Annotate) go into
// the log context. When the request acted on a ride (obs.ContinueTrace), the
// line carries the ride's trace_id; the request's own trace is http_trace_id.
func Handler(service string, mux *http.ServeMux) http.Handler {
	logged := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			mux.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		var reqBody []byte
		if r.Body != nil {
			reqBody, _ = io.ReadAll(io.LimitReader(r.Body, 1<<20))
			r.Body = io.NopCloser(bytes.NewReader(reqBody))
		}
		r = r.WithContext(obs.WithRequestScope(r.Context()))
		rec := &recorder{ResponseWriter: w, status: 200}
		// ServeMux stores the matched pattern and path values on r itself.
		mux.ServeHTTP(rec, r)

		span := trace.SpanFromContext(r.Context())
		if v := r.PathValue("rideId"); v != "" {
			span.SetAttributes(obs.RideAttr(v))
		}
		if v := r.PathValue("driverId"); v != "" {
			span.SetAttributes(attribute.String("driver.id", v))
		}
		ctx := obs.With(r.Context(), obs.RideID, r.PathValue("rideId"), obs.DriverID, r.PathValue("driverId"))
		ctx, httpTrace := obs.ScopeLogContext(ctx)

		attrs := []any{"method", r.Method, "path", r.URL.Path, "route", r.Pattern,
			"status", rec.status, "duration_ms", time.Since(start).Milliseconds(), "http_trace_id", httpTrace}
		if r.URL.RawQuery != "" {
			attrs = append(attrs, "query", r.URL.RawQuery)
		}
		if k := r.Header.Get("Idempotency-Key"); k != "" {
			attrs = append(attrs, "idempotency_key", k)
		}
		if len(reqBody) > 0 {
			attrs = append(attrs, "request_body", obs.NewBody(reqBody))
		}
		if rec.body.Len() > 0 {
			attrs = append(attrs, "response_body", obs.NewBody(rec.body.Bytes()))
		}
		slog.InfoContext(ctx, "http request", attrs...)
	})
	return otelhttp.NewHandler(logged, service, otelhttp.WithSpanNameFormatter(
		func(_ string, r *http.Request) string { return r.Method + " " + r.URL.Path }))
}

// Health registers /healthz (liveness) and /readyz (dependencies).
func Health(mux *http.ServeMux, ready func(context.Context) error) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		WriteJSON(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := ready(ctx); err != nil {
			WriteJSON(w, 503, map[string]string{"status": "not ready", "error": err.Error()})
			return
		}
		WriteJSON(w, 200, map[string]string{"status": "ready"})
	})
}

// Serve runs srv until ctx is cancelled, then shuts down gracefully.
func Serve(ctx context.Context, srv *http.Server) error {
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(sctx)
	}
}
