package wallet

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/money"
)

func mustMoney(t *testing.T, amount, currency string) money.Money {
	t.Helper()

	value, err := money.New(amount, currency)
	if err != nil {
		t.Fatalf("could not create money: %v", err)
	}

	return value
}

func TestNewWallet(t *testing.T) {
	initialBalance := mustMoney(t, "100.00", "BRL")

	w, err := New("wallet-1", "player-1", initialBalance)
	if err != nil {
		t.Fatal(err)
	}

	if w.ID() != "wallet-1" {
		t.Errorf("unexpected wallet ID: %s", w.ID())
	}
	if w.PlayerID() != "player-1" {
		t.Errorf("unexpected player ID: %s", w.PlayerID())
	}
	if w.Balance().AmountInMinorUnits() != 10000 {
		t.Errorf("unexpected balance: %s", w.Balance())
	}
	if w.Version() != 1 {
		t.Errorf("expected version 1, got %d", w.Version())
	}
	if w.CreatedAt().IsZero() || w.UpdatedAt().IsZero() {
		t.Errorf("expected creation and update timestamps")
	}
}
func TestNewWalletRejectsInvalidData(t *testing.T) {
	balance := mustMoney(t, "100.00", "BRL")

	tests := []struct {
		name     string
		id       string
		playerID string
		balance  money.Money
	}{
		{
			name:     "missing wallet ID",
			playerID: "player-1",
			balance:  balance,
		},
		{
			name:     "missing player ID",
			playerID: "wallet-1",
			balance:  balance,
		},
		{
			name:     "uninitialized balance",
			id:       "wallet-1",
			playerID: "player-1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.id, tt.playerID, tt.balance)
			if !errors.Is(err, ErrInvalidWallet) {
				t.Fatalf("expected invalid wallet, got %v", err)
			}
		})
	}
}

func TestCredit(t *testing.T) {
	w, err := New("wallet-1", "player-1",
		mustMoney(t, "100.00", "BRL"))
	if err != nil {
		t.Fatal(err)
	}

	err = w.Credit(mustMoney(t, "25.00", "BRL"))
	if err != nil {
		t.Fatal(err)
	}

	if got := w.Balance().AmountInMinorUnits(); got != 12500 {
		t.Errorf("expected 12500, got %d", got)
	}
	if w.Version() != 2 {
		t.Errorf("expected version 2, got %d", w.Version())
	}
}

func TestDebit(t *testing.T) {
	w, err := New("wallet-1", "player-1",
		mustMoney(t, "100.00", "BRL"))
	if err != nil {
		t.Fatal(err)
	}

	err = w.Debit(mustMoney(t, "25.00", "BRL"))
	if err != nil {
		t.Fatal(err)
	}

	if got := w.Balance().AmountInMinorUnits(); got != 7500 {
		t.Errorf("expected 7500, got %d", got)
	}
	if w.Version() != 2 {
		t.Errorf("expected version 2, got %d", w.Version())
	}
}

func TestDebitRejectsInsufficientBalanceWithoutChangingWallet(t *testing.T) {
	w, err := New("wallet-1", "player-1",
		mustMoney(t, "100.00", "BRL"))
	if err != nil {
		t.Fatal(err)
	}

	previousBalance := w.Balance()
	previousVersion := w.Version()
	previousUpdateAt := w.UpdatedAt()

	err = w.Debit(mustMoney(t, "100.01", "BRL"))
	if !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("expected insufficient balance, got %v", err)
	}

	if w.Balance() != previousBalance {
		t.Error("balance changed after rejected debit")
	}
	if w.Version() != previousVersion {
		t.Error("version changed after rejected debit")
	}
	if !w.UpdatedAt().Equal(previousUpdateAt) {
		t.Error("updatedAt change after rejected debit")
	}
}

func TestMovementRejectsInvalidAmountAndCurrency(t *testing.T) {
	w, err := New("wallet-1", "player-1",
		mustMoney(t, "100.00", "BRL"))
	if err != nil {
		t.Fatal(err)
	}

	zero := mustMoney(t, "0.00", "BRL")
	if err := w.Credit(zero); !errors.Is(err, ErrInvalidAmount) {
		t.Errorf("expected invalid amount, got %v", err)
	}

	usd := mustMoney(t, "10.00", "USD")
	if err := w.Debit(usd); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("expected currency mismatch, got %v", err)
	}

	if w.Balance().AmountInMinorUnits() != 10000 || w.Version() != 1 {
		t.Error("wallet changed after invalid movement")
	}
}

func TestZeroBalanceWallet(t *testing.T) {
	w, err := New("wallet-1", "player-1",
		mustMoney(t, "0.00", "BRL"))
	if err != nil {
		t.Fatal(err)
	}

	if err := w.Debit(mustMoney(t, "0.01", "BRL")); !errors.Is(err, ErrInsufficientBalance) {
		t.Errorf("expected insufficient balance, got %v", err)
	}

	if w.Version() != 1 {
		t.Errorf("expected version 1, got %d", w.Version())
	}
}

func TestRehydrateDoesNotApplyMovements(t *testing.T) {
	createdAt := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	updatedAt := createdAt.Add(time.Hour)
	balance := mustMoney(t, "75.00", "BRL")

	w, err := Rehydrate(
		"wallet-1",
		"player-1",
		balance,
		4,
		createdAt,
		updatedAt,
	)
	if err != nil {
		t.Fatal(err)
	}

	if w.Balance().AmountInMinorUnits() != 7500 {
		t.Errorf("unexpected balance: %s", w.Balance())
	}
	if w.Version() != 4 {
		t.Errorf("expected version 4, got %d", w.Version())
	}
	if !w.CreatedAt().Equal(createdAt) || !w.UpdatedAt().Equal(updatedAt) {
		t.Error("timestamps were not restored")
	}
}

func TestRehydrateRejectsInvalidData(t *testing.T) {
	createdAt := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	balance := mustMoney(t, "75.00", "BRL")

	tests := []struct {
		name    string
		id      string
		version int64
		created time.Time
		updated time.Time
	}{
		{"missing ID", "", 1, createdAt, createdAt},
		{"invalid version", "wallet-1", 0, createdAt, createdAt},
		{"missing creation time", "wallet-1", 1, time.Time{}, createdAt},
		{"update before creation", "wallet-1", 1, createdAt, createdAt.Add(-time.Second)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Rehydrate(
				tt.id, "player-1", balance, tt.version, tt.created, tt.updated,
			)
			if !errors.Is(err, ErrInvalidWallet) {
				t.Fatalf("expected invalid wallet, got %v", err)
			}
		})
	}
}

func TestCreditRejectsVersionOverflow(t *testing.T) {
	zero, err := money.Zero("BRL")
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()

	w, err := Rehydrate(
		"wallet-1",
		"player-1",
		zero,
		math.MaxInt64,
		now,
		now,
	)
	if err != nil {
		t.Fatal(err)
	}

	one, err := money.New("1.00", "BRL")
	if err != nil {
		t.Fatal(err)
	}

	err = w.Credit(one)
	if !errors.Is(err, ErrVersionOverflow) {
		t.Fatalf("expected ErrVersionOverflow, got %v", err)
	}

	if w.Balance().AmountInMinorUnits() != 0 {
		t.Fatal("balance changed after version overflow")
	}
}
