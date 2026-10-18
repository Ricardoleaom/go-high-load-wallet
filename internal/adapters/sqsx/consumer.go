package sqsx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"go.opentelemetry.io/otel/trace"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/wagering"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/money"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/wager"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/platform/observability"
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

// Inbox records which messages this consumer has handled, so a redelivery
// of a completed one is acknowledged without being applied again.
type Inbox interface {
	Record(ctx context.Context, consumer, messageID, payloadHash string) (alreadyCompleted bool, err error)
	Complete(ctx context.Context, consumer, messageID string) error
}

// Consumer polls the wager-transactions queue and feeds messages into the
// same use case HTTP calls, so both transports share identical idempotency
// and processing guarantees.
//
// The inbox and the submission are separate writes. Safety does not depend
// on them being atomic: the engine applies a submission once per
// idempotency key whatever the transport, so a message whose inbox record
// was lost is simply answered as a replay. The inbox lets a redelivery of a
// completed message short-circuit before reaching the engine.
type Consumer struct {
	client   *sqs.Client
	queueURL string
	inbox    Inbox
	wagering *wagering.Service
	metrics  *observability.Metrics
}

func NewConsumer(
	client *sqs.Client,
	queueURL string,
	inbox Inbox,
	service *wagering.Service,
	metrics *observability.Metrics,
) *Consumer {
	return &Consumer{
		client:   client,
		queueURL: queueURL,
		inbox:    inbox,
		wagering: service,
		metrics:  metrics,
	}
}

// Run polls until ctx is canceled. On shutdown the context ends: no new
// ReceiveMessage starts, and a message mid-processing is cancelled before
// it is deleted, so it becomes visible again after the visibility timeout
// for a safe redelivery.
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

	// Every return below sets outcome first: invalid messages will never
	// succeed and head for the DLQ, errors are retried by redelivery.
	started := time.Now()
	outcome := "error"
	defer func() {
		observability.ObserveDuration(ctx,
			c.metrics.SQSProcessingDuration.WithLabelValues(outcome), time.Since(started).Seconds())
	}()

	sqsMessageID := aws.ToString(msg.MessageId)

	// A message that does not decode never will: let it exhaust the
	// redrive policy's maxReceiveCount and land in the DLQ instead of
	// looping here forever.
	messageID, input, err := decodeMessage([]byte(aws.ToString(msg.Body)))
	if err != nil {
		slog.ErrorContext(ctx, "invalid SQS message", "error", err, "sqsMessageId", sqsMessageID)
		outcome = "invalid"
		return
	}

	logger := slog.With(
		"transport", "sqs",
		"messageId", messageID,
		"correlationId", messageID,
		"providerId", input.ProviderID,
		"walletId", input.WalletID,
	)

	payloadHash, err := wagering.ComputePayloadHash(input)
	if err != nil {
		logger.ErrorContext(ctx, "compute payload hash", "error", err)
		return
	}

	alreadyCompleted, err := c.inbox.Record(ctx, ConsumerName, messageID, payloadHash)
	if err != nil {
		logger.ErrorContext(ctx, "record inbox message", "error", err)
		return
	}
	if alreadyCompleted {
		logger.InfoContext(ctx, "duplicate SQS delivery acknowledged from inbox")
		c.ack(ctx, msg, logger)
		outcome = "duplicate"
		return
	}

	submitStarted := time.Now()
	result, err := c.wagering.Submit(ctx, input)
	kindLabel, statusLabel := wagering.MetricLabels(input.Kind, result, err)
	observability.ObserveSubmit(ctx, c.metrics, "sqs", kindLabel, statusLabel, submitStarted)
	if err != nil {
		logger.ErrorContext(ctx, "process wager transaction from SQS", "error", err)
		if invalidSubmission(err) {
			outcome = "invalid"
		}
		return
	}

	if err := c.inbox.Complete(ctx, ConsumerName, messageID); err != nil {
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
	outcome = "processed"
}

// invalidSubmission tells a message that can never succeed, whatever the
// number of redeliveries, from a failure worth retrying. Either way the
// message stays on the queue and reaches the DLQ after its last attempt.
func invalidSubmission(err error) bool {
	return errors.Is(err, wagering.ErrInvalidInput) ||
		errors.Is(err, wagering.ErrOpeningNotAllowed) ||
		errors.Is(err, wager.ErrInvalidTransaction) ||
		errors.Is(err, wager.ErrInvalidAmount) ||
		errors.Is(err, money.ErrInvalidAmount) ||
		errors.Is(err, money.ErrInvalidCurrency)
}

// decodeMessage reads a queue message into the use case's input. The
// envelope's messageId is the message's durable identity, not the SQS one:
// a producer that resends the same envelope gets a new SQS MessageId.
func decodeMessage(body []byte) (string, wagering.SubmitInput, error) {
	var envelope messageEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return "", wagering.SubmitInput{}, fmt.Errorf("decode message body: %w", err)
	}
	if envelope.MessageID == "" {
		return "", wagering.SubmitInput{}, errors.New("message without an envelope messageId")
	}
	// The messageId keys the inbox and correlates every event the message
	// causes: it is held to the limit of the submission's own fields.
	if utf8.RuneCountInString(envelope.MessageID) > wagering.MaxFieldLength {
		return "", wagering.SubmitInput{}, fmt.Errorf("messageId over %d characters", wagering.MaxFieldLength)
	}
	amount, err := money.New(envelope.Data.Money.Amount, envelope.Data.Money.Currency)
	if err != nil {
		return "", wagering.SubmitInput{}, fmt.Errorf("decode message money: %w", err)
	}
	data := envelope.Data
	return envelope.MessageID, wagering.SubmitInput{
		ProviderID:          data.ProviderID,
		ExternalID:          data.ExternalTransactionID,
		IdempotencyKey:      data.IdempotencyKey,
		PlayerID:            data.PlayerID,
		WalletID:            data.WalletID,
		RoundID:             data.RoundID,
		GameID:              data.GameID,
		Kind:                wager.Kind(data.Kind),
		Amount:              amount,
		ReferenceExternalID: data.ReferenceExternalTransactionID,
		CorrelationID:       envelope.MessageID,
	}, nil
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
