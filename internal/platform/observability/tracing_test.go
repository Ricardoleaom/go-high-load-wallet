package observability_test

import (
	"context"
	"net"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/platform/observability"
)

func restoreTracerProvider(t *testing.T) {
	t.Helper()
	previous := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(previous) })
}

func TestTracingIsANoOpWithoutAnEndpoint(t *testing.T) {
	restoreTracerProvider(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")

	shutdown, err := observability.SetupTracing(context.Background())
	if err != nil {
		t.Fatalf("SetupTracing: %v", err)
	}
	if _, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider); ok {
		t.Fatal("an SDK tracer provider was installed without an endpoint")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

// Jaeger being down must not stall the API: spans are dropped and shutdown
// gives up at its deadline.
func TestTracingShutdownIsBoundedWhenCollectorIsDown(t *testing.T) {
	restoreTracerProvider(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	closedEndpoint := "http://" + listener.Addr().String()
	_ = listener.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", closedEndpoint)

	shutdown, err := observability.SetupTracing(context.Background())
	if err != nil {
		t.Fatalf("SetupTracing: %v", err)
	}

	tracer := otel.Tracer("test")
	for range 10 {
		_, span := tracer.Start(context.Background(), "work")
		span.End()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	_ = shutdown(ctx)
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("shutdown took %v with the collector down", elapsed)
	}
}
