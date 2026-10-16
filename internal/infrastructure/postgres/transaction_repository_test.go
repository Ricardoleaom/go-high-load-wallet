package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/money"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/wager"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/testsupport/pgtest"
)

func newTransactionTestID(t *testing.T) string {
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

func setupTransactionRepositoryTest(t *testing.T) (
	*sql.DB,
	context.Context,
	*TransactionRepository,
	*sql.Tx,
	string,
) {
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

	walletID := newTransactionTestID(t)
	playerID := newTransactionTestID(t)

	_, err = db.ExecContext(ctx, `
		INSERT INTO wallets (
			id, player_id, currency, balance_minor_units, version
		)
		VALUES ($1, $2, 'BRL', 10000, 1)
	`, walletID, playerID)
	if err != nil {
		t.Fatalf("create test wallet: %v", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin database transaction: %v", err)
	}

	t.Cleanup(func() {
		_ = tx.Rollback()

		// Remove only test transactions that have no ledger entries.
		_, err := db.ExecContext(
			context.Background(),
			`DELETE FROM transactions
			 WHERE wallet_id = $1
			   AND NOT EXISTS (
			       SELECT 1 FROM ledger_entries le
			       WHERE le.transaction_id = transactions.id
			   )`,
			walletID,
		)
		if err != nil {
			t.Errorf("clean up test transactions: %v", err)
		}

		_, err = db.ExecContext(
			context.Background(),
			`DELETE FROM wallets WHERE id = $1`, walletID,
		)
		if err != nil {
			t.Errorf("clean up test wallet: %v", err)
		}
	})

	return db, ctx, NewTransactionRepository(db), tx, walletID
}

func newTestWager(t *testing.T, walletID string) wager.Transaction {
	t.Helper()

	amount, err := money.New("10.00", "BRL")
	if err != nil {
		t.Fatalf("create test amount: %v", err)
	}

	transaction, err := wager.New(wager.Input{
		ID:             newTransactionTestID(t),
		ProviderID:     "test-provider-" + newTransactionTestID(t),
		ExternalID:     newTransactionTestID(t),
		IdempotencyKey: newTransactionTestID(t),
		PayloadHash:    "test-payload-hash",
		WalletID:       walletID,
		PlayerID:       newTransactionTestID(t),
		RoundID:        "round-test",
		GameID:         "game-test",
		Kind:           wager.KindBet,
		Amount:         amount,
	})
	if err != nil {
		t.Fatalf("create domain transaction: %v", err)
	}

	return transaction
}

func TestTransactionRepositoryCreateAndGetByID(t *testing.T) {
	_, ctx, repository, tx, walletID := setupTransactionRepositoryTest(t)
	defer tx.Rollback()

	transaction := newTestWager(t, walletID)

	if err := repository.Create(ctx, tx, transaction); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit transaction: %v", err)
	}

	got, err := repository.GetByID(ctx, transaction.ID())
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}

	if got.ID() != transaction.ID() {
		t.Errorf("ID = %q, want %q", got.ID(), transaction.ID())
	}
	if got.ProviderID() != transaction.ProviderID() {
		t.Errorf("ProviderID = %q, want %q", got.ProviderID(), transaction.ProviderID())
	}
	if got.ExternalID() != transaction.ExternalID() {
		t.Errorf("ExternalID = %q, want %q", got.ExternalID(), transaction.ExternalID())
	}
	if got.IdempotencyKey() != transaction.IdempotencyKey() {
		t.Errorf("IdempotencyKey = %q, want %q", got.IdempotencyKey(), transaction.IdempotencyKey())
	}
	if got.PayloadHash() != transaction.PayloadHash() {
		t.Errorf("PayloadHash = %q, want %q", got.PayloadHash(), transaction.PayloadHash())
	}
	if got.Amount().AmountInMinorUnits() != 1000 {
		t.Errorf("amount = %d, want 1000", got.Amount().AmountInMinorUnits())
	}
	if got.Amount().Currency() != "BRL" {
		t.Errorf("currency = %q, want BRL", got.Amount().Currency())
	}
	if got.Status() != wager.StatusPending {
		t.Errorf("status = %q, want %q", got.Status(), wager.StatusPending)
	}
}

func TestTransactionRepositoryGetByIdempotencyKey(t *testing.T) {
	_, ctx, repository, tx, walletID := setupTransactionRepositoryTest(t)
	defer tx.Rollback()

	transaction := newTestWager(t, walletID)

	if err := repository.Create(ctx, tx, transaction); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit transaction: %v", err)
	}

	got, err := repository.GetByIdempotencyKey(
		ctx,
		transaction.ProviderID(),
		transaction.IdempotencyKey(),
	)
	if err != nil {
		t.Fatalf("GetByIdempotencyKey: %v", err)
	}
	if got.ID() != transaction.ID() {
		t.Errorf("ID = %q, want %q", got.ID(), transaction.ID())
	}
}

func TestTransactionRepositoryGetByExternalID(t *testing.T) {
	_, ctx, repository, tx, walletID := setupTransactionRepositoryTest(t)
	defer tx.Rollback()

	transaction := newTestWager(t, walletID)

	if err := repository.Create(ctx, tx, transaction); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit transaction: %v", err)
	}

	got, err := repository.GetByExternalID(
		ctx,
		transaction.ProviderID(),
		transaction.ExternalID(),
	)
	if err != nil {
		t.Fatalf("GetByExternalID: %v", err)
	}
	if got.ID() != transaction.ID() {
		t.Errorf("ID = %q, want %q", got.ID(), transaction.ID())
	}
}

func TestTransactionRepositoryGetByIDNotFound(t *testing.T) {
	_, ctx, repository, _, _ := setupTransactionRepositoryTest(t)

	_, err := repository.GetByID(ctx, newTransactionTestID(t))
	if !errors.Is(err, ErrTransactionNotFound) {
		t.Fatalf("error = %v, want ErrTransactionNotFound", err)
	}
}

func TestTransactionRepositoryRejectsDuplicateIdempotencyKey(t *testing.T) {
	_, ctx, repository, tx, walletID := setupTransactionRepositoryTest(t)
	defer tx.Rollback()

	first := newTestWager(t, walletID)
	second := newTestWager(t, walletID)

	// A repeated key for the same provider must conflict.
	// The second transaction has a distinct ID and external ID.
	second = mustRebuildWithIdempotencyKey(t, second, first)

	if err := repository.Create(ctx, tx, first); err != nil {
		t.Fatalf("create first transaction: %v", err)
	}

	err := repository.Create(ctx, tx, second)
	if !errors.Is(err, ErrTransactionConflict) {
		t.Fatalf("error = %v, want ErrTransactionConflict", err)
	}
}

func mustRebuildWithIdempotencyKey(
	t *testing.T,
	original wager.Transaction,
	first wager.Transaction,
) wager.Transaction {
	t.Helper()

	amount, err := money.New("10.00", "BRL")
	if err != nil {
		t.Fatalf("create test amount: %v", err)
	}

	transaction, err := wager.New(wager.Input{
		ID:             original.ID(),
		ProviderID:     first.ProviderID(),
		ExternalID:     original.ExternalID(),
		IdempotencyKey: first.IdempotencyKey(),
		PayloadHash:    original.PayloadHash(),
		WalletID:       original.WalletID(),
		PlayerID:       original.PlayerID(),
		RoundID:        original.RoundID(),
		GameID:         original.GameID(),
		Kind:           wager.KindBet,
		Amount:         amount,
	})
	if err != nil {
		t.Fatalf("rebuild duplicate transaction: %v", err)
	}

	return transaction
}

// TestListDuePendingReferencesClaimsEachRowOnce guards against a race where
// two instances of the reference-retry worker poll at the same time: before
// ListDuePendingReferences claimed rows atomically, both could load the same
// PENDING_REFERENCE transaction and process it concurrently, with one
// instance's successful result getting overwritten by the other's rejection.
func TestListDuePendingReferencesClaimsEachRowOnce(t *testing.T) {
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

	walletID := newTransactionTestID(t)
	playerID := newTransactionTestID(t)

	_, err = db.ExecContext(ctx, `
		INSERT INTO wallets (id, player_id, currency, balance_minor_units, version)
		VALUES ($1, $2, 'BRL', 10000, 1)
	`, walletID, playerID)
	if err != nil {
		t.Fatalf("create test wallet: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM transactions WHERE wallet_id = $1`, walletID)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM wallets WHERE id = $1`, walletID)
	})

	const total = 10
	ids := make([]string, total)
	for i := range ids {
		ids[i] = newTransactionTestID(t)
		_, err := db.ExecContext(ctx, `
			INSERT INTO transactions (
				id, provider_id, external_id, idempotency_key, payload_hash,
				wallet_id, player_id, round_id, game_id, kind,
				amount_minor_units, status, reference_external_id,
				reference_next_attempt_at
			)
			VALUES (
				$1, 'test-provider', $2, $2, 'test-hash',
				$3, $4, 'round-test', 'game-test', 'REFUND',
				1000, 'PENDING_REFERENCE', 'original-bet-id',
				NOW()
			)
		`, ids[i], ids[i], walletID, playerID)
		if err != nil {
			t.Fatalf("create pending reference transaction: %v", err)
		}
	}

	dbA, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open second PostgreSQL connection: %v", err)
	}
	defer dbA.Close()

	repoMain := NewTransactionRepository(db)
	repoA := NewTransactionRepository(dbA)

	var wg sync.WaitGroup
	results := make([][]DuePendingReference, 2)
	errs := make([]error, 2)

	wg.Add(2)
	go func() {
		defer wg.Done()
		results[0], errs[0] = repoMain.ListDuePendingReferences(ctx, total)
	}()
	go func() {
		defer wg.Done()
		results[1], errs[1] = repoA.ListDuePendingReferences(ctx, total)
	}()
	wg.Wait()

	if errs[0] != nil {
		t.Fatalf("ListDuePendingReferences[0]: %v", errs[0])
	}
	if errs[1] != nil {
		t.Fatalf("ListDuePendingReferences[1]: %v", errs[1])
	}

	seen := make(map[string]int)
	for _, batch := range results {
		for _, due := range batch {
			seen[due.Transaction.ID()]++
		}
	}

	for id, count := range seen {
		if count != 1 {
			t.Errorf("transaction %s claimed by %d concurrent callers, want 1", id, count)
		}
	}

	totalClaimed := len(results[0]) + len(results[1])
	if totalClaimed != total {
		t.Errorf("total claimed = %d, want %d (every row claimed exactly once)", totalClaimed, total)
	}
}
