package postgres_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/adapters/postgres"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/queries"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/ledger"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/wager"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/testsupport/pgtest"
)

func newReader(t *testing.T) (harness, *postgres.Reader) {
	t.Helper()
	h := newHarness(t, pgtest.URL(t))
	return h, postgres.NewReader(h.pool)
}

func TestReaderWallet(t *testing.T) {
	h, reader := newReader(t)
	c := newCommands(t)
	walletID, playerID := openWallet(t, h.engine, "100.00")
	mustExecute(t, h.engine, c.bet(walletID, playerID, "30.00"))

	w, err := reader.Wallet(testContext(t), walletID)
	if err != nil {
		t.Fatal(err)
	}
	if w.ID() != walletID || w.PlayerID() != playerID || w.Balance().String() != "70.00 BRL" || w.Version() != 2 {
		t.Errorf("wallet = %s %s %s v%d", w.ID(), w.PlayerID(), w.Balance(), w.Version())
	}
	if w.UpdatedAt().Before(w.CreatedAt()) {
		t.Errorf("updated %s before created %s", w.UpdatedAt(), w.CreatedAt())
	}
}

func TestReaderTransactionCarriesItsBalance(t *testing.T) {
	h, reader := newReader(t)
	c := newCommands(t)
	walletID, playerID := openWallet(t, h.engine, "100.00")

	bet := mustExecute(t, h.engine, c.bet(walletID, playerID, "30.00"))
	mustExecute(t, h.engine, c.bet(walletID, playerID, "10.00"))
	loss := mustExecute(t, h.engine, c.wager(walletID, playerID, wager.KindLoss, "0.00", ""))
	mustExecute(t, h.engine, c.wager(walletID, playerID, wager.KindWin, "5.00", ""))
	rejected := mustExecute(t, h.engine, c.bet(walletID, playerID, "900.00"))

	for name, tc := range map[string]struct {
		id, balance string
		status      wager.Status
	}{
		"bet, at its own balance":  {bet.TransactionID, "70.00", wager.StatusProcessed},
		"loss, at the current one": {loss.TransactionID, "65.00", wager.StatusProcessed},
		"rejection, at none":       {rejected.TransactionID, "", wager.StatusRejected},
	} {
		view, err := reader.Transaction(testContext(t), tc.id)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := ""
		if view.Balance.Currency() != "" {
			got = view.Balance.DecimalString()
		}
		if view.Transaction.ID() != tc.id || view.Transaction.Status() != tc.status || got != tc.balance {
			t.Errorf("%s = %s %s balance %q, want %s balance %q",
				name, view.Transaction.ID(), view.Transaction.Status(), got, tc.status, tc.balance)
		}
	}
}

func TestReaderTransactionByExternalID(t *testing.T) {
	h, reader := newReader(t)
	c := newCommands(t)
	walletID, playerID := openWallet(t, h.engine, "100.00")
	bet := c.bet(walletID, playerID, "30.00")
	placed := mustExecute(t, h.engine, bet)

	view, err := reader.TransactionByExternalID(testContext(t), bet.ProviderID, bet.ExternalID)
	if err != nil {
		t.Fatal(err)
	}
	tx := view.Transaction
	if tx.ID() != placed.TransactionID || tx.ProviderID() != bet.ProviderID || tx.ExternalID() != bet.ExternalID ||
		tx.IdempotencyKey() != bet.IdempotencyKey || tx.Kind() != wager.KindBet || tx.Amount().String() != "30.00 BRL" {
		t.Errorf("transaction = %+v", tx)
	}
	if _, err := reader.TransactionByExternalID(testContext(t), "another-provider", bet.ExternalID); !errors.Is(err, queries.ErrTransactionNotFound) {
		t.Errorf("another provider's lookup: err = %v, want ErrTransactionNotFound", err)
	}
}

func TestReaderNotFound(t *testing.T) {
	_, reader := newReader(t)
	ctx := testContext(t)
	if _, err := reader.Wallet(ctx, uuid.NewString()); !errors.Is(err, queries.ErrWalletNotFound) {
		t.Errorf("Wallet: err = %v", err)
	}
	if _, err := reader.Transaction(ctx, uuid.NewString()); !errors.Is(err, queries.ErrTransactionNotFound) {
		t.Errorf("Transaction: err = %v", err)
	}
	if _, err := reader.Reconciliation(ctx, uuid.NewString()); !errors.Is(err, queries.ErrWalletNotFound) {
		t.Errorf("Reconciliation: err = %v", err)
	}
}

// The HTTP adapter validates identifiers, but the reader does not count on
// it: a malformed ID names nothing.
func TestReaderTreatsMalformedIDsAsNotFound(t *testing.T) {
	_, reader := newReader(t)
	ctx := testContext(t)
	if _, err := reader.Wallet(ctx, "not-a-uuid"); !errors.Is(err, queries.ErrWalletNotFound) {
		t.Errorf("Wallet: err = %v", err)
	}
	if _, err := reader.Transaction(ctx, "not-a-uuid"); !errors.Is(err, queries.ErrTransactionNotFound) {
		t.Errorf("Transaction: err = %v", err)
	}
	if _, err := reader.Reconciliation(ctx, "not-a-uuid"); !errors.Is(err, queries.ErrWalletNotFound) {
		t.Errorf("Reconciliation: err = %v", err)
	}
	if entries, err := reader.Ledger(ctx, "not-a-uuid", queries.Cursor{}, 10); err != nil || len(entries) != 0 {
		t.Errorf("Ledger: %d entries, err = %v; want none and no error", len(entries), err)
	}
	if entries, err := reader.Ledger(ctx, uuid.NewString(), queries.Cursor{ID: "not-a-uuid"}, 10); err != nil || len(entries) != 0 {
		t.Errorf("Ledger with a malformed cursor: %d entries, err = %v; want none and no error", len(entries), err)
	}
}

func TestLedgerPagesWithoutGapsOrRepeats(t *testing.T) {
	h, reader := newReader(t)
	c := newCommands(t)
	walletID, playerID := openWallet(t, h.engine, "100.00")
	for range 6 {
		mustExecute(t, h.engine, c.bet(walletID, playerID, "1.00"))
		mustExecute(t, h.engine, c.wager(walletID, playerID, wager.KindWin, "2.00", ""))
	}

	var all []ledger.Entry
	var cursor queries.Cursor
	for page := 0; ; page++ {
		entries, err := reader.Ledger(testContext(t), walletID, cursor, 5)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, entries...)
		if len(entries) < 5 {
			break
		}
		last := entries[len(entries)-1]
		cursor = queries.Cursor{CreatedAt: last.CreatedAt(), ID: last.ID()}
		if page > 10 {
			t.Fatal("paging does not end")
		}
	}

	if len(all) != 13 {
		t.Fatalf("paged %d entries, want 13 (opening + 12 movements)", len(all))
	}
	seen := map[string]bool{}
	for i, e := range all {
		if seen[e.ID()] {
			t.Fatalf("entry %s returned twice", e.ID())
		}
		seen[e.ID()] = true
		if i > 0 && e.BalanceBefore().AmountInMinorUnits() != all[i-1].BalanceAfter().AmountInMinorUnits() {
			t.Errorf("entry %d starts at %s, the previous one ended at %s", i, e.BalanceBefore(), all[i-1].BalanceAfter())
		}
	}
	if first := all[0]; first.Direction() != ledger.DirectionCredit || first.Amount().String() != "100.00 BRL" ||
		first.BalanceBefore().DecimalString() != "0.00" {
		t.Errorf("first entry = %s %s from %s", first.Direction(), first.Amount(), first.BalanceBefore())
	}
	if last := all[len(all)-1]; last.BalanceAfter().DecimalString() != "106.00" {
		t.Errorf("last entry ends at %s, want 106.00", last.BalanceAfter())
	}
}

func TestReconciliationSnapshot(t *testing.T) {
	h, reader := newReader(t)
	c := newCommands(t)
	walletID, playerID := openWallet(t, h.engine, "100.00")
	mustExecute(t, h.engine, c.bet(walletID, playerID, "30.00"))
	mustExecute(t, h.engine, c.wager(walletID, playerID, wager.KindWin, "5.00", ""))

	snapshot, err := reader.Reconciliation(testContext(t), walletID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.StoredBalance.String() != "75.00 BRL" || snapshot.CalculatedBalance.String() != "75.00 BRL" ||
		snapshot.CheckedEntries != 3 {
		t.Errorf("snapshot = %s / %s over %d entries", snapshot.StoredBalance, snapshot.CalculatedBalance, snapshot.CheckedEntries)
	}

	// A corrupted ledger shows up as a difference, even one below zero.
	loss := mustExecute(t, h.engine, c.wager(walletID, playerID, wager.KindLoss, "0.00", ""))
	if _, err := h.pool.Exec(testContext(t), `
		INSERT INTO ledger_entries (id, wallet_id, transaction_id, direction, amount_delta_minor_units,
		                            balance_before_minor_units, balance_after_minor_units)
		VALUES ($1, $2, $3, 'DEBIT', -100000, 0, 0)`, uuid.NewString(), walletID, loss.TransactionID); err != nil {
		t.Fatal(err)
	}
	snapshot, err = reader.Reconciliation(testContext(t), walletID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.CalculatedBalance.DecimalString() != "-925.00" || snapshot.CheckedEntries != 4 {
		t.Errorf("corrupted snapshot = %s over %d entries, want -925.00 over 4",
			snapshot.CalculatedBalance, snapshot.CheckedEntries)
	}
}

func TestReadinessPingsThePool(t *testing.T) {
	h := newHarness(t, pgtest.URL(t))
	readiness := postgres.NewReadiness(h.pool)
	if readiness.Name() != "postgres" {
		t.Errorf("Name = %q", readiness.Name())
	}
	if err := readiness.Ready(testContext(t)); err != nil {
		t.Errorf("Ready on a live database: %v", err)
	}
	h.pool.Close()
	if err := readiness.Ready(testContext(t)); err == nil {
		t.Error("Ready on a closed pool reported no error")
	}
}
