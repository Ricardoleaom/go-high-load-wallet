package sqsx

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// Readiness checks that the configured SQS queue is reachable. It
// implements httpapi.ReadinessChecker structurally, without importing it.
type Readiness struct {
	client   *sqs.Client
	queueURL string
}

func NewReadiness(client *sqs.Client, queueURL string) *Readiness {
	return &Readiness{client: client, queueURL: queueURL}
}

func (r *Readiness) Name() string { return "sqs" }

func (r *Readiness) Ready(ctx context.Context) error {
	_, err := r.client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: aws.String(r.queueURL),
	})
	if err != nil {
		return fmt.Errorf("sqs not reachable: %w", err)
	}
	return nil
}
