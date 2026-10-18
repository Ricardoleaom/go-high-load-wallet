package sqsx_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"

	wagerapp "github.com/Ricardoleaom/go-high-load-wallet/internal/application/wager"
	walletapp "github.com/Ricardoleaom/go-high-load-wallet/internal/application/wallet"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/money"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/infrastructure/postgres"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/infrastructure/sqsx"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/observability"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/testsupport/pgtest"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/testsupport/sqstest"
)

func openFreshDB(ctx context.Context, t *testing.T) *sql.DB {
	t.Helper()
	db, err := postgres.Open(ctx, pgtest.FreshURL(t))
	if err != nil {
		t.Fatalf("open PostgreSQL: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestConsumerRecordsOutcomes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	endpoint := sqstest.Start(ctx, t)
	queueURL := sqstest.CreateFIFOQueue(ctx, t, endpoint, "wager-transactions.fifo")
	client := sqstest.Client(ctx, t, endpoint)
	db := openFreshDB(ctx, t)

	wallets := postgres.NewWalletRepository(db)
	transactions := postgres.NewTransactionRepository(db)
	ledgerRepo := postgres.NewLedgerRepository(db)
	outbox := postgres.NewOutboxRepository(db)
	metrics := observability.New()
	consumer := sqsx.NewConsumer(client, queueURL, db, postgres.NewInboxRepository(db),
		wagerapp.NewService(db, wallets, transactions, ledgerRepo, outbox), metrics)

	initial, err := money.New("100.00", "BRL")
	if err != nil {
		t.Fatalf("money: %v", err)
	}
	opened, err := walletapp.NewService(db, wallets, transactions, ledgerRepo, outbox).
		Open(ctx, walletapp.OpenInput{PlayerID: uuid.NewString(), InitialBalance: initial})
	if err != nil {
		t.Fatalf("open wallet: %v", err)
	}

	bet := betEnvelope(t, opened.ID(), opened.PlayerID())
	send(ctx, t, client, queueURL, opened.ID(), "first", bet)
	send(ctx, t, client, queueURL, "invalid", "malformed", "not json")
	// Same envelope, new SQS deduplication ID: a producer-side resend that
	// only the inbox can recognize.
	send(ctx, t, client, queueURL, opened.ID(), "resend", bet)

	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- consumer.Run(runCtx) }()

	deadline := time.Now().Add(90 * time.Second)
	for outcomeCount(t, metrics, "processed") < 1 || outcomeCount(t, metrics, "duplicate") < 1 || outcomeCount(t, metrics, "invalid") < 1 {
		if time.Now().After(deadline) {
			stop()
			t.Fatalf("outcomes after 90s: processed=%d duplicate=%d invalid=%d",
				outcomeCount(t, metrics, "processed"), outcomeCount(t, metrics, "duplicate"), outcomeCount(t, metrics, "invalid"))
		}
		time.Sleep(200 * time.Millisecond)
	}
	stop()
	if err := <-done; err != nil {
		t.Fatalf("consumer stopped with error: %v", err)
	}

	if got := outcomeCount(t, metrics, "processed"); got != 1 {
		t.Fatalf("processed = %d, want 1", got)
	}
	reloaded, err := wallets.GetByID(ctx, opened.ID())
	if err != nil {
		t.Fatalf("reload wallet: %v", err)
	}
	if got := reloaded.Balance().DecimalString(); got != "75.00" {
		t.Fatalf("balance = %s, want 75.00 (debited exactly once)", got)
	}
}

func TestPublishOnceSendsDueEventsAndRecordsTheBatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	endpoint := sqstest.Start(ctx, t)
	queueURL := sqstest.CreateFIFOQueue(ctx, t, endpoint, "wager-events.fifo")
	client := sqstest.Client(ctx, t, endpoint)
	db := openFreshDB(ctx, t)
	outbox := postgres.NewOutboxRepository(db)

	eventID := uuid.NewString()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := outbox.Append(ctx, tx, postgres.OutboxEvent{
		ID: eventID, AggregateID: "aggregate-1", EventType: "TestEvent",
		Payload: []byte(`{"eventId":"` + eventID + `"}`), CorrelationID: "correlation", OccurredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("append event: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	metrics := observability.New()
	published, err := sqsx.NewOutboxPublisher(client, queueURL, outbox, metrics, 20).PublishOnce(ctx)
	if err != nil {
		t.Fatalf("PublishOnce: %v", err)
	}
	if published != 1 {
		t.Fatalf("published = %d, want 1", published)
	}

	out, err := client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:                    aws.String(queueURL),
		MaxNumberOfMessages:         10,
		WaitTimeSeconds:             5,
		MessageSystemAttributeNames: []sqstypes.MessageSystemAttributeName{sqstypes.MessageSystemAttributeNameMessageDeduplicationId},
	})
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	if len(out.Messages) != 1 {
		t.Fatalf("received %d messages, want 1", len(out.Messages))
	}
	if got := out.Messages[0].Attributes["MessageDeduplicationId"]; got != eventID {
		t.Fatalf("MessageDeduplicationId = %q, want the event ID %q", got, eventID)
	}
	if got := sampleCount(t, metrics, "outbox_publish_batch_duration_seconds", nil); got != 1 {
		t.Fatalf("batch observations = %d, want 1", got)
	}
}

func betEnvelope(t *testing.T, walletID, playerID string) string {
	t.Helper()
	externalID := uuid.NewString()
	raw, err := json.Marshal(map[string]any{
		"messageId":  uuid.NewString(),
		"type":       "WagerTransactionRequested",
		"occurredAt": time.Now().UTC().Format(time.RFC3339),
		"data": map[string]any{
			"providerId":            "provider-a",
			"externalTransactionId": externalID,
			"idempotencyKey":        "provider-a:" + externalID,
			"playerId":              playerID,
			"walletId":              walletID,
			"roundId":               "round-sqs",
			"gameId":                "game-sqs",
			"kind":                  "BET",
			"money":                 map[string]string{"amount": "25.00", "currency": "BRL"},
		},
	})
	if err != nil {
		t.Fatalf("encode envelope: %v", err)
	}
	return string(raw)
}

func send(ctx context.Context, t *testing.T, client *sqs.Client, queueURL, group, dedup, body string) {
	t.Helper()
	_, err := client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:               aws.String(queueURL),
		MessageBody:            aws.String(body),
		MessageGroupId:         aws.String(group),
		MessageDeduplicationId: aws.String(dedup),
	})
	if err != nil {
		t.Fatalf("send %s: %v", dedup, err)
	}
}

func outcomeCount(t *testing.T, metrics *observability.Metrics, outcome string) uint64 {
	t.Helper()
	return sampleCount(t, metrics, "sqs_message_processing_duration_seconds", map[string]string{"outcome": outcome})
}

func sampleCount(t *testing.T, metrics *observability.Metrics, name string, labels map[string]string) uint64 {
	t.Helper()
	families, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
	series:
		for _, metric := range family.GetMetric() {
			got := map[string]string{}
			for _, pair := range metric.GetLabel() {
				got[pair.GetName()] = pair.GetValue()
			}
			for key, value := range labels {
				if got[key] != value {
					continue series
				}
			}
			return metric.GetHistogram().GetSampleCount()
		}
	}
	return 0
}

func TestConsumerClassifiesMalformedIDsAsInvalid(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	endpoint := sqstest.Start(ctx, t)
	queueURL := sqstest.CreateFIFOQueue(ctx, t, endpoint, "wager-transactions.fifo")
	client := sqstest.Client(ctx, t, endpoint)
	db := openFreshDB(ctx, t)

	wallets := postgres.NewWalletRepository(db)
	transactions := postgres.NewTransactionRepository(db)
	metrics := observability.New()
	consumer := sqsx.NewConsumer(client, queueURL, db, postgres.NewInboxRepository(db),
		wagerapp.NewService(db, wallets, transactions, postgres.NewLedgerRepository(db), postgres.NewOutboxRepository(db)),
		metrics)

	send(ctx, t, client, queueURL, "malformed", "malformed-ids", betEnvelope(t, "abc", "def"))

	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- consumer.Run(runCtx) }()

	deadline := time.Now().Add(60 * time.Second)
	for outcomeCount(t, metrics, "invalid") < 1 {
		if time.Now().After(deadline) {
			stop()
			t.Fatalf("outcomes after 60s: invalid=%d error=%d", outcomeCount(t, metrics, "invalid"), outcomeCount(t, metrics, "error"))
		}
		time.Sleep(200 * time.Millisecond)
	}
	stop()
	if err := <-done; err != nil {
		t.Fatalf("consumer stopped with error: %v", err)
	}

	var recorded int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM transactions`).Scan(&recorded); err != nil {
		t.Fatalf("count transactions: %v", err)
	}
	if recorded != 0 {
		t.Fatalf("transactions recorded = %d, want 0", recorded)
	}
}
