package wallet

import (
	"errors"
	"math"
	"time"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/money"
)

var (
	ErrInvalidWallet       = errors.New("invalid wallet")
	ErrInvalidAmount       = errors.New("amount must be greater than zero")
	ErrInsufficientBalance = errors.New("insufficient wallet balance")
	ErrCurrencyMismatch    = errors.New("wallet currency mismatch")
	ErrVersionOverflow     = errors.New("wallet version overflow")
)

type Wallet struct {
	id        string
	playerID  string
	balance   money.Money
	version   int64
	createdAt time.Time
	updatedAt time.Time
}

// New creates a wallet with version 1.
func New(id, playerID string, initialBalance money.Money) (Wallet, error) {
	if id == "" || playerID == "" ||
		initialBalance.Currency() == "" ||
		initialBalance.AmountInMinorUnits() < 0 {
		return Wallet{}, ErrInvalidWallet
	}

	now := time.Now().UTC()

	return Wallet{
		id:        id,
		playerID:  playerID,
		balance:   initialBalance,
		version:   1,
		createdAt: now,
		updatedAt: now,
	}, nil
}

// Rehydrate restores a wallet from persisted data without applying movements.
func Rehydrate(
	id, playerID string,
	balance money.Money,
	version int64,
	createdAt, updatedAt time.Time,
) (Wallet, error) {
	if id == "" || playerID == "" ||
		balance.Currency() == "" ||
		balance.AmountInMinorUnits() < 0 ||
		version < 1 ||
		createdAt.IsZero() ||
		updatedAt.IsZero() ||
		updatedAt.Before(createdAt) {
		return Wallet{}, ErrInvalidWallet
	}

	return Wallet{
		id:        id,
		playerID:  playerID,
		balance:   balance,
		version:   version,
		createdAt: createdAt,
		updatedAt: updatedAt,
	}, nil
}

func (w Wallet) ID() string {
	return w.id
}

func (w Wallet) PlayerID() string {
	return w.playerID
}

func (w Wallet) Balance() money.Money {
	return w.balance
}

func (w Wallet) Version() int64 {
	return w.version
}

func (w Wallet) CreatedAt() time.Time {
	return w.createdAt
}

func (w Wallet) UpdatedAt() time.Time {
	return w.updatedAt
}

func (w *Wallet) Credit(amount money.Money) error {
	if err := w.validateMovement(amount); err != nil {
		return err
	}

	if err := w.validateVersionIncrement(); err != nil {
		return err
	}

	newBalance, err := w.balance.Add(amount)
	if err != nil {
		return err
	}

	w.balance = newBalance
	w.version++
	w.updatedAt = time.Now().UTC()

	return nil
}

func (w *Wallet) Debit(amount money.Money) error {
	if err := w.validateMovement(amount); err != nil {
		return err
	}

	if err := w.validateVersionIncrement(); err != nil {
		return err
	}

	newBalance, err := w.balance.Subtract(amount)
	if err != nil {
		return err
	}

	if newBalance.AmountInMinorUnits() < 0 {
		return ErrInsufficientBalance
	}

	w.balance = newBalance
	w.version++
	w.updatedAt = time.Now().UTC()

	return nil
}

func (w Wallet) validateMovement(amount money.Money) error {
	if amount.AmountInMinorUnits() <= 0 {
		return ErrInvalidAmount
	}

	if amount.Currency() == "" {
		return money.ErrInvalidCurrency
	}

	if amount.Currency() != w.balance.Currency() {
		return ErrCurrencyMismatch
	}

	return nil
}

func (w Wallet) validateVersionIncrement() error {
	if w.version == math.MaxInt64 {
		return ErrVersionOverflow
	}

	return nil
}
