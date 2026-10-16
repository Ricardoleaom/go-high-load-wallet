package ledger

import (
	"errors"
	"testing"
	"time"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/money"
)

func mustMoney(t *testing.T, amount string) money.Money {
	t.Helper()
	m, err := money.New(amount, "BRL")
	if err != nil {
		t.Fatalf("money.New(%q): %v", amount, err)
	}
	return m
}

func TestNewDebitEntry(t *testing.T) {
	entry, err := New(
		"entry-1", "wallet-1", "tx-1",
		DirectionDebit,
		mustMoney(t, "25.00"),
		mustMoney(t, "100.00"),
		mustMoney(t, "75.00"),
		time.Now().UTC(),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if entry.Direction() != DirectionDebit {
		t.Errorf("direction = %v, want DEBIT", entry.Direction())
	}
}

func TestNewCreditEntry(t *testing.T) {
	_, err := New(
		"entry-1", "wallet-1", "tx-1",
		DirectionCredit,
		mustMoney(t, "25.00"),
		mustMoney(t, "100.00"),
		mustMoney(t, "125.00"),
		time.Now().UTC(),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
}

func TestNewRejectsBalanceMismatch(t *testing.T) {
	_, err := New(
		"entry-1", "wallet-1", "tx-1",
		DirectionDebit,
		mustMoney(t, "25.00"),
		mustMoney(t, "100.00"),
		mustMoney(t, "80.00"),
		time.Now().UTC(),
	)
	if !errors.Is(err, ErrBalanceMismatch) {
		t.Fatalf("error = %v, want ErrBalanceMismatch", err)
	}
}

func TestNewRejectsZeroAmount(t *testing.T) {
	_, err := New(
		"entry-1", "wallet-1", "tx-1",
		DirectionCredit,
		mustMoney(t, "0.00"),
		mustMoney(t, "100.00"),
		mustMoney(t, "100.00"),
		time.Now().UTC(),
	)
	if !errors.Is(err, ErrInvalidEntry) {
		t.Fatalf("error = %v, want ErrInvalidEntry", err)
	}
}

func TestNewRejectsMissingIdentifiers(t *testing.T) {
	_, err := New(
		"", "wallet-1", "tx-1",
		DirectionDebit,
		mustMoney(t, "25.00"),
		mustMoney(t, "100.00"),
		mustMoney(t, "75.00"),
		time.Now().UTC(),
	)
	if !errors.Is(err, ErrInvalidEntry) {
		t.Fatalf("error = %v, want ErrInvalidEntry", err)
	}
}
