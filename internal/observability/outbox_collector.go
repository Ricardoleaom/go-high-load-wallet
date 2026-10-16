package observability

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	outboxPendingDesc = prometheus.NewDesc(
		"outbox_pending_events", "Outbox events not yet published.", nil, nil)
	outboxOldestDesc = prometheus.NewDesc(
		"outbox_oldest_pending_age_seconds", "Age of the oldest unpublished outbox event.", nil, nil)
)

type backlogResult struct {
	pending int64
	oldest  time.Duration
	err     error
}

// outboxCollector queries the outbox backlog on every scrape. The query is
// bounded by timeout and runs aside from the scrape, so a slow or
// unreachable database only removes these two series from that scrape
// instead of stalling or failing it.
type outboxCollector struct {
	backlog func(context.Context) (int64, time.Duration, error)
	timeout time.Duration
}

func NewOutboxCollector(backlog func(context.Context) (int64, time.Duration, error), timeout time.Duration) prometheus.Collector {
	return outboxCollector{backlog: backlog, timeout: timeout}
}

func (c outboxCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- outboxPendingDesc
	ch <- outboxOldestDesc
}

func (c outboxCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	done := make(chan backlogResult, 1)
	go func() {
		pending, oldest, err := c.backlog(ctx)
		done <- backlogResult{pending: pending, oldest: oldest, err: err}
	}()

	select {
	case result := <-done:
		if result.err != nil {
			slog.Warn("outbox backlog unavailable for this scrape", "error", result.err)
			return
		}
		ch <- prometheus.MustNewConstMetric(outboxPendingDesc, prometheus.GaugeValue, float64(result.pending))
		ch <- prometheus.MustNewConstMetric(outboxOldestDesc, prometheus.GaugeValue, result.oldest.Seconds())
	case <-ctx.Done():
		slog.Warn("outbox backlog timed out for this scrape", "timeout", c.timeout)
	}
}
