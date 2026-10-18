package wager

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/observability"
)

// RetryWorker periodically resolves PENDING_REFERENCE transactions whose
// next attempt is due. Several instances can run it concurrently: each round
// claims its rows with SELECT ... FOR UPDATE SKIP LOCKED (see
// ListDuePendingReferences), and applyReversal locks the original
// transaction, so no reference is processed twice at once.
type RetryWorker struct {
	service *Service
	metrics *observability.Metrics
	batch   int
}

func NewRetryWorker(service *Service, metrics *observability.Metrics) *RetryWorker {
	return &RetryWorker{service: service, metrics: metrics, batch: 20}
}

func (w *RetryWorker) Run(ctx context.Context, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			roundCtx, span := tracer.Start(ctx, "reference_retry.run")
			count, err := w.service.RetryPendingReferences(roundCtx, w.batch)
			if err != nil {
				slog.ErrorContext(roundCtx, "retry pending references", "error", err)
			}
			if count > 0 {
				w.metrics.ReferenceRetriesTotal.Add(float64(count))
				slog.InfoContext(roundCtx, "retried pending references", "count", count)
			}
			span.End()
		}
	}
}

var tracer = otel.Tracer("github.com/Ricardoleaom/go-high-load-wallet/internal/application/wager")
