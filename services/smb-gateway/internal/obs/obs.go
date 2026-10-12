// Package obs holds the gateway's logging, tracing and metrics setup.
//
// Logs are JSON on stdout (Alloy ships every pod's stdout to Loki). Lines
// written inside a span carry "trace_id", which Grafana's Loki datasource
// turns into a link to the trace in Tempo - the same convention as
// Traefik's access log. Traces go to Tempo over OTLP/gRPC when
// OTEL_EXPORTER_OTLP_ENDPOINT is set. Metrics are served on /metrics.
package obs

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

const ServiceName = "smb-gateway"

// Tracer is the gateway's tracer; a no-op until SetupTracing installs a
// provider.
func Tracer() trace.Tracer { return otel.Tracer(ServiceName) }

// ParseLevel accepts debug, info, warn, error.
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

// NewLogger returns a JSON logger that adds trace_id/span_id from the
// context to every line logged with a context carrying a span.
func NewLogger(level slog.Level) *slog.Logger {
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				a.Value = slog.StringValue(a.Value.Time().UTC().Format(time.RFC3339Nano))
			}
			return a
		},
	})
	return slog.New(traceHandler{h}).With("service", ServiceName)
}

type traceHandler struct{ slog.Handler }

func (h traceHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	return h.Handler.Handle(ctx, r)
}

func (h traceHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return traceHandler{h.Handler.WithAttrs(as)}
}

func (h traceHandler) WithGroup(n string) slog.Handler {
	return traceHandler{h.Handler.WithGroup(n)}
}

// SetupTracing exports spans over OTLP/gRPC if OTEL_EXPORTER_OTLP_ENDPOINT
// (or ..._TRACES_ENDPOINT) is set, and otherwise leaves tracing off. The
// returned function flushes and stops the exporter.
func SetupTracing(ctx context.Context, log *slog.Logger) func(context.Context) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") == "" {
		log.Info("tracing disabled (OTEL_EXPORTER_OTLP_ENDPOINT not set)")
		return func(context.Context) {}
	}
	exp, err := otlptracegrpc.New(ctx)
	if err != nil {
		log.Error("tracing disabled: creating OTLP exporter failed", "err", err)
		return func(context.Context) {}
	}
	res, _ := resource.Merge(resource.Default(), resource.NewWithAttributes(semconv.SchemaURL,
		semconv.ServiceName(ServiceName)))
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
		// OTEL_TRACES_SAMPLER(_ARG) can override; the gateway's own spans
		// are low-volume (one trace per change batch), so keep them all.
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
	)
	otel.SetTracerProvider(tp)
	log.Info("tracing enabled", "endpoint", firstNonEmpty(os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"), os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")))
	return func(ctx context.Context) { _ = tp.Shutdown(ctx) }
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
