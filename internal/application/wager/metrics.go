package wager

import (
	"context"
	"time"

	domainwager "github.com/Ricardoleaom/go-high-load-wallet/internal/domain/wager"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/platform/observability"
)

// RecordSubmit observes one Submit call on wager_submit_duration_seconds.
// kind comes from the caller's payload, so unknown values collapse into
// "OTHER" to keep the label bounded; a failed call is labelled "error".
func RecordSubmit(
	ctx context.Context,
	metrics *observability.Metrics,
	transport string,
	kind domainwager.Kind,
	result Result,
	err error,
	started time.Time,
) {
	kindLabel := string(kind)
	if !kind.Known() {
		kindLabel = "OTHER"
	}
	status := string(result.Status)
	if err != nil {
		status = "error"
	}
	observability.ObserveDuration(ctx,
		metrics.SubmitDuration.WithLabelValues(transport, kindLabel, status),
		time.Since(started).Seconds(),
	)
}
