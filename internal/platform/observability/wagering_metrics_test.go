package observability_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/platform/observability"
)

// sampleCount returns how many observations the histogram name holds under
// exactly labels.
func sampleCount(t *testing.T, m *observability.Metrics, name string, labels map[string]string) uint64 {
	t.Helper()
	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
	metrics:
		for _, metric := range f.GetMetric() {
			for _, pair := range metric.GetLabel() {
				if labels[pair.GetName()] != pair.GetValue() {
					continue metrics
				}
			}
			return metric.GetHistogram().GetSampleCount()
		}
	}
	return 0
}

func TestObserveSubmitRecordsTheLatencyByTransportKindAndStatus(t *testing.T) {
	m := observability.New()
	observability.ObserveSubmit(context.Background(), m, "sqs", "BET", "PROCESSED", time.Now())

	labels := map[string]string{"transport": "sqs", "kind": "BET", "status": "PROCESSED"}
	if n := sampleCount(t, m, "wager_submit_duration_seconds", labels); n != 1 {
		t.Fatalf("%d observations under %v, want 1", n, labels)
	}
}

func TestRelayObserverCountsEachBatch(t *testing.T) {
	m := observability.New()
	observer := observability.NewRelayObserver(m)
	observer.Batch(context.Background(), 3, 1, 20*time.Millisecond)
	observer.Batch(context.Background(), 2, 0, 10*time.Millisecond)

	if got := testutil.ToFloat64(m.OutboxPublishedTotal); got != 5 {
		t.Errorf("published = %v, want 5", got)
	}
	if got := testutil.ToFloat64(m.OutboxPublishFailedTotal); got != 1 {
		t.Errorf("failed = %v, want 1", got)
	}
	if n := sampleCount(t, m, "outbox_publish_batch_duration_seconds", nil); n != 2 {
		t.Errorf("%d batch durations, want 2", n)
	}
}
