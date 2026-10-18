package sqsx

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/relay"
)

// Sink sends outbox events to the events queue. FIFO groups by aggregate
// keep one aggregate's events in order, and the event ID as deduplication
// ID drops a resend of the same event within SQS's five-minute window.
type Sink struct {
	client   *sqs.Client
	queueURL string
}

func NewSink(client *sqs.Client, queueURL string) *Sink {
	return &Sink{client: client, queueURL: queueURL}
}

func (s *Sink) Send(ctx context.Context, e relay.Event) error {
	if _, err := s.client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:               aws.String(s.queueURL),
		MessageBody:            aws.String(string(e.Payload)),
		MessageGroupId:         aws.String(e.AggregateID),
		MessageDeduplicationId: aws.String(e.ID),
	}); err != nil {
		return fmt.Errorf("send event %s: %w", e.ID, err)
	}
	return nil
}

var _ relay.Sink = (*Sink)(nil)
