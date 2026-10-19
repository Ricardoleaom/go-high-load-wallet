package events

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/money"
)

const (
	TypeWagerTransactionProcessed        = "WagerTransactionProcessed"
	TypeWagerTransactionRejected         = "WagerTransactionRejected"
	TypeWalletBalanceChanged             = "WalletBalanceChanged"
	TypeWagerTransactionPendingReference = "WagerTransactionPendingReference"
)

const envelopeVersion = 1

type Envelope struct {
	EventID       string          `json:"eventId"`
	EventType     string          `json:"eventType"`
	AggregateID   string          `json:"aggregateId"`
	CorrelationID string          `json:"correlationId"`
	CausationID   string          `json:"causationId,omitempty"`
	OccurredAt    string          `json:"occurredAt"`
	Version       int             `json:"version"`
	Data          json.RawMessage `json:"data"`
}

type MoneyDTO struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

func DTO(m money.Money) MoneyDTO {
	return MoneyDTO{Amount: m.DecimalString(), Currency: m.Currency()}
}

type WagerTransactionProcessedData struct {
	TransactionID string   `json:"transactionId"`
	WalletID      string   `json:"walletId"`
	Kind          string   `json:"kind"`
	Money         MoneyDTO `json:"money"`
}

type WagerTransactionRejectedData struct {
	TransactionID string `json:"transactionId"`
	FailureCode   string `json:"failureCode"`
}

type WalletBalanceChangedData struct {
	WalletID      string   `json:"walletId"`
	TransactionID string   `json:"transactionId"`
	Direction     string   `json:"direction"`
	Money         MoneyDTO `json:"money"`
	BalanceBefore MoneyDTO `json:"balanceBefore"`
	BalanceAfter  MoneyDTO `json:"balanceAfter"`
	WalletVersion int64    `json:"walletVersion"`
}

type WagerTransactionPendingReferenceData struct {
	TransactionID                  string `json:"transactionId"`
	ReferenceExternalTransactionID string `json:"referenceExternalTransactionId"`
}

// Build serializes an event envelope ready to be stored in the outbox.
func Build(
	eventType string,
	aggregateID string,
	correlationID string,
	causationID string,
	data any,
) (id string, payload []byte, occurredAt time.Time, err error) {
	rawData, err := json.Marshal(data)
	if err != nil {
		return "", nil, time.Time{}, fmt.Errorf("marshal event data: %w", err)
	}

	eventID, err := uuid.NewV7()
	if err != nil {
		return "", nil, time.Time{}, fmt.Errorf("generate event ID: %w", err)
	}
	id = eventID.String()
	occurredAt = time.Now().UTC()

	envelope := Envelope{
		EventID:       id,
		EventType:     eventType,
		AggregateID:   aggregateID,
		CorrelationID: correlationID,
		CausationID:   causationID,
		OccurredAt:    occurredAt.Format(time.RFC3339Nano),
		Version:       envelopeVersion,
		Data:          rawData,
	}

	payload, err = json.Marshal(envelope)
	if err != nil {
		return "", nil, time.Time{}, fmt.Errorf("marshal event envelope: %w", err)
	}

	return id, payload, occurredAt, nil
}
