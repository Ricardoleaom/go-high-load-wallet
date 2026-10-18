package wagering

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
)

var tracer = otel.Tracer("github.com/Ricardoleaom/go-high-load-wallet/internal/app/wagering")

// RetryWorker retries due pending reversals on a timer. Several instances
// can run it at once: each round claims its reversals under a lease, and
// the engine locks a reversal while it settles it.
type RetryWorker struct {
	service   *Service
	onRetried func(n int)
	batch     int
}

// NewRetryWorker reports how many reversals each round handled to
// onRetried, which may be nil.
func NewRetryWorker(service *Service, onRetried func(n int)) *RetryWorker {
	return &RetryWorker{service: service, onRetried: onRetried, batch: 20}
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
				if w.onRetried != nil {
					w.onRetried(count)
				}
				slog.InfoContext(roundCtx, "retried pending references", "count", count)
			}
			span.End()
		}
	}
}
