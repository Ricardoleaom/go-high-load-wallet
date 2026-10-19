package migrations_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/testsupport/pgtest"
)

// Transactions written before the currency column existed (migration 11)
// take their wallet's currency, and the migration can be rolled back.
func TestMigrationBackfillsTransactionCurrency(t *testing.T) {
	url := pgtest.FreshURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	if err := pgtest.MigrateTo(ctx, url, 10); err != nil {
		t.Fatalf("migrate to the version before the currency column: %v", err)
	}
	db, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	walletID, playerID, transactionID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO wallets (id, player_id, currency, balance_minor_units) VALUES ($1, $2, 'EUR', 1000)`,
		walletID, playerID); err != nil {
		t.Fatalf("insert wallet: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO transactions (id, wallet_id, player_id, kind, amount_minor_units, status)
		VALUES ($1, $2, $3, 'OPENING', 1000, 'PROCESSED')`, transactionID, walletID, playerID); err != nil {
		t.Fatalf("insert transaction under the previous schema: %v", err)
	}

	if err := pgtest.MigrateTo(ctx, url, 11); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	var currency string
	if err := db.QueryRowContext(ctx, `SELECT currency FROM transactions WHERE id = $1`, transactionID).Scan(&currency); err != nil {
		t.Fatalf("read backfilled currency: %v", err)
	}
	if currency != "EUR" {
		t.Fatalf("currency = %q, want EUR from the wallet", currency)
	}

	if err := pgtest.MigrateTo(ctx, url, 10); err != nil {
		t.Fatalf("migrate down again: %v", err)
	}
}
