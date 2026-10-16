package sqsx

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"go.opentelemetry.io/otel/trace"

	wagerapp "github.com/Ricardoleaom/go-high-load-wallet/internal/application/wager"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/money"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/wager"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/infrastructure/postgres"
	"github.com/google/uuid"
)

// ConsumerName identifies this consumer in the inbox table.
const ConsumerName = "wager-transactions-consumer"

type messageEnvelope struct {
	MessageID  string      `json:"messageId"`
	Type       string      `json:"type"`
	OccurredAt string      `json:"occurredAt"`
	Data       messageData `json:"data"`
}

type messageData struct {
	ProviderID            string `json:"providerId"`
	ExternalTransactionID string `json:"externalTransactionId"`
	IdempotencyKey        string `json:"idempotencyKey"`
	PlayerID              string `json:"playerId"`
	WalletID              string `json:"walletId"`
	RoundID               string `json:"roundId"`
	GameID                string `json:"gameId"`
	Kind                  string `json:"kind"`
	Money                 struct {
		Amount   string `json:"amount"`
		Currency string `json:"currency"`
	} `json:"money"`
	ReferenceExternalTransactionID string `json:"referenceExternalTransactionId,omitempty"`
}

// Consumer polls the wager-transactions queue and feeds messages into the
// same application use case HTTP calls, so both transports share identical
// idempotency and processing guarantees.
//
// The inbox write and the financial commit run as two separate database
// transactions rather than one shared transaction: threading a
// caller-supplied *sql.Tx through the whole wager application service was
// out of scope for the time available. Safety does not depend on that
// atomicity. The transactions table's unique idempotency index (proven
// under 50 concurrent duplicate submissions in
// internal/application/wager/service_test.go) is what actually prevents a
// duplicate movement. The inbox here tracks message-level completion for
// operational visibility and to let a duplicate delivery short-circuit
// before re-running business logic.
type Consumer struct {
	client   *sqs.Client
	queueURL string
	db       *sql.DB
	inbox    *postgres.InboxRepository
	wager    *wagerapp.Service
}

func NewConsumer(
	client *sqs.Client,
	queueURL string,
	db *sql.DB,
	inbox *postgres.InboxRepository,
	wagerService *wagerapp.Service,
) *Consumer {
	return &Consumer{
		client:   client,
		queueURL: queueURL,
		db:       db,
		inbox:    inbox,
		wager:    wagerService,
	}
}

// Run polls until ctx is canceled. On shutdown Fx cancels ctx: no new
// ReceiveMessage starts, and a message mid-processing has its open SQL
// transaction aborted instead of committed, so it is never deleted and
// becomes visible again after the visibility timeout for a safe redelivery.
func (c *Consumer) Run(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return nil
		}

		out, err := c.client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(c.queueURL),
			MaxNumberOfMessages: 10,
			WaitTimeSeconds:     10,
			VisibilityTimeout:   30,
		})
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			slog.Error("receive SQS message", "error", err)
			time.Sleep(time.Second)
			continue
		}

		for _, msg := range out.Messages {
			c.handle(ctx, msg)
		}
	}
}

func (c *Consumer) handle(ctx context.Context, msg sqstypes.Message) {
	ctx, span := tracer.Start(ctx, "sqs.process_message", trace.WithSpanKind(trace.SpanKindConsumer))
	defer span.End()

	sqsMessageID := aws.ToString(msg.MessageId)

	var envelope messageEnvelope
	if err := json.Unmarshal([]byte(aws.ToString(msg.Body)), &envelope); err != nil {
		// A malformed body will never parse; let it exhaust the redrive
		// policy's maxReceiveCount and land in the DLQ instead of looping
		// here forever.
		slog.ErrorContext(ctx, "invalid SQS message body", "error", err, "sqsMessageId", sqsMessageID)
		return
	}

	// The envelope's messageId is the durable identity, not the SQS one: a
	// producer that resends the same envelope gets a brand new SQS MessageId.
	messageID := envelope.MessageID
	if messageID == "" {
		slog.ErrorContext(ctx, "SQS message without envelope messageId", "sqsMessageId", sqsMessageID)
		return
	}

	logger := slog.With(
		"transport", "sqs",
		"messageId", messageID,
		"correlationId", messageID,
		"providerId", envelope.Data.ProviderID,
		"walletId", envelope.Data.WalletID,
	)

	amount, err := money.New(envelope.Data.Money.Amount, envelope.Data.Money.Currency)
	if err != nil {
		logger.ErrorContext(ctx, "invalid money in SQS message", "error", err)
		return
	}

	input := wagerapp.SubmitInput{
		ProviderID:          envelope.Data.ProviderID,
		ExternalID:          envelope.Data.ExternalTransactionID,
		IdempotencyKey:      envelope.Data.IdempotencyKey,
		PlayerID:            envelope.Data.PlayerID,
		WalletID:            envelope.Data.WalletID,
		RoundID:             envelope.Data.RoundID,
		GameID:              envelope.Data.GameID,
		Kind:                wager.Kind(envelope.Data.Kind),
		Amount:              amount,
		ReferenceExternalID: envelope.Data.ReferenceExternalTransactionID,
		CorrelationID:       messageID,
	}

	payloadHash, err := wagerapp.ComputePayloadHash(input)
	if err != nil {
		logger.ErrorContext(ctx, "compute payload hash", "error", err)
		return
	}

	alreadyCompleted, err := c.recordInbox(ctx, messageID, payloadHash)
	if err != nil {
		logger.ErrorContext(ctx, "record inbox message", "error", err)
		return
	}
	if alreadyCompleted {
		logger.InfoContext(ctx, "duplicate SQS delivery acknowledged from inbox")
		c.ack(ctx, msg, logger)
		return
	}

	result, err := c.wager.Submit(ctx, input)
	if err != nil {
		logger.ErrorContext(ctx, "process wager transaction from SQS", "error", err)
		return
	}

	if err := c.completeInbox(ctx, messageID); err != nil {
		logger.ErrorContext(ctx, "complete inbox message", "error", err)
		return
	}

	logger.InfoContext(ctx, "wager transaction handled",
		"transactionId", result.TransactionID,
		"status", result.Status,
		"failureCode", result.FailureCode,
		"idempotentReplay", result.IdempotentReplay,
	)
	c.ack(ctx, msg, logger)
}

func (c *Consumer) recordInbox(ctx context.Context, messageID, payloadHash string) (alreadyCompleted bool, err error) {
	dbTx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin inbox record: %w", err)
	}
	defer dbTx.Rollback()

	alreadyCompleted, err = c.inbox.Record(ctx, dbTx, uuid.NewString(), ConsumerName, messageID, payloadHash)
	if err != nil {
		return false, err
	}

	if err := dbTx.Commit(); err != nil {
		return false, fmt.Errorf("commit inbox record: %w", err)
	}

	return alreadyCompleted, nil
}

func (c *Consumer) completeInbox(ctx context.Context, messageID string) error {
	dbTx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin inbox complete: %w", err)
	}
	defer dbTx.Rollback()

	if err := c.inbox.Complete(ctx, dbTx, ConsumerName, messageID); err != nil {
		return err
	}

	return dbTx.Commit()
}

func (c *Consumer) ack(ctx context.Context, msg sqstypes.Message, logger *slog.Logger) {
	_, err := c.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(c.queueURL),
		ReceiptHandle: msg.ReceiptHandle,
	})
	if err != nil {
		logger.ErrorContext(ctx, "delete processed SQS message", "error", err)
	}
}
