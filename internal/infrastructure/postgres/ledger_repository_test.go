package postgres

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/ledger"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/money"
)

func setupLedgerTest(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL is not set; skipping PostgreSQL integration test")
	}

	ctx, cancel := context.WithTimeout(
		context.Background(),
		10*time.Second,
	)
	t.Cleanup(cancel)

	db, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open PostgreSQL: %v", err)
	}

	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close PostgreSQL: %v", err)
		}
	})

	return db, ctx
}

func TestLedgerRepositoryAppendAndGetByTransactionID(t *testing.T) {
	db, ctx := setupLedgerTest(t)
	repository := NewLedgerRepository(db)

	walletID := "51111111-1111-4111-8111-111111111111"
	transactionID := "52222222-2222-4222-8222-222222222222"
	entryID := "53333333-3333-4333-8333-333333333333"
	playerID := "54444444-4444-4444-8444-444444444444"

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}

	// Always roll back test data, including the append-only ledger entry.
	t.Cleanup(func() {
		if err := tx.Rollback(); err != nil &&
			!errors.Is(err, sql.ErrTxDone) {
			t.Errorf("rollback test transaction: %v", err)
		}
	})

	_, err = tx.ExecContext(ctx, `
		INSERT INTO wallets (
			id, player_id, currency, balance_minor_units, version
		)
		VALUES ($1, $2, 'BRL', 7000, 2)
	`, walletID, playerID)
	if err != nil {
		t.Fatalf("insert test wallet: %v", err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO transactions (
			id, provider_id, external_id, idempotency_key, payload_hash,
			wallet_id, player_id, round_id, game_id, kind,
			amount_minor_units, status
		)
		VALUES (
			$1, 'test-provider', $2, $3, 'test-hash',
			$4, $5, 'round-1', 'game-1', 'BET', 1000, 'PROCESSED'
		)
	`, transactionID, transactionID, transactionID, walletID, playerID)
	if err != nil {
		t.Fatalf("insert test transaction: %v", err)
	}

	before, err := money.New("8000.00", "BRL")
	if err != nil {
		t.Fatalf("money.New before: %v", err)
	}
	amount, err := money.New("1000.00", "BRL")
	if err != nil {
		t.Fatalf("money.New amount: %v", err)
	}
	after, err := money.New("7000.00", "BRL")
	if err != nil {
		t.Fatalf("money.New after: %v", err)
	}

	entry, err := ledger.New(
		entryID, walletID, transactionID,
		ledger.DirectionDebit,
		amount, before, after,
		time.Now().UTC(),
	)
	if err != nil {
		t.Fatalf("ledger.New: %v", err)
	}

	if err := repository.Append(ctx, tx, entry); err != nil {
		t.Fatalf("Append: %v", err)
	}

	var gotID, gotWalletID, gotTransactionID, gotDirection string
	var gotBalanceAfter int64
	var gotCreatedAt time.Time

	err = tx.QueryRowContext(ctx, `
		SELECT
			id,
			wallet_id,
			transaction_id,
			direction,
			balance_after_minor_units,
			created_at
		FROM ledger_entries
		WHERE transaction_id = $1
	`, transactionID).Scan(
		&gotID,
		&gotWalletID,
		&gotTransactionID,
		&gotDirection,
		&gotBalanceAfter,
		&gotCreatedAt,
	)
	if err != nil {
		t.Fatalf("read appended ledger entry: %v", err)
	}

	if gotID != entryID {
		t.Errorf("ID = %q, want %q", gotID, entryID)
	}
	if gotWalletID != walletID {
		t.Errorf("WalletID = %q, want %q", gotWalletID, walletID)
	}
	if gotTransactionID != transactionID {
		t.Errorf("TransactionID = %q, want %q", gotTransactionID, transactionID)
	}
	if gotDirection != "DEBIT" {
		t.Errorf("direction = %q, want DEBIT", gotDirection)
	}
	if gotBalanceAfter != 700000 {
		t.Errorf("balance after = %d, want 700000", gotBalanceAfter)
	}
	if gotCreatedAt.IsZero() {
		t.Error("expected created_at to be populated")
	}
}

// TestReconciliationSnapshot commits real data, since the snapshot reads
// outside any caller transaction. Fresh UUIDs keep reruns independent; the
// ledger is append-only, so these rows stay behind like other integration
// test data.
func TestReconciliationSnapshot(t *testing.T) {
	db, ctx := setupLedgerTest(t)
	repository := NewLedgerRepository(db)

	walletID := uuid.NewString()
	openingID := uuid.NewString()
	betID := uuid.NewString()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO wallets (id, player_id, currency, balance_minor_units, version)
		VALUES ($1, $1, 'BRL', 7500, 3)
	`, walletID); err != nil {
		t.Fatalf("insert wallet: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO transactions (id, wallet_id, player_id, kind, amount_minor_units, status)
		VALUES ($1, $2, $2, 'OPENING', 10000, 'PROCESSED')
	`, openingID, walletID); err != nil {
		t.Fatalf("insert opening: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO transactions (
			id, provider_id, external_id, idempotency_key, payload_hash,
			wallet_id, player_id, round_id, game_id, kind, amount_minor_units, status
		)
		VALUES ($1, 'test-provider', $2, $3, 'test-hash', $4, $4, 'round-1', 'game-1', 'BET', 2500, 'PROCESSED')
	`, betID, betID, betID, walletID); err != nil {
		t.Fatalf("insert bet: %v", err)
	}

	appendEntry := func(transactionID string, direction ledger.Direction, amount, before, after string) {
		t.Helper()
		entry, err := ledger.New(
			uuid.NewString(), walletID, transactionID, direction,
			mustLedgerMoney(t, amount), mustLedgerMoney(t, before), mustLedgerMoney(t, after),
			time.Now().UTC(),
		)
		if err != nil {
			t.Fatalf("ledger.New: %v", err)
		}
		if err := repository.Append(ctx, tx, entry); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	appendEntry(openingID, ledger.DirectionCredit, "100.00", "0.00", "100.00")
	appendEntry(betID, ledger.DirectionDebit, "25.00", "100.00", "75.00")

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	snapshot, err := repository.ReconciliationSnapshot(ctx, walletID)
	if err != nil {
		t.Fatalf("ReconciliationSnapshot: %v", err)
	}
	if snapshot.StoredBalance.AmountInMinorUnits() != 7500 || snapshot.CalculatedBalance.AmountInMinorUnits() != 7500 {
		t.Errorf("stored = %s, calculated = %s, want both 75.00", snapshot.StoredBalance, snapshot.CalculatedBalance)
	}
	if snapshot.CheckedEntries != 2 {
		t.Errorf("checked entries = %d, want 2", snapshot.CheckedEntries)
	}

	// A balance edited behind the ledger's back must be reported, not hidden.
	if _, err := db.ExecContext(ctx, `UPDATE wallets SET balance_minor_units = 8000 WHERE id = $1`, walletID); err != nil {
		t.Fatalf("tamper wallet balance: %v", err)
	}
	snapshot, err = repository.ReconciliationSnapshot(ctx, walletID)
	if err != nil {
		t.Fatalf("ReconciliationSnapshot after tamper: %v", err)
	}
	if snapshot.StoredBalance.AmountInMinorUnits() != 8000 || snapshot.CalculatedBalance.AmountInMinorUnits() != 7500 {
		t.Errorf("stored = %s, calculated = %s, want 80.00 and 75.00", snapshot.StoredBalance, snapshot.CalculatedBalance)
	}
}

func TestReconciliationSnapshotWalletNotFound(t *testing.T) {
	db, ctx := setupLedgerTest(t)

	_, err := NewLedgerRepository(db).ReconciliationSnapshot(ctx, uuid.NewString())
	if !errors.Is(err, ErrWalletNotFound) {
		t.Fatalf("error = %v, want ErrWalletNotFound", err)
	}
}

func mustLedgerMoney(t *testing.T, amount string) money.Money {
	t.Helper()
	m, err := money.New(amount, "BRL")
	if err != nil {
		t.Fatalf("money.New(%q): %v", amount, err)
	}
	return m
}

func TestLedgerRepositoryGetByTransactionIDNotFound(t *testing.T) {
	db, ctx := setupLedgerTest(t)
	repository := NewLedgerRepository(db)

	_, err := repository.GetByTransactionID(
		ctx,
		"49999999-9999-4999-8999-999999999999",
	)

	if !errors.Is(err, ErrLedgerEntryNotFound) {
		t.Fatalf(
			"error = %v, want ErrLedgerEntryNotFound",
			err,
		)
	}
}
