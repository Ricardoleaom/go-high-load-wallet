package wager

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/money"
	domainwager "github.com/Ricardoleaom/go-high-load-wallet/internal/domain/wager"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/infrastructure/postgres"
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

func mustMoney(t *testing.T, amount string) money.Money {
	t.Helper()
	m, err := money.New(amount, "BRL")
	if err != nil {
		t.Fatalf("money.New(%q): %v", amount, err)
	}
	return m
}

// newTestService opens its own *sql.DB, simulating a separate application
// instance pointed at the same database.
func newTestService(t *testing.T) (*Service, *sql.DB, context.Context) {
	t.Helper()

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL is not set; skipping PostgreSQL integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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

// createTestWallet uses the wallet ID itself as the player ID too: each
// call already generates a fresh UUID, so this keeps every test wallet
// under a distinct (playerId, currency) pair, required by the real
// uniqueness constraint on wallets, without a second return value.
func createTestWallet(t *testing.T, ctx context.Context, db *sql.DB, balance string) string {
	t.Helper()

	walletID := newTestID(t)
	amount := mustMoney(t, balance)

	_, err := db.ExecContext(ctx, `
		INSERT INTO wallets (id, player_id, currency, balance_minor_units, version)
		VALUES ($1, $1, 'BRL', $2, 1)
	`, walletID, amount.AmountInMinorUnits())
	if err != nil {
		t.Fatalf("create test wallet: %v", err)
	}

	return walletID
}

func betInput(walletID, providerID, externalID string, amount money.Money) SubmitInput {
	return SubmitInput{
		ProviderID:     providerID,
		ExternalID:     externalID,
		IdempotencyKey: providerID + ":" + externalID,
		PlayerID:       walletID,
		WalletID:       walletID,
		RoundID:        "round-1",
		GameID:         "game-1",
		Kind:           domainwager.KindBet,
		Amount:         amount,
	}
}

func TestSubmitRejectsWalletPlayerMismatch(t *testing.T) {
	service, db, ctx := newTestService(t)
	walletID := createTestWallet(t, ctx, db, "100.00")

	input := betInput(walletID, "provider-a", newTestID(t), mustMoney(t, "25.00"))
	input.PlayerID = newTestID(t) // a different player than the wallet's owner

	_, err := service.Submit(ctx, input)
	if !errors.Is(err, ErrWalletNotFound) {
		t.Fatalf("error = %v, want ErrWalletNotFound", err)
	}

	var balance int64
	if err := db.QueryRowContext(ctx, `SELECT balance_minor_units FROM wallets WHERE id = $1`, walletID).Scan(&balance); err != nil {
		t.Fatalf("read wallet balance: %v", err)
	}
	if balance != 10000 {
		t.Errorf("wallet changed after mismatched submission: %d, want 10000", balance)
	}
}

func TestSubmitBetDebitsWallet(t *testing.T) {
	service, db, ctx := newTestService(t)
	walletID := createTestWallet(t, ctx, db, "100.00")

	result, err := service.Submit(ctx, betInput(walletID, "provider-a", newTestID(t), mustMoney(t, "25.00")))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	if result.Status != domainwager.StatusProcessed {
		t.Fatalf("status = %s, want PROCESSED", result.Status)
	}
	if result.Balance.AmountInMinorUnits() != 7500 {
		t.Errorf("balance = %d, want 7500", result.Balance.AmountInMinorUnits())
	}
}

func TestSubmitBetRejectsInsufficientBalance(t *testing.T) {
	service, db, ctx := newTestService(t)
	walletID := createTestWallet(t, ctx, db, "10.00")

	result, err := service.Submit(ctx, betInput(walletID, "provider-a", newTestID(t), mustMoney(t, "25.00")))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	if result.Status != domainwager.StatusRejected {
		t.Fatalf("status = %s, want REJECTED", result.Status)
	}
	if result.FailureCode != FailureInsufficientBalance {
		t.Errorf("failure code = %s, want %s", result.FailureCode, FailureInsufficientBalance)
	}

	var balance int64
	if err := db.QueryRowContext(ctx, `SELECT balance_minor_units FROM wallets WHERE id = $1`, walletID).Scan(&balance); err != nil {
		t.Fatalf("read wallet balance: %v", err)
	}
	if balance != 1000 {
		t.Errorf("wallet balance changed after rejected bet: %d, want 1000", balance)
	}

	var ledgerCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ledger_entries WHERE wallet_id = $1`, walletID).Scan(&ledgerCount); err != nil {
		t.Fatalf("count ledger entries: %v", err)
	}
	if ledgerCount != 0 {
		t.Errorf("ledger entries after rejected bet = %d, want 0", ledgerCount)
	}
}

func TestSubmitIsIdempotentOnSameKeyAndPayload(t *testing.T) {
	service, db, ctx := newTestService(t)
	walletID := createTestWallet(t, ctx, db, "100.00")

	input := betInput(walletID, "provider-a", newTestID(t), mustMoney(t, "25.00"))

	first, err := service.Submit(ctx, input)
	if err != nil {
		t.Fatalf("first Submit: %v", err)
	}
	if first.IdempotentReplay {
		t.Error("first submission reported as replay")
	}

	second, err := service.Submit(ctx, input)
	if err != nil {
		t.Fatalf("second Submit: %v", err)
	}
	if !second.IdempotentReplay {
		t.Error("second submission not reported as replay")
	}
	if second.TransactionID != first.TransactionID {
		t.Errorf("transaction ID changed on replay: %s != %s", second.TransactionID, first.TransactionID)
	}
	if second.Balance.AmountInMinorUnits() != first.Balance.AmountInMinorUnits() {
		t.Errorf("replay balance = %d, want %d (original processed balance)",
			second.Balance.AmountInMinorUnits(), first.Balance.AmountInMinorUnits())
	}

	var balance int64
	if err := db.QueryRowContext(ctx, `SELECT balance_minor_units FROM wallets WHERE id = $1`, walletID).Scan(&balance); err != nil {
		t.Fatalf("read wallet balance: %v", err)
	}
	if balance != 7500 {
		t.Errorf("balance = %d, want 7500 (single debit)", balance)
	}
}

func TestSubmitRejectsSameKeyDifferentPayload(t *testing.T) {
	service, db, ctx := newTestService(t)
	walletID := createTestWallet(t, ctx, db, "100.00")

	input := betInput(walletID, "provider-a", newTestID(t), mustMoney(t, "25.00"))
	if _, err := service.Submit(ctx, input); err != nil {
		t.Fatalf("first Submit: %v", err)
	}

	input.Amount = mustMoney(t, "30.00")
	_, err := service.Submit(ctx, input)
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("error = %v, want ErrIdempotencyConflict", err)
	}
}

func TestSubmitRejectsReusedExternalIDWithDifferentKey(t *testing.T) {
	service, db, ctx := newTestService(t)
	walletID := createTestWallet(t, ctx, db, "100.00")

	externalID := newTestID(t)
	input := betInput(walletID, "provider-a", externalID, mustMoney(t, "25.00"))
	if _, err := service.Submit(ctx, input); err != nil {
		t.Fatalf("first Submit: %v", err)
	}

	input.IdempotencyKey = "provider-a:some-other-key"
	_, err := service.Submit(ctx, input)
	if !errors.Is(err, ErrIdempotencyKeyMismatch) {
		t.Fatalf("error = %v, want ErrIdempotencyKeyMismatch", err)
	}
}

func TestSubmitWinCreditsWallet(t *testing.T) {
	service, db, ctx := newTestService(t)
	walletID := createTestWallet(t, ctx, db, "100.00")

	input := betInput(walletID, "provider-a", newTestID(t), mustMoney(t, "40.00"))
	input.Kind = domainwager.KindWin

	result, err := service.Submit(ctx, input)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if result.Status != domainwager.StatusProcessed {
		t.Fatalf("status = %s, want PROCESSED", result.Status)
	}
	if result.Balance.AmountInMinorUnits() != 14000 {
		t.Errorf("balance = %d, want 14000", result.Balance.AmountInMinorUnits())
	}
}

func TestSubmitLossDoesNotMoveMoney(t *testing.T) {
	service, db, ctx := newTestService(t)
	walletID := createTestWallet(t, ctx, db, "100.00")

	input := betInput(walletID, "provider-a", newTestID(t), mustMoney(t, "0.00"))
	input.Kind = domainwager.KindLoss

	result, err := service.Submit(ctx, input)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if result.Status != domainwager.StatusProcessed {
		t.Fatalf("status = %s, want PROCESSED", result.Status)
	}
	if result.Balance.AmountInMinorUnits() != 10000 {
		t.Errorf("balance = %d, want 10000 (unchanged)", result.Balance.AmountInMinorUnits())
	}

	var ledgerCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ledger_entries WHERE wallet_id = $1`, walletID).Scan(&ledgerCount); err != nil {
		t.Fatalf("count ledger entries: %v", err)
	}
	if ledgerCount != 0 {
		t.Errorf("ledger entries for LOSS = %d, want 0", ledgerCount)
	}
}

func TestSubmitRefundReturnsBetAmount(t *testing.T) {
	service, db, ctx := newTestService(t)
	walletID := createTestWallet(t, ctx, db, "100.00")

	betExternalID := newTestID(t)
	bet := betInput(walletID, "provider-a", betExternalID, mustMoney(t, "40.00"))
	if _, err := service.Submit(ctx, bet); err != nil {
		t.Fatalf("submit bet: %v", err)
	}

	refund := betInput(walletID, "provider-a", newTestID(t), mustMoney(t, "40.00"))
	refund.Kind = domainwager.KindRefund
	refund.ReferenceExternalID = betExternalID

	result, err := service.Submit(ctx, refund)
	if err != nil {
		t.Fatalf("submit refund: %v", err)
	}
	if result.Status != domainwager.StatusProcessed {
		t.Fatalf("status = %s, want PROCESSED", result.Status)
	}
	if result.Balance.AmountInMinorUnits() != 10000 {
		t.Errorf("balance = %d, want 10000 (refunded back to original)", result.Balance.AmountInMinorUnits())
	}
}

func TestSubmitRefundRejectsSecondAttemptOnSameBet(t *testing.T) {
	service, db, ctx := newTestService(t)
	walletID := createTestWallet(t, ctx, db, "100.00")

	betExternalID := newTestID(t)
	bet := betInput(walletID, "provider-a", betExternalID, mustMoney(t, "40.00"))
	if _, err := service.Submit(ctx, bet); err != nil {
		t.Fatalf("submit bet: %v", err)
	}

	refund1 := betInput(walletID, "provider-a", newTestID(t), mustMoney(t, "40.00"))
	refund1.Kind = domainwager.KindRefund
	refund1.ReferenceExternalID = betExternalID
	if result, err := service.Submit(ctx, refund1); err != nil || result.Status != domainwager.StatusProcessed {
		t.Fatalf("first refund: result=%+v err=%v", result, err)
	}

	refund2 := betInput(walletID, "provider-a", newTestID(t), mustMoney(t, "40.00"))
	refund2.Kind = domainwager.KindRefund
	refund2.ReferenceExternalID = betExternalID

	result, err := service.Submit(ctx, refund2)
	if err != nil {
		t.Fatalf("second refund: %v", err)
	}
	if result.Status != domainwager.StatusRejected {
		t.Fatalf("status = %s, want REJECTED", result.Status)
	}
	if result.FailureCode != FailureReferenceAlreadyReversed {
		t.Errorf("failure code = %s, want %s", result.FailureCode, FailureReferenceAlreadyReversed)
	}
}

func TestSubmitRollbackUndoesWin(t *testing.T) {
	service, db, ctx := newTestService(t)
	walletID := createTestWallet(t, ctx, db, "100.00")

	winExternalID := newTestID(t)
	win := betInput(walletID, "provider-a", winExternalID, mustMoney(t, "30.00"))
	win.Kind = domainwager.KindWin
	if _, err := service.Submit(ctx, win); err != nil {
		t.Fatalf("submit win: %v", err)
	}

	rollback := betInput(walletID, "provider-a", newTestID(t), mustMoney(t, "30.00"))
	rollback.Kind = domainwager.KindRollback
	rollback.ReferenceExternalID = winExternalID

	result, err := service.Submit(ctx, rollback)
	if err != nil {
		t.Fatalf("submit rollback: %v", err)
	}
	if result.Status != domainwager.StatusProcessed {
		t.Fatalf("status = %s, want PROCESSED", result.Status)
	}
	if result.Balance.AmountInMinorUnits() != 10000 {
		t.Errorf("balance = %d, want 10000 (win undone)", result.Balance.AmountInMinorUnits())
	}
}

func TestSubmitRefundBeforeReferenceArrivesGoesPendingThenResolves(t *testing.T) {
	service, db, ctx := newTestService(t)
	walletID := createTestWallet(t, ctx, db, "100.00")

	betExternalID := newTestID(t)

	refund := betInput(walletID, "provider-a", newTestID(t), mustMoney(t, "40.00"))
	refund.Kind = domainwager.KindRefund
	refund.ReferenceExternalID = betExternalID

	result, err := service.Submit(ctx, refund)
	if err != nil {
		t.Fatalf("submit refund: %v", err)
	}
	if result.Status != domainwager.StatusPendingReference {
		t.Fatalf("status = %s, want PENDING_REFERENCE", result.Status)
	}

	bet := betInput(walletID, "provider-a", betExternalID, mustMoney(t, "40.00"))
	if _, err := service.Submit(ctx, bet); err != nil {
		t.Fatalf("submit bet: %v", err)
	}

	resolved, err := db.ExecContext(ctx,
		`UPDATE transactions SET reference_next_attempt_at = NOW() WHERE id = $1`,
		result.TransactionID,
	)
	if err != nil {
		t.Fatalf("force reference retry due: %v", err)
	}
	if n, _ := resolved.RowsAffected(); n != 1 {
		t.Fatalf("expected to update 1 row, updated %d", n)
	}

	retried, err := service.RetryPendingReferences(ctx, 10)
	if err != nil {
		t.Fatalf("RetryPendingReferences: %v", err)
	}
	if retried != 1 {
		t.Fatalf("retried = %d, want 1", retried)
	}

	var status string
	if err := db.QueryRowContext(ctx, `SELECT status FROM transactions WHERE id = $1`, result.TransactionID).Scan(&status); err != nil {
		t.Fatalf("read resolved transaction status: %v", err)
	}
	if status != string(domainwager.StatusProcessed) {
		t.Fatalf("status = %s, want PROCESSED", status)
	}
}

// TestConcurrentBetsOnSameWallet is the challenge's required scenario: a
// wallet with 100.00 BRL receives two concurrent 80.00 BRL bets. Exactly one
// must process, the other must be rejected for insufficient balance, the
// final balance must be 20.00 BRL, and there must be a single ledger entry.
// TestConcurrentBetsOnSameWallet is the scenario the edital names
// explicitly: a 100.00 BRL wallet gets two concurrent 80.00 BRL bets, one
// must process and one must be rejected for insufficient balance. Each bet
// comes from its own Service backed by its own *sql.DB, standing in for two
// independent application processes racing on the same wallet row.
func TestConcurrentBetsOnSameWallet(t *testing.T) {
	serviceA, db, ctx := newTestService(t)
	serviceB, _, _ := newTestService(t)
	walletID := createTestWallet(t, ctx, db, "100.00")

	services := []*Service{serviceA, serviceB}

	var wg sync.WaitGroup
	results := make([]Result, 2)
	errs := make([]error, 2)

	for i := 0; i < 2; i++ {
		wg.Add(1)
		externalID := newTestID(t)
		go func(i int, externalID string) {
			defer wg.Done()
			input := betInput(walletID, "provider-a", externalID, mustMoney(t, "80.00"))
			results[i], errs[i] = services[i].Submit(ctx, input)
		}(i, externalID)
	}
	wg.Wait()

	processed, rejected := 0, 0
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Submit[%d]: %v", i, err)
		}
		switch results[i].Status {
		case domainwager.StatusProcessed:
			processed++
		case domainwager.StatusRejected:
			rejected++
		default:
			t.Fatalf("unexpected status[%d]: %s", i, results[i].Status)
		}
	}

	if processed != 1 || rejected != 1 {
		t.Fatalf("processed=%d rejected=%d, want 1 and 1", processed, rejected)
	}

	var balance int64
	if err := db.QueryRowContext(ctx, `SELECT balance_minor_units FROM wallets WHERE id = $1`, walletID).Scan(&balance); err != nil {
		t.Fatalf("read final balance: %v", err)
	}
	if balance != 2000 {
		t.Errorf("final balance = %d, want 2000", balance)
	}

	var ledgerCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ledger_entries WHERE wallet_id = $1`, walletID).Scan(&ledgerCount); err != nil {
		t.Fatalf("count ledger entries: %v", err)
	}
	if ledgerCount != 1 {
		t.Errorf("ledger entries = %d, want 1", ledgerCount)
	}
}

// TestConcurrentBetsAcrossThreeIndependentInstances repeats the same-wallet
// race from TestConcurrentBetsOnSameWallet across three Service values, each
// with its own *sql.DB, the minimum the edital asks for when demonstrating
// concurrency guarantees. Three 40.00 BRL bets hit a 100.00 BRL wallet: any
// two fit (80.00), a third never does, so exactly two must process and one
// must be rejected no matter which instance wins the race.
func TestConcurrentBetsAcrossThreeIndependentInstances(t *testing.T) {
	serviceA, db, ctx := newTestService(t)
	serviceB, _, _ := newTestService(t)
	serviceC, _, _ := newTestService(t)
	walletID := createTestWallet(t, ctx, db, "100.00")

	services := []*Service{serviceA, serviceB, serviceC}

	var wg sync.WaitGroup
	results := make([]Result, len(services))
	errs := make([]error, len(services))

	for i, svc := range services {
		wg.Add(1)
		externalID := newTestID(t)
		go func(i int, svc *Service, externalID string) {
			defer wg.Done()
			input := betInput(walletID, "provider-a", externalID, mustMoney(t, "40.00"))
			results[i], errs[i] = svc.Submit(ctx, input)
		}(i, svc, externalID)
	}
	wg.Wait()

	processed, rejected := 0, 0
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Submit[%d]: %v", i, err)
		}
		switch results[i].Status {
		case domainwager.StatusProcessed:
			processed++
		case domainwager.StatusRejected:
			rejected++
		default:
			t.Fatalf("unexpected status[%d]: %s", i, results[i].Status)
		}
	}

	if processed != 2 || rejected != 1 {
		t.Fatalf("processed=%d rejected=%d, want 2 and 1", processed, rejected)
	}

	var balance int64
	if err := db.QueryRowContext(ctx, `SELECT balance_minor_units FROM wallets WHERE id = $1`, walletID).Scan(&balance); err != nil {
		t.Fatalf("read final balance: %v", err)
	}
	if balance != 2000 {
		t.Errorf("final balance = %d, want 2000", balance)
	}

	var ledgerCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ledger_entries WHERE wallet_id = $1`, walletID).Scan(&ledgerCount); err != nil {
		t.Fatalf("count ledger entries: %v", err)
	}
	if ledgerCount != 2 {
		t.Errorf("ledger entries = %d, want 2", ledgerCount)
	}
}

// TestDistinctWalletsProcessConcurrently fires bets on ten different wallets
// at once from three independent instances. Each wallet is its own row, so
// none of them should wait on or interfere with another.
func TestDistinctWalletsProcessConcurrently(t *testing.T) {
	serviceA, db, ctx := newTestService(t)
	serviceB, _, _ := newTestService(t)
	serviceC, _, _ := newTestService(t)
	services := []*Service{serviceA, serviceB, serviceC}

	const wallets = 10
	walletIDs := make([]string, wallets)
	for i := range walletIDs {
		walletIDs[i] = createTestWallet(t, ctx, db, "100.00")
	}

	var wg sync.WaitGroup
	results := make([]Result, wallets)
	errs := make([]error, wallets)

	for i, walletID := range walletIDs {
		wg.Add(1)
		externalID := newTestID(t)
		go func(i int, walletID, externalID string) {
			defer wg.Done()
			input := betInput(walletID, "provider-a", externalID, mustMoney(t, "30.00"))
			results[i], errs[i] = services[i%len(services)].Submit(ctx, input)
		}(i, walletID, externalID)
	}
	wg.Wait()

	for i, walletID := range walletIDs {
		if errs[i] != nil {
			t.Fatalf("Submit[%d]: %v", i, errs[i])
		}
		if results[i].Status != domainwager.StatusProcessed {
			t.Errorf("wallet %d status = %s, want PROCESSED", i, results[i].Status)
		}

		var balance int64
		if err := db.QueryRowContext(ctx, `SELECT balance_minor_units FROM wallets WHERE id = $1`, walletID).Scan(&balance); err != nil {
			t.Fatalf("read balance of wallet %d: %v", i, err)
		}
		if balance != 7000 {
			t.Errorf("wallet %d balance = %d, want 7000", i, balance)
		}
	}
}

// TestFiftyConcurrentDuplicateSubmissions sends the same bet 50 times in
// parallel, spread round-robin across three independent Service instances
// (three separate *sql.DB), and checks a single debit results. This
// exercises the persisted idempotency guarantee under real contention
// across processes, not just across goroutines of one process.
func TestFiftyConcurrentDuplicateSubmissions(t *testing.T) {
	serviceA, db, ctx := newTestService(t)
	serviceB, _, _ := newTestService(t)
	serviceC, _, _ := newTestService(t)
	walletID := createTestWallet(t, ctx, db, "100.00")

	services := []*Service{serviceA, serviceB, serviceC}
	input := betInput(walletID, "provider-a", newTestID(t), mustMoney(t, "25.00"))

	const attempts = 50
	var wg sync.WaitGroup
	results := make([]Result, attempts)
	errs := make([]error, attempts)

	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = services[i%len(services)].Submit(ctx, input)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Submit[%d]: %v", i, err)
		}
	}

	var balance int64
	if err := db.QueryRowContext(ctx, `SELECT balance_minor_units FROM wallets WHERE id = $1`, walletID).Scan(&balance); err != nil {
		t.Fatalf("read final balance: %v", err)
	}
	if balance != 7500 {
		t.Errorf("final balance = %d, want 7500 (single debit across 50 duplicates)", balance)
	}

	var ledgerCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ledger_entries WHERE wallet_id = $1`, walletID).Scan(&ledgerCount); err != nil {
		t.Fatalf("count ledger entries: %v", err)
	}
	if ledgerCount != 1 {
		t.Errorf("ledger entries = %d, want 1", ledgerCount)
	}
}
