// Package relay moves committed events from the outbox to the broker.
package relay

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
)

var tracer = otel.Tracer("github.com/Ricardoleaom/go-high-load-wallet/internal/app/relay")

// Event is an outbox row ready to send.
type Event struct {
	ID            string
	AggregateID   string
	EventType     string
	Payload       []byte
	CorrelationID string
	CausationID   string
	OccurredAt    time.Time
}

// Store is where events wait. PublishDue claims up to limit due events,
// calls publish for each and records the result: published, or failed and
// retried after backoff(attempts). Concurrent callers never claim the same
// event.
type Store interface {
	PublishDue(ctx context.Context, limit int, backoff func(attempts int) time.Duration,
		publish func(context.Context, Event) error) (published, failed int, err error)
	// Backlog reports how many events wait and how old the oldest is.
	Backlog(ctx context.Context) (pending int64, oldest time.Duration, err error)
}

// Sink is the broker events go to.
type Sink interface {
	Send(ctx context.Context, e Event) error
}

// Backoff is how long an event waits after its attempts-th failed send:
// 2 seconds doubling, at most 2 minutes.
func Backoff(attempts int) time.Duration {
	const base = 2 * time.Second
	const maxBackoff = 2 * time.Minute
	const maxShift = 20 // base<<20 already dwarfs maxBackoff; caps the shift count

	shift := min(max(attempts, 0), maxShift)
	d := base << uint(shift)
	if d <= 0 || d > maxBackoff {
		return maxBackoff
	}
	return d
}

// Observer is told how each publishing round went.
type Observer interface {
	Batch(ctx context.Context, published, failed int, took time.Duration)
}

// Relay polls the store and sends what is due to the sink. Several relays
// can run at once: the store never hands the same event to two of them.
type Relay struct {
	store    Store
	sink     Sink
	batch    int
	observer Observer
}

func New(store Store, sink Sink, batch int, observer Observer) *Relay {
	return &Relay{store: store, sink: sink, batch: batch, observer: observer}
}

// Run publishes on a fixed interval until ctx ends.
func (r *Relay) Run(ctx context.Context, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if _, err := r.PublishOnce(ctx); err != nil {
				slog.ErrorContext(ctx, "publish outbox batch", "error", err)
			}
		}
	}
}

// PublishOnce runs one round: up to the batch size of due events go to
// the sink, and the round is reported to the observer.
func (r *Relay) PublishOnce(ctx context.Context) (int, error) {
	ctx, span := tracer.Start(ctx, "outbox.publish_batch")
	defer span.End()

	started := time.Now()
	published, failed, err := r.store.PublishDue(ctx, r.batch, Backoff, r.sink.Send)
	r.observer.Batch(ctx, published, failed, time.Since(started))
	if err != nil {
		return published, err
	}
	if published > 0 {
		slog.InfoContext(ctx, "published outbox events", "count", published)
	}
	return published, nil
}
