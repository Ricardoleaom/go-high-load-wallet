// Package relay moves committed events from the outbox to the broker.
package relay

import (
	"context"
	"time"
)

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
