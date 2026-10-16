package wager

import (
	"context"
	"log/slog"
	"time"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/observability"
)

// RetryWorker periodically resolves PENDING_REFERENCE transactions whose
// next attempt is due. Several instances can run it concurrently: the
// underlying SELECT ... FOR UPDATE SKIP LOCKED pattern used when applying a
// reversal (see applyReversal's LockByID call) keeps them from double
// -processing the same reference.
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
			count, err := w.service.RetryPendingReferences(ctx, w.batch)
			if err != nil {
				slog.Error("retry pending references", "error", err)
				continue
			}
			if count > 0 {
				w.metrics.ReferenceRetriesTotal.Add(float64(count))
				slog.Info("retried pending references", "count", count)
			}
		}
	}
}
