// Package outboxevents builds the outbox rows for the events required by
// the challenge, shared by the wallet and wager application services so
// both emit the exact same envelope shape.
package outboxevents

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/application/events"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/ledger"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/wager"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/infrastructure/postgres"
)

// AppendProcessed records WagerTransactionProcessed and, when the operation
// moved the wallet balance (entry != nil), the paired WalletBalanceChanged.
// LOSS and other no-movement kinds pass a nil entry.
func AppendProcessed(
	ctx context.Context,
	tx *sql.Tx,
	outbox *postgres.OutboxRepository,
	transaction wager.Transaction,
	entry *ledger.Entry,
	walletVersion int64,
	correlationID string,
) error {
	id, payload, occurredAt, err := events.Build(
		events.TypeWagerTransactionProcessed,
		transaction.ID(),
		correlationID,
		"",
		events.WagerTransactionProcessedData{
			TransactionID: transaction.ID(),
			WalletID:      transaction.WalletID(),
			Kind:          string(transaction.Kind()),
			Money:         events.DTO(transaction.Amount()),
		},
	)
	if err != nil {
		return fmt.Errorf("build WagerTransactionProcessed: %w", err)
	}

	if err := outbox.Append(ctx, tx, postgres.OutboxEvent{
		ID:            id,
		AggregateID:   transaction.ID(),
		EventType:     events.TypeWagerTransactionProcessed,
		Payload:       payload,
		CorrelationID: correlationID,
		OccurredAt:    occurredAt,
	}); err != nil {
		return fmt.Errorf("append WagerTransactionProcessed: %w", err)
	}

	if entry == nil {
		return nil
	}

	balanceID, balancePayload, balanceOccurredAt, err := events.Build(
		events.TypeWalletBalanceChanged,
		transaction.WalletID(),
		correlationID,
		id,
		events.WalletBalanceChangedData{
			WalletID:      transaction.WalletID(),
			TransactionID: transaction.ID(),
			Direction:     string(entry.Direction()),
			Money:         events.DTO(entry.Amount()),
			BalanceBefore: events.DTO(entry.BalanceBefore()),
			BalanceAfter:  events.DTO(entry.BalanceAfter()),
			WalletVersion: walletVersion,
		},
	)
	if err != nil {
		return fmt.Errorf("build WalletBalanceChanged: %w", err)
	}

	if err := outbox.Append(ctx, tx, postgres.OutboxEvent{
		ID:            balanceID,
		AggregateID:   transaction.WalletID(),
		EventType:     events.TypeWalletBalanceChanged,
		Payload:       balancePayload,
		CorrelationID: correlationID,
		CausationID:   id,
		OccurredAt:    balanceOccurredAt,
	}); err != nil {
		return fmt.Errorf("append WalletBalanceChanged: %w", err)
	}

	return nil
}

func AppendRejected(
	ctx context.Context,
	tx *sql.Tx,
	outbox *postgres.OutboxRepository,
	transaction wager.Transaction,
	correlationID string,
) error {
	id, payload, occurredAt, err := events.Build(
		events.TypeWagerTransactionRejected,
		transaction.ID(),
		correlationID,
		"",
		events.WagerTransactionRejectedData{
			TransactionID: transaction.ID(),
			FailureCode:   transaction.FailureCode(),
		},
	)
	if err != nil {
		return fmt.Errorf("build WagerTransactionRejected: %w", err)
	}

	if err := outbox.Append(ctx, tx, postgres.OutboxEvent{
		ID:            id,
		AggregateID:   transaction.ID(),
		EventType:     events.TypeWagerTransactionRejected,
		Payload:       payload,
		CorrelationID: correlationID,
		OccurredAt:    occurredAt,
	}); err != nil {
		return fmt.Errorf("append WagerTransactionRejected: %w", err)
	}

	return nil
}

func AppendPendingReference(
	ctx context.Context,
	tx *sql.Tx,
	outbox *postgres.OutboxRepository,
	transaction wager.Transaction,
	correlationID string,
) error {
	id, payload, occurredAt, err := events.Build(
		events.TypeWagerTransactionPendingReference,
		transaction.ID(),
		correlationID,
		"",
		events.WagerTransactionPendingReferenceData{
			TransactionID:                  transaction.ID(),
			ReferenceExternalTransactionID: transaction.ReferenceExternalID(),
		},
	)
	if err != nil {
		return fmt.Errorf("build WagerTransactionPendingReference: %w", err)
	}

	if err := outbox.Append(ctx, tx, postgres.OutboxEvent{
		ID:            id,
		AggregateID:   transaction.ID(),
		EventType:     events.TypeWagerTransactionPendingReference,
		Payload:       payload,
		CorrelationID: correlationID,
		OccurredAt:    occurredAt,
	}); err != nil {
		return fmt.Errorf("append WagerTransactionPendingReference: %w", err)
	}

	return nil
}
