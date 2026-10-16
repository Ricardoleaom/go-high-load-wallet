package observability_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/observability"
)

func TestTraceHandlerAddsIDsFromTheContext(t *testing.T) {
	provider := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	var buf bytes.Buffer
	logger := slog.New(observability.NewTraceHandler(slog.NewJSONHandler(&buf, nil)))

	ctx, span := provider.Tracer("test").Start(context.Background(), "operation")
	logger.InfoContext(ctx, "inside a span")
	span.End()

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("decode log line: %v", err)
	}
	if record["trace_id"] != span.SpanContext().TraceID().String() {
		t.Errorf("trace_id = %v, want %s", record["trace_id"], span.SpanContext().TraceID())
	}
	if record["span_id"] != span.SpanContext().SpanID().String() {
		t.Errorf("span_id = %v, want %s", record["span_id"], span.SpanContext().SpanID())
	}

	buf.Reset()
	logger.InfoContext(context.Background(), "outside any span")
	record = map[string]any{}
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("decode log line: %v", err)
	}
	if _, ok := record["trace_id"]; ok {
		t.Errorf("trace_id logged without a span: %v", record)
	}
}
