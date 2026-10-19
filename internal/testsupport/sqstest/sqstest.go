// Package sqstest runs a LocalStack SQS for integration tests.
package sqstest

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/localstack"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/adapters/sqsx"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/testsupport/hostport"
)

// Start runs LocalStack with only SQS enabled for the rest of the test and
// returns its endpoint.
func Start(ctx context.Context, t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}

	ctr, err := localstack.Run(ctx, "localstack/localstack:3.8",
		testcontainers.WithEnv(map[string]string{"SERVICES": "sqs"}),
		hostport.Publish("4566/tcp"),
	)
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start LocalStack: %v", err)
	}

	endpoint, err := ctr.PortEndpoint(ctx, "4566/tcp", "http")
	if err != nil {
		t.Fatalf("LocalStack endpoint: %v", err)
	}
	return endpoint
}

// Client returns an SQS client for a LocalStack endpoint.
func Client(ctx context.Context, t *testing.T, endpoint string) *sqs.Client {
	t.Helper()
	client, err := sqsx.NewClient(ctx, sqsx.Config{
		Region:          "us-east-1",
		Endpoint:        endpoint,
		AccessKeyID:     "test",
		SecretAccessKey: "test",
	})
	if err != nil {
		t.Fatalf("SQS client: %v", err)
	}
	return client
}

// CreateFIFOQueue creates the queue the way deploy/localstack/init-queues.sh
// does (without its DLQ) and returns the path-style URL the compose setup
// also uses.
func CreateFIFOQueue(ctx context.Context, t *testing.T, endpoint, name string) string {
	t.Helper()
	_, err := Client(ctx, t, endpoint).CreateQueue(ctx, &sqs.CreateQueueInput{
		QueueName: aws.String(name),
		Attributes: map[string]string{
			"FifoQueue":                 "true",
			"ContentBasedDeduplication": "false",
		},
	})
	if err != nil {
		t.Fatalf("create queue %s: %v", name, err)
	}
	return endpoint + "/000000000000/" + name
}
