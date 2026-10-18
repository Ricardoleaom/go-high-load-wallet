// Package observability holds the service's Prometheus metrics, tracing
// setup and log correlation.
package observability

import (
	"context"
	"net/http"
	"runtime"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/trace"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/platform/buildinfo"
)

// LatencyBuckets spans sub-millisecond database round trips up to
// multi-second stalls, which is where a wallet's latency lives.
var LatencyBuckets = []float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}

type Metrics struct {
	Registry *prometheus.Registry

	HTTPRequestsTotal         *prometheus.CounterVec
	WagerTransactionsTotal    *prometheus.CounterVec
	IdempotentReplaysTotal    prometheus.Counter
	ConcurrencyConflictsTotal prometheus.Counter
	OutboxPublishedTotal      prometheus.Counter
	OutboxPublishFailedTotal  prometheus.Counter
	ReferenceRetriesTotal     prometheus.Counter
	ReconciliationMismatches  prometheus.Counter
	AuthTokenCacheTotal       *prometheus.CounterVec

	HTTPRequestDuration   *prometheus.HistogramVec
	SubmitDuration        *prometheus.HistogramVec
	SQSProcessingDuration *prometheus.HistogramVec
	OutboxBatchDuration   prometheus.Histogram
}

func New() *Metrics {
	registry := prometheus.NewRegistry()

	m := &Metrics{
		Registry: registry,
		HTTPRequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "HTTP requests by path, method and status.",
		}, []string{"path", "method", "status"}),
		WagerTransactionsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wager_transactions_total",
			Help: "Processed wager transactions by resulting status.",
		}, []string{"status"}),
		IdempotentReplaysTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "wager_idempotent_replays_total",
			Help: "Submissions that replayed a previously persisted result.",
		}),
		ConcurrencyConflictsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "wager_concurrency_conflicts_total",
			Help: "Submissions that lost a race on the idempotency key and replayed the winner.",
		}),
		OutboxPublishedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "outbox_events_published_total",
			Help: "Outbox events successfully published.",
		}),
		OutboxPublishFailedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "outbox_events_publish_failed_total",
			Help: "Outbox publish attempts that failed and were rescheduled with backoff.",
		}),
		ReferenceRetriesTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "wager_reference_retries_total",
			Help: "PENDING_REFERENCE transactions retried by the background worker.",
		}),
		ReconciliationMismatches: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "wallet_reconciliation_mismatches_total",
			Help: "Reconciliation checks that found stored and calculated balances differ.",
		}),
		AuthTokenCacheTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "auth_token_cache_total",
			Help: "Bearer token lookups in the verified-token cache, by result (hit or miss).",
		}, []string{"result"}),
		HTTPRequestDuration: prometheus.NewHistogramVec(latencyOpts(
			"http_request_duration_seconds", "HTTP request latency by route, method and status class.",
		), []string{"route", "method", "status_class"}),
		SubmitDuration: prometheus.NewHistogramVec(latencyOpts(
			"wager_submit_duration_seconds", "Wager submission latency by transport, kind and resulting status.",
		), []string{"transport", "kind", "status"}),
		SQSProcessingDuration: prometheus.NewHistogramVec(latencyOpts(
			"sqs_message_processing_duration_seconds", "SQS message handling latency by outcome.",
		), []string{"outcome"}),
		OutboxBatchDuration: prometheus.NewHistogram(latencyOpts(
			"outbox_publish_batch_duration_seconds", "Duration of one outbox publishing round.",
		)),
	}

	buildInfo := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "wallet_build_info",
		Help: "Always 1; the labels identify the running build.",
		ConstLabels: prometheus.Labels{
			"version":   buildinfo.Version,
			"goversion": runtime.Version(),
		},
	})
	buildInfo.Set(1)

	registry.MustRegister(
		buildInfo,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.HTTPRequestsTotal,
		m.WagerTransactionsTotal,
		m.IdempotentReplaysTotal,
		m.ConcurrencyConflictsTotal,
		m.OutboxPublishedTotal,
		m.OutboxPublishFailedTotal,
		m.ReferenceRetriesTotal,
		m.ReconciliationMismatches,
		m.AuthTokenCacheTotal,
		m.HTTPRequestDuration,
		m.SubmitDuration,
		m.SQSProcessingDuration,
		m.OutboxBatchDuration,
	)

	return m
}

func latencyOpts(name, help string) prometheus.HistogramOpts {
	return prometheus.HistogramOpts{
		Name:                        name,
		Help:                        help,
		Buckets:                     LatencyBuckets,
		NativeHistogramBucketFactor: 1.1,
	}
}

// Handler serves the registry in the OpenMetrics format when the scraper
// asks for it, which is what carries exemplars.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{EnableOpenMetrics: true})
}

// ObserveDuration records seconds on o and, when ctx carries a sampled span,
// attaches its trace ID as an exemplar so a latency spike links to the
// trace that caused it.
func ObserveDuration(ctx context.Context, o prometheus.Observer, seconds float64) {
	spanContext := trace.SpanContextFromContext(ctx)
	if exemplars, ok := o.(prometheus.ExemplarObserver); ok && spanContext.IsSampled() {
		exemplars.ObserveWithExemplar(seconds, prometheus.Labels{"trace_id": spanContext.TraceID().String()})
		return
	}
	o.Observe(seconds)
}

// ObserveSubmit records one wager submission on wager_submit_duration_seconds.
// The caller bounds the labels (wagering.MetricLabels).
func ObserveSubmit(ctx context.Context, m *Metrics, transport, kind, status string, started time.Time) {
	ObserveDuration(ctx, m.SubmitDuration.WithLabelValues(transport, kind, status), time.Since(started).Seconds())
}

// RelayObserver records each outbox publishing round.
type RelayObserver struct {
	metrics *Metrics
}

func NewRelayObserver(m *Metrics) RelayObserver {
	return RelayObserver{metrics: m}
}

func (o RelayObserver) Batch(ctx context.Context, published, failed int, took time.Duration) {
	o.metrics.OutboxPublishedTotal.Add(float64(published))
	o.metrics.OutboxPublishFailedTotal.Add(float64(failed))
	ObserveDuration(ctx, o.metrics.OutboxBatchDuration, took.Seconds())
}
