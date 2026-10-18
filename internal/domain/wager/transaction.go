package wager

import (
	"errors"
	"time"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/money"
)

var (
	ErrInvalidTransaction = errors.New("invalid wager transaction")
	ErrInvalidTransition  = errors.New("invalid transaction state transition")
	ErrInvalidAmount      = errors.New("invalid transaction amount")
)

type Kind string

const (
	KindOpening  Kind = "OPENING"
	KindBet      Kind = "BET"
	KindWin      Kind = "WIN"
	KindLoss     Kind = "LOSS"
	KindRefund   Kind = "REFUND"
	KindRollback Kind = "ROLLBACK"
)

type Status string

const (
	StatusPending          Status = "PENDING"
	StatusPendingReference Status = "PENDING_REFERENCE"
	StatusProcessed        Status = "PROCESSED"
	StatusRejected         Status = "REJECTED"
	StatusFailed           Status = "FAILED"
)

type Transaction struct {
	id                  string
	providerID          string
	externalID          string
	idempotencyKey      string
	payloadHash         string
	walletID            string
	playerID            string
	roundID             string
	gameID              string
	kind                Kind
	amount              money.Money
	referenceExternalID string
	resolvedReferenceID string
	status              Status
	failureCode         string
	createdAt           time.Time
	updatedAt           time.Time
}

type Input struct {
	ID                  string
	ProviderID          string
	ExternalID          string
	IdempotencyKey      string
	PayloadHash         string
	WalletID            string
	PlayerID            string
	RoundID             string
	GameID              string
	Kind                Kind
	Amount              money.Money
	ReferenceExternalID string
}

// New creates a transaction in PENDING state. OPENING is reserved for
// internal wallet creation and does not carry provider/external metadata.
func New(input Input) (Transaction, error) {
	if input.ID == "" ||
		input.WalletID == "" ||
		input.PlayerID == "" ||
		input.Amount.Currency() == "" {
		return Transaction{}, ErrInvalidTransaction
	}

	if input.Kind != KindOpening {
		if input.ProviderID == "" ||
			input.ExternalID == "" ||
			input.IdempotencyKey == "" ||
			input.PayloadHash == "" ||
			input.RoundID == "" ||
			input.GameID == "" {
			return Transaction{}, ErrInvalidTransaction
		}
	}

	if err := validateOperation(input.Kind, input.Amount, input.ReferenceExternalID); err != nil {
		return Transaction{}, err
	}

	now := time.Now().UTC()

	return Transaction{
		id:                  input.ID,
		providerID:          input.ProviderID,
		externalID:          input.ExternalID,
		idempotencyKey:      input.IdempotencyKey,
		payloadHash:         input.PayloadHash,
		walletID:            input.WalletID,
		playerID:            input.PlayerID,
		roundID:             input.RoundID,
		gameID:              input.GameID,
		kind:                input.Kind,
		amount:              input.Amount,
		referenceExternalID: input.ReferenceExternalID,
		status:              StatusPending,
		createdAt:           now,
		updatedAt:           now,
	}, nil
}

func validateOperation(kind Kind, amount money.Money, referenceExternalID string) error {
	units := amount.AmountInMinorUnits()

	switch kind {
	case KindOpening, KindBet, KindWin:
		if units <= 0 {
			return ErrInvalidAmount
		}
	case KindLoss:
		if units != 0 {
			return ErrInvalidAmount
		}
	case KindRefund, KindRollback:
		if units <= 0 || referenceExternalID == "" {
			return ErrInvalidAmount
		}
	default:
		return ErrInvalidTransaction
	}

	return nil
}

func (t Transaction) ID() string {
	return t.id
}

func (t Transaction) ProviderID() string {
	return t.providerID
}

func (t Transaction) ExternalID() string {
	return t.externalID
}

func (t Transaction) WalletID() string {
	return t.walletID
}

func (t Transaction) PlayerID() string {
	return t.playerID
}

func (t Transaction) RoundID() string {
	return t.roundID
}

func (t Transaction) GameID() string {
	return t.gameID
}

// Known reports whether k is one of the kinds the service understands;
// callers use it to keep metric labels bounded.
func (k Kind) Known() bool {
	switch k {
	case KindOpening, KindBet, KindWin, KindLoss, KindRefund, KindRollback:
		return true
	default:
		return false
	}
}

func (t Transaction) Kind() Kind {
	return t.kind
}

func (t Transaction) Amount() money.Money {
	return t.amount
}

func (t Transaction) ReferenceExternalID() string {
	return t.referenceExternalID
}

func (t Transaction) ResolvedReferenceID() string {
	return t.resolvedReferenceID
}

// ResolveReference records the internal ID of the transaction this one
// references, once resolved. It does not change status.
func (t *Transaction) ResolveReference(internalID string) error {
	if !t.canTransition() || internalID == "" {
		return ErrInvalidTransition
	}

	t.resolvedReferenceID = internalID
	t.updatedAt = time.Now().UTC()
	return nil
}

func (t Transaction) Status() Status {
	return t.status
}

func (t Transaction) FailureCode() string {
	return t.failureCode
}

func (t Transaction) CreatedAt() time.Time {
	return t.createdAt
}

func (t Transaction) UpdatedAt() time.Time {
	return t.updatedAt
}

func (t *Transaction) MarkPendingReference() error {
	if !t.canTransition() {
		return ErrInvalidTransition
	}

	t.status = StatusPendingReference
	t.updatedAt = time.Now().UTC()
	return nil
}

func (t *Transaction) MarkProcessed() error {
	if !t.canTransition() {
		return ErrInvalidTransition
	}

	t.status = StatusProcessed
	t.updatedAt = time.Now().UTC()
	return nil
}

func (t *Transaction) Reject(failureCode string) error {
	if !t.canTransition() || failureCode == "" {
		return ErrInvalidTransition
	}

	t.status = StatusRejected
	t.failureCode = failureCode
	t.updatedAt = time.Now().UTC()
	return nil
}

func (t *Transaction) Fail(failureCode string) error {
	if !t.canTransition() || failureCode == "" {
		return ErrInvalidTransition
	}

	t.status = StatusFailed
	t.failureCode = failureCode
	t.updatedAt = time.Now().UTC()
	return nil
}

func (t Transaction) canTransition() bool {
	return t.status == StatusPending ||
		t.status == StatusPendingReference
}

func (t Transaction) IdempotencyKey() string {
	return t.idempotencyKey
}

func (t Transaction) PayloadHash() string {
	return t.payloadHash
}

func Rehydrate(
	input Input,
	status Status,
	failureCode string,
	resolvedReferenceID string,
	createdAt time.Time,
	updatedAt time.Time,
) (Transaction, error) {
	if createdAt.IsZero() || updatedAt.IsZero() || updatedAt.Before(createdAt) {
		return Transaction{}, ErrInvalidTransaction
	}

	transaction, err := New(input)
	if err != nil {
		return Transaction{}, err
	}

	transaction.resolvedReferenceID = resolvedReferenceID

	switch status {
	case StatusPending, StatusPendingReference, StatusProcessed:
		if failureCode != "" {
			return Transaction{}, ErrInvalidTransaction
		}
	case StatusRejected, StatusFailed:
		if failureCode == "" {
			return Transaction{}, ErrInvalidTransaction
		}
	default:
		return Transaction{}, ErrInvalidTransaction
	}

	transaction.status = status
	transaction.failureCode = failureCode
	transaction.createdAt = createdAt
	transaction.updatedAt = updatedAt

	return transaction, nil
}
