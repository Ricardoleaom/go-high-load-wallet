package observability

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// traceHandler adds the active span's trace_id and span_id to every record
// logged with a context, so a log line leads straight to its trace.
type traceHandler struct {
	next slog.Handler
}

func NewTraceHandler(next slog.Handler) slog.Handler {
	return traceHandler{next: next}
}

func (h traceHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h traceHandler) Handle(ctx context.Context, record slog.Record) error {
	if spanContext := trace.SpanContextFromContext(ctx); spanContext.IsValid() {
		record.AddAttrs(
			slog.String("trace_id", spanContext.TraceID().String()),
			slog.String("span_id", spanContext.SpanID().String()),
		)
	}
	return h.next.Handle(ctx, record)
}

func (h traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return traceHandler{next: h.next.WithAttrs(attrs)}
}

func (h traceHandler) WithGroup(name string) slog.Handler {
	return traceHandler{next: h.next.WithGroup(name)}
}
