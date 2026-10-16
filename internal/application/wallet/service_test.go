package wallet

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/money"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/infrastructure/postgres"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/testsupport/pgtest"
)

func newTestID(t *testing.T) string {
	t.Helper()

	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		t.Fatalf("generate test ID: %v", err)
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80

	return fmt.Sprintf(
		"%08x-%04x-%04x-%04x-%012x",
		bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16],
	)
}

func newTestService(t *testing.T) (*Service, *sql.DB, context.Context) {
	t.Helper()

	databaseURL := pgtest.URL(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	db, err := postgres.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open PostgreSQL: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close PostgreSQL: %v", err)
		}
	})

	service := NewService(
		db,
		postgres.NewWalletRepository(db),
		postgres.NewTransactionRepository(db),
		postgres.NewLedgerRepository(db),
		postgres.NewOutboxRepository(db),
	)

	return service, db, ctx
}

func mustMoney(t *testing.T, amount string) money.Money {
	t.Helper()
	m, err := money.New(amount, "BRL")
	if err != nil {
		t.Fatalf("money.New(%q): %v", amount, err)
	}
	return m
}

func TestOpenWithPositiveBalanceCreatesOpeningAndLedger(t *testing.T) {
	service, db, ctx := newTestService(t)

	playerID := newTestID(t)

	w, err := service.Open(ctx, OpenInput{
		PlayerID:       playerID,
		InitialBalance: mustMoney(t, "1000.00"),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if w.Balance().AmountInMinorUnits() != 100000 {
		t.Errorf("balance = %d, want 100000", w.Balance().AmountInMinorUnits())
	}
	if w.Version() != 1 {
		t.Errorf("version = %d, want 1", w.Version())
	}

	var txCount int
	err = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM transactions WHERE wallet_id = $1 AND kind = 'OPENING'`,
		w.ID(),
	).Scan(&txCount)
	if err != nil {
		t.Fatalf("count opening transactions: %v", err)
	}
	if txCount != 1 {
		t.Errorf("opening transactions = %d, want 1", txCount)
	}

	var ledgerCount int
	err = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ledger_entries WHERE wallet_id = $1`,
		w.ID(),
	).Scan(&ledgerCount)
	if err != nil {
		t.Fatalf("count ledger entries: %v", err)
	}
	if ledgerCount != 1 {
		t.Errorf("ledger entries = %d, want 1", ledgerCount)
	}

	var outboxCount int
	err = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM outbox_events WHERE aggregate_id = $1 OR aggregate_id = $2`,
		w.ID(), w.ID(),
	).Scan(&outboxCount)
	if err != nil {
		t.Fatalf("count outbox events: %v", err)
	}
	if outboxCount < 1 {
		t.Errorf("outbox events = %d, want at least 1", outboxCount)
	}
}

func TestOpenWithZeroBalanceCreatesOnlyTheWallet(t *testing.T) {
	service, db, ctx := newTestService(t)

	playerID := newTestID(t)

	w, err := service.Open(ctx, OpenInput{
		PlayerID:       playerID,
		InitialBalance: mustMoney(t, "0.00"),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	var txCount int
	err = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM transactions WHERE wallet_id = $1`,
		w.ID(),
	).Scan(&txCount)
	if err != nil {
		t.Fatalf("count transactions: %v", err)
	}
	if txCount != 0 {
		t.Errorf("transactions = %d, want 0 for zero-balance opening", txCount)
	}
}

func TestOpenRejectsDuplicatePlayerAndCurrency(t *testing.T) {
	service, _, ctx := newTestService(t)

	playerID := newTestID(t)

	if _, err := service.Open(ctx, OpenInput{
		PlayerID:       playerID,
		InitialBalance: mustMoney(t, "10.00"),
	}); err != nil {
		t.Fatalf("first Open: %v", err)
	}

	_, err := service.Open(ctx, OpenInput{
		PlayerID:       playerID,
		InitialBalance: mustMoney(t, "20.00"),
	})
	if !errors.Is(err, ErrWalletConflict) {
		t.Fatalf("error = %v, want ErrWalletConflict", err)
	}
}
