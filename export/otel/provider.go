package otel

import (
	"context"
	"fmt"
	"os"
	"strings"

	otelapi "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// globalProvider is the SDK-free default for an Exporter with no
// TracerProvider.
func globalProvider() trace.TracerProvider { return otelapi.GetTracerProvider() }

// Provider builds a TracerProvider for Exporter from the standard OTEL_*
// environment, with IDGenerator installed and spans batched:
//
//	OTEL_TRACES_EXPORTER                 otlp (default), console, or none
//	OTEL_EXPORTER_OTLP_TRACES_PROTOCOL   http/protobuf (default) or grpc;
//	OTEL_EXPORTER_OTLP_PROTOCOL          http/json is not built in
//	OTEL_EXPORTER_OTLP_ENDPOINT          read by the OTLP exporter, with
//	OTEL_EXPORTER_OTLP_TRACES_ENDPOINT   _HEADERS, _TIMEOUT, _INSECURE,
//	                                     _CERTIFICATE, and the rest; the
//	                                     defaults are a stock collector's
//	                                     http://localhost:4318/v1/traces
//	                                     and localhost:4317 (grpc, TLS unless
//	                                     the endpoint's scheme is http or
//	                                     _INSECURE is true)
//	OTEL_SERVICE_NAME                    read into the resource; the default
//	OTEL_RESOURCE_ATTRIBUTES             service.name is agentrt
//
// opts are applied after the exporter and resource, so they may replace
// either. Shut the provider down to flush what is batched.
func Provider(ctx context.Context, opts ...sdktrace.TracerProviderOption) (*sdktrace.TracerProvider, error) {
	res, err := resource.New(ctx,
		resource.WithAttributes(attribute.String("service.name", "agentrt")),
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
	)
	if err != nil {
		return nil, fmt.Errorf("otel: resource: %w", err)
	}
	all := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
	switch kind := strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_TRACES_EXPORTER"))); kind {
	case "", "otlp":
		proto := os.Getenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL")
		if proto == "" {
			proto = os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
		}
		var exp sdktrace.SpanExporter
		switch proto = strings.ToLower(strings.TrimSpace(proto)); proto {
		case "", "http/protobuf":
			exp, err = otlptracehttp.New(ctx)
		case "grpc":
			exp, err = otlptracegrpc.New(ctx)
		default:
			return nil, fmt.Errorf("otel: OTLP protocol %q: http/protobuf or grpc", proto)
		}
		if err != nil {
			return nil, fmt.Errorf("otel: otlp exporter: %w", err)
		}
		all = append(all, sdktrace.WithBatcher(exp))
	case "console":
		exp, err := stdouttrace.New()
		if err != nil {
			return nil, fmt.Errorf("otel: console exporter: %w", err)
		}
		all = append(all, sdktrace.WithBatcher(exp))
	case "none":
	default:
		return nil, fmt.Errorf("otel: OTEL_TRACES_EXPORTER %q: otlp, console, or none", kind)
	}
	return NewTracerProvider(append(all, opts...)...), nil
}
