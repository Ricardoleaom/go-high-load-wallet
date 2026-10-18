package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/testsupport/pgtest"
)

// Transactions written before the currency column existed take their
// wallet's currency, and the migration can be rolled back.
func TestMigrationBackfillsTransactionCurrency(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	url := pgtest.FreshURL(t)
	if err := pgtest.MigrateSteps(ctx, url, -1); err != nil {
		t.Fatalf("migrate down one step: %v", err)
	}

	db, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("open PostgreSQL: %v", err)
	}
	defer func() { _ = db.Close() }()

	walletID, playerID, transactionID := newTransactionTestID(t), newTransactionTestID(t), newTransactionTestID(t)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO wallets (id, player_id, currency, balance_minor_units) VALUES ($1, $2, 'EUR', 1000);
	`, walletID, playerID); err != nil {
		t.Fatalf("insert wallet: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO transactions (id, wallet_id, player_id, kind, amount_minor_units, status)
		VALUES ($1, $2, $3, 'OPENING', 1000, 'PROCESSED')
	`, transactionID, walletID, playerID); err != nil {
		t.Fatalf("insert transaction under the previous schema: %v", err)
	}

	if err := pgtest.MigrateSteps(ctx, url, 1); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	var currency string
	if err := db.QueryRowContext(ctx, `SELECT currency FROM transactions WHERE id = $1`, transactionID).Scan(&currency); err != nil {
		t.Fatalf("read backfilled currency: %v", err)
	}
	if currency != "EUR" {
		t.Fatalf("currency = %q, want EUR from the wallet", currency)
	}

	if err := pgtest.MigrateSteps(ctx, url, -1); err != nil {
		t.Fatalf("migrate down again: %v", err)
	}
}
