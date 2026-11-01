// Package otelsetup wires the OpenTelemetry SDK and OTLP exporters (traces,
// metrics, logs) and installs the slog logger. It is infrastructure, called
// only from cmd/ (composition roots); application code uses pkg/obs, which
// depends on the OpenTelemetry API only.
package otelsetup

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"ridehail/pkg/obs"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Setup configures the global logger, tracer and meter providers.
// OTLP export is enabled only when OTEL_EXPORTER_OTLP_ENDPOINT is set, so unit
// tests and plain `go run` work without a collector.
func Setup(ctx context.Context, service string) (shutdown func(context.Context) error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))

	level := slog.LevelInfo
	if os.Getenv("LOG_LEVEL") == "debug" {
		level = slog.LevelDebug
	}
	// Plain-text key=value lines by default; LOG_FORMAT=json for JSON lines.
	// (Loki gets structured fields through OpenTelemetry either way.)
	opts := &slog.HandlerOptions{Level: level}
	var stdout slog.Handler = slog.NewTextHandler(os.Stdout, opts)
	if os.Getenv("LOG_FORMAT") == "json" {
		stdout = slog.NewJSONHandler(os.Stdout, opts)
	}
	stdout = stdout.WithAttrs([]slog.Attr{slog.String("service", service)})
	handlers := []slog.Handler{stdout}

	var shutdowns []func(context.Context) error
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" {
		res, _ := resource.Merge(resource.Default(), resource.NewWithAttributes(
			semconv.SchemaURL, semconv.ServiceName(service)))

		if te, err := otlptracegrpc.New(ctx); err == nil {
			tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(te), sdktrace.WithResource(res))
			otel.SetTracerProvider(tp)
			shutdowns = append(shutdowns, tp.Shutdown)
		}
		if me, err := otlpmetricgrpc.New(ctx); err == nil {
			mp := sdkmetric.NewMeterProvider(sdkmetric.WithResource(res),
				sdkmetric.WithReader(sdkmetric.NewPeriodicReader(me, sdkmetric.WithInterval(10*time.Second))))
			otel.SetMeterProvider(mp)
			shutdowns = append(shutdowns, mp.Shutdown)
		}
		if le, err := otlploggrpc.New(ctx); err == nil {
			lp := sdklog.NewLoggerProvider(sdklog.WithResource(res),
				sdklog.WithProcessor(sdklog.NewBatchProcessor(le)))
			handlers = append(handlers, otelslog.NewHandler(service, otelslog.WithLoggerProvider(lp)))
			shutdowns = append(shutdowns, lp.Shutdown)
		}
	}
	slog.SetDefault(slog.New(obs.NewHandler(level, handlers...)))

	return func(ctx context.Context) error {
		var errs []error
		for _, s := range shutdowns {
			errs = append(errs, s(ctx))
		}
		return errors.Join(errs...)
	}
}
