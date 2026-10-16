package sqsx

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/infrastructure/postgres"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/observability"
)

// OutboxPublisher drains the outbox into the events queue. Several
// instances can run this concurrently: PublishDue claims rows with
// SELECT ... FOR UPDATE SKIP LOCKED, so they never double-publish, and a
// crash mid-batch just releases the lock for the next poller to pick up.
type OutboxPublisher struct {
	client   *sqs.Client
	queueURL string
	outbox   *postgres.OutboxRepository
	metrics  *observability.Metrics
	batch    int
}

func NewOutboxPublisher(
	client *sqs.Client,
	queueURL string,
	outbox *postgres.OutboxRepository,
	metrics *observability.Metrics,
) *OutboxPublisher {
	return &OutboxPublisher{client: client, queueURL: queueURL, outbox: outbox, metrics: metrics, batch: 20}
}

// Run polls on a fixed interval until ctx is canceled.
func (p *OutboxPublisher) Run(ctx context.Context, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			published, err := p.outbox.PublishDue(ctx, p.batch, backoff, p.publish)
			if err != nil {
				slog.Error("publish outbox batch", "error", err)
				continue
			}
			if published > 0 {
				slog.Info("published outbox events", "count", published)
			}
		}
	}
}

func (p *OutboxPublisher) publish(ctx context.Context, event postgres.OutboxEvent) error {
	_, err := p.client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:               aws.String(p.queueURL),
		MessageBody:            aws.String(string(event.Payload)),
		MessageGroupId:         aws.String(event.AggregateID),
		MessageDeduplicationId: aws.String(event.ID),
	})
	if err != nil {
		p.metrics.OutboxPublishFailedTotal.Inc()
		return fmt.Errorf("send event %s: %w", event.ID, err)
	}
	p.metrics.OutboxPublishedTotal.Inc()
	return nil
}

func backoff(attempts int) time.Duration {
	const base = 2 * time.Second
	const maxBackoff = 2 * time.Minute
	const maxShift = 20 // base<<20 already dwarfs maxBackoff; caps the shift count

	shift := attempts
	if shift < 0 {
		shift = 0
	}
	if shift > maxShift {
		shift = maxShift
	}

	d := base << uint(shift)
	if d <= 0 || d > maxBackoff {
		return maxBackoff
	}
	return d
}
