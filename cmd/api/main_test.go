package main

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/localstack"
	"go.uber.org/fx"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/infrastructure/sqsx"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/testsupport/hostport"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/testsupport/jwttest"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/testsupport/pgtest"
)

// TestFxAppStartsAndStopsCleanly builds the exact module graph main() runs,
// starts it against a real PostgreSQL and LocalStack (containers) and an
// in-process JWKS, then stops it. It checks that every provider resolves,
// every OnStart hook succeeds, and every background worker (SQS consumer,
// outbox publisher, reference retry worker) actually exits during OnStop
// instead of leaking a goroutine.
func TestFxAppStartsAndStopsCleanly(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	databaseURL := pgtest.URL(t)
	issuer := jwttest.NewIssuer(t)
	endpoint := startLocalStack(ctx, t)

	t.Setenv("DATABASE_URL", databaseURL)
	t.Setenv("JWKS_URL", issuer.JWKSURL())
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("SQS_ENDPOINT", endpoint)
	t.Setenv("SQS_TRANSACTIONS_QUEUE_URL", createFIFOQueue(ctx, t, endpoint, "wager-transactions.fifo"))
	t.Setenv("SQS_EVENTS_QUEUE_URL", createFIFOQueue(ctx, t, endpoint, "wager-events.fifo"))
	// Port 0 avoids colliding with an app container already bound to 8080.
	t.Setenv("HTTP_PORT", "0")

	app := fx.New(
		configModule,
		persistenceModule,
		applicationModule,
		messagingModule,
		httpModule,
		fx.Invoke(
			registerHTTPServer,
			registerSQSConsumer,
			registerOutboxPublisher,
			registerReferenceRetryWorker,
		),
		fx.NopLogger,
	)

	startCtx, cancelStart := context.WithTimeout(ctx, 30*time.Second)
	defer cancelStart()
	if err := app.Start(startCtx); err != nil {
		t.Fatalf("app.Start: %v", err)
	}

	stopCtx, cancelStop := context.WithTimeout(ctx, 30*time.Second)
	defer cancelStop()
	if err := app.Stop(stopCtx); err != nil {
		t.Fatalf("app.Stop: %v", err)
	}
}

func startLocalStack(ctx context.Context, t *testing.T) string {
	t.Helper()
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

// createFIFOQueue creates the queue the way deploy/localstack/init-queues.sh
// does (the DLQ does not matter for start/stop) and returns the path-style
// URL the compose setup also uses.
func createFIFOQueue(ctx context.Context, t *testing.T, endpoint, name string) string {
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

	_, err = client.CreateQueue(ctx, &sqs.CreateQueueInput{
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
