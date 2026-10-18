package observability_test

import (
	"context"
	"runtime"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/platform/buildinfo"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/platform/observability"
)

func TestBuildInfoIsExposed(t *testing.T) {
	families, err := observability.New().Registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}

	for _, family := range families {
		if family.GetName() != "wallet_build_info" {
			continue
		}
		metric := family.GetMetric()[0]
		labels := map[string]string{}
		for _, pair := range metric.GetLabel() {
			labels[pair.GetName()] = pair.GetValue()
		}
		if labels["version"] != buildinfo.Version || labels["goversion"] != runtime.Version() {
			t.Fatalf("labels = %v, want version %q and goversion %q", labels, buildinfo.Version, runtime.Version())
		}
		if value := metric.GetGauge().GetValue(); value != 1 {
			t.Fatalf("value = %v, want 1", value)
		}
		return
	}
	t.Fatal("wallet_build_info is not registered")
}

func TestGoAndProcessCollectorsAreRegistered(t *testing.T) {
	families, err := observability.New().Registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	names := map[string]bool{}
	for _, family := range families {
		names[family.GetName()] = true
	}
	for _, want := range []string{"go_goroutines", "process_cpu_seconds_total"} {
		if !names[want] {
			t.Errorf("%s is not registered", want)
		}
	}
}

func TestObserveDurationAttachesTheTraceAsExemplar(t *testing.T) {
	provider := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	metrics := observability.New()
	ctx, span := provider.Tracer("test").Start(context.Background(), "request")
	observability.ObserveDuration(ctx, metrics.OutboxBatchDuration, 0.003)
	span.End()

	if got := exemplarTraceIDs(t, metrics, "outbox_publish_batch_duration_seconds"); len(got) != 1 || got[0] != span.SpanContext().TraceID().String() {
		t.Fatalf("exemplar trace IDs = %v, want [%s]", got, span.SpanContext().TraceID())
	}

	untraced := observability.New()
	observability.ObserveDuration(context.Background(), untraced.OutboxBatchDuration, 0.003)
	if got := exemplarTraceIDs(t, untraced, "outbox_publish_batch_duration_seconds"); len(got) != 0 {
		t.Fatalf("exemplar recorded without a span: %v", got)
	}
}

func exemplarTraceIDs(t *testing.T, metrics *observability.Metrics, name string) []string {
	t.Helper()
	families, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	var ids []string
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, bucket := range metric.GetHistogram().GetBucket() {
				for _, label := range bucket.GetExemplar().GetLabel() {
					if label.GetName() == "trace_id" {
						ids = append(ids, label.GetValue())
					}
				}
			}
		}
	}
	return ids
}
