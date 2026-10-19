package observability_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/platform/observability"
)

func TestOutboxCollectorReportsBacklog(t *testing.T) {
	collector := observability.NewOutboxCollector(func(context.Context) (int64, time.Duration, error) {
		return 3, 90 * time.Second, nil
	}, time.Second)

	expected := `
# HELP outbox_oldest_pending_age_seconds Age of the oldest unpublished outbox event.
# TYPE outbox_oldest_pending_age_seconds gauge
outbox_oldest_pending_age_seconds 90
# HELP outbox_pending_events Outbox events not yet published.
# TYPE outbox_pending_events gauge
outbox_pending_events 3
`
	if err := testutil.CollectAndCompare(collector, strings.NewReader(expected)); err != nil {
		t.Fatal(err)
	}
}

// A scrape while PostgreSQL is down or slow must stay fast and must not
// fail: the backlog series are simply absent for that scrape.
func TestOutboxCollectorOmitsSeriesWhenBacklogFails(t *testing.T) {
	failing := observability.NewOutboxCollector(func(context.Context) (int64, time.Duration, error) {
		return 0, 0, errors.New("connection refused")
	}, time.Second)
	if got := testutil.CollectAndCount(failing); got != 0 {
		t.Fatalf("failing backlog emitted %d series, want 0", got)
	}

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	hanging := observability.NewOutboxCollector(func(context.Context) (int64, time.Duration, error) {
		<-release
		return 1, time.Second, nil
	}, 50*time.Millisecond)

	started := time.Now()
	if got := testutil.CollectAndCount(hanging); got != 0 {
		t.Fatalf("hanging backlog emitted %d series, want 0", got)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("collecting took %v with a hung database", elapsed)
	}
}
