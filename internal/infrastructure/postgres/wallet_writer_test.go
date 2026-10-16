package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

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
		bytes[0:4],
		bytes[4:6],
		bytes[6:8],
		bytes[8:10],
		bytes[10:16],
	)
}

func setupWalletWriterTest(t *testing.T, walletID string, balance int64) (*sql.DB, context.Context) {
	t.Helper()

	databaseURL := pgtest.URL(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
	_, err = db.ExecContext(ctx, `
		INSERT INTO wallets (
			id, player_id, currency, balance_minor_units, version
		)
		VALUES ($1, $2, 'BRL', $3, 1)
	`, walletID, newTestID(t), balance)
	if err != nil {
		t.Fatalf("create test wallet: %v", err)
	}

	return db, ctx
}

func TestDebitWalletSuccess(t *testing.T) {
	walletID := newTestID(t)

	db, ctx := setupWalletWriterTest(t, walletID, 10000)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	defer tx.Rollback()

	balance, version, err := DebitWallet(ctx, tx, walletID, 3000, "BRL")
	if err != nil {
		t.Fatalf("DebitWallet: %v", err)
	}

	if balance != 7000 {
		t.Errorf("balance = %d, want 7000", balance)
	}
	if version != 2 {
		t.Errorf("version = %d, want 2", version)
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit transaction: %v", err)
	}

	var persistedBalance int64
	err = db.QueryRowContext(
		ctx,
		`SELECT balance_minor_units FROM wallets WHERE id = $1`,
		walletID,
	).Scan(&persistedBalance)
	if err != nil {
		t.Fatalf("read persisted balance: %v", err)
	}

	if persistedBalance != 7000 {
		t.Errorf("persisted balance = %d, want 7000", persistedBalance)
	}
}

func TestDebitWalletRejectsInsufficientBalance(t *testing.T) {
	walletID := newTestID(t)

	db, ctx := setupWalletWriterTest(t, walletID, 10000)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	defer tx.Rollback()

	_, _, err = DebitWallet(ctx, tx, walletID, 10001, "BRL")
	if !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("error = %v, want ErrInsufficientBalance", err)
	}

	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("rollback transaction: %v", err)
	}

	var balance int64
	var version int64
	err = db.QueryRowContext(
		ctx,
		`SELECT balance_minor_units, version FROM wallets WHERE id = $1`,
		walletID,
	).Scan(&balance, &version)
	if err != nil {
		t.Fatalf("read wallet: %v", err)
	}

	if balance != 10000 || version != 1 {
		t.Errorf("wallet changed after rejected debit: balance=%d version=%d", balance, version)
	}
}

func TestDebitWalletRollbackRestoresBalance(t *testing.T) {
	walletID := newTestID(t)

	db, ctx := setupWalletWriterTest(t, walletID, 10000)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}

	_, _, err = DebitWallet(ctx, tx, walletID, 3000, "BRL")
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("DebitWallet: %v", err)
	}

	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback transaction: %v", err)
	}

	var balance int64
	var version int64
	err = db.QueryRowContext(
		ctx,
		`SELECT balance_minor_units, version FROM wallets WHERE id = $1`,
		walletID,
	).Scan(&balance, &version)
	if err != nil {
		t.Fatalf("read wallet after rollback: %v", err)
	}

	if balance != 10000 || version != 1 {
		t.Errorf("rollback did not restore wallet: balance=%d version=%d", balance, version)
	}
}

func TestCreditWalletSuccess(t *testing.T) {
	walletID := newTestID(t)

	db, ctx := setupWalletWriterTest(t, walletID, 10000)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	defer tx.Rollback()

	balance, version, err := CreditWallet(ctx, tx, walletID, 3000, "BRL")
	if err != nil {
		t.Fatalf("CreditWallet: %v", err)
	}

	if balance != 13000 {
		t.Errorf("balance = %d, want 13000", balance)
	}
	if version != 2 {
		t.Errorf("version = %d, want 2", version)
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit transaction: %v", err)
	}

	var persistedBalance, persistedVersion int64
	err = db.QueryRowContext(
		ctx,
		`SELECT balance_minor_units, version FROM wallets WHERE id = $1`,
		walletID,
	).Scan(&persistedBalance, &persistedVersion)
	if err != nil {
		t.Fatalf("read persisted wallet: %v", err)
	}

	if persistedBalance != 13000 || persistedVersion != 2 {
		t.Errorf(
			"persisted wallet = balance %d, version %d; want balance 13000, version 2",
			persistedBalance,
			persistedVersion,
		)
	}
}

func TestCreditWalletRejectsCurrencyMismatch(t *testing.T) {
	walletID := newTestID(t)

	db, ctx := setupWalletWriterTest(t, walletID, 10000)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	defer tx.Rollback()

	_, _, err = CreditWallet(ctx, tx, walletID, 3000, "USD")
	if !errors.Is(err, ErrWalletCurrencyMismatch) {
		t.Fatalf("error = %v, want ErrWalletCurrencyMismatch", err)
	}
}

func TestCreditWalletRejectsBalanceOverflow(t *testing.T) {
	walletID := newTestID(t)

	maxInt64 := int64(^uint64(0) >> 1)
	db, ctx := setupWalletWriterTest(t, walletID, maxInt64-5)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	defer tx.Rollback()

	_, _, err = CreditWallet(ctx, tx, walletID, 10, "BRL")
	if !errors.Is(err, ErrWalletBalanceOverflow) {
		t.Fatalf("error = %v, want ErrWalletBalanceOverflow", err)
	}

	var balance, version int64
	err = db.QueryRowContext(
		ctx,
		`SELECT balance_minor_units, version FROM wallets WHERE id = $1`,
		walletID,
	).Scan(&balance, &version)
	if err != nil {
		t.Fatalf("read wallet: %v", err)
	}

	if balance != maxInt64-5 || version != 1 {
		t.Errorf("wallet changed after rejected credit: balance=%d version=%d", balance, version)
	}
}

func TestCreditWalletRejectsVersionOverflow(t *testing.T) {
	walletID := newTestID(t)

	maxInt64 := int64(^uint64(0) >> 1)
	db, ctx := setupWalletWriterTest(t, walletID, 10000)

	_, err := db.ExecContext(
		ctx,
		`UPDATE wallets SET version = $1 WHERE id = $2`,
		maxInt64,
		walletID,
	)
	if err != nil {
		t.Fatalf("set wallet version to maximum: %v", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	defer tx.Rollback()

	_, _, err = CreditWallet(ctx, tx, walletID, 100, "BRL")
	if !errors.Is(err, ErrWalletVersionOverflow) {
		t.Fatalf("error = %v, want ErrWalletVersionOverflow", err)
	}
}

func TestCreditWalletRollbackRestoresBalance(t *testing.T) {
	walletID := newTestID(t)

	db, ctx := setupWalletWriterTest(t, walletID, 10000)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}

	_, _, err = CreditWallet(ctx, tx, walletID, 3000, "BRL")
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("CreditWallet: %v", err)
	}

	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback transaction: %v", err)
	}

	var balance, version int64
	err = db.QueryRowContext(
		ctx,
		`SELECT balance_minor_units, version FROM wallets WHERE id = $1`,
		walletID,
	).Scan(&balance, &version)
	if err != nil {
		t.Fatalf("read wallet after rollback: %v", err)
	}

	if balance != 10000 || version != 1 {
		t.Errorf(
			"rollback did not restore wallet: balance=%d version=%d",
			balance,
			version,
		)
	}
}
