package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/money"
)

func TestWalletRepositoryGetByID(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL is not set; skipping PostgreSQL integration test")
	}

	ctx, cancel := context.WithTimeout(
		context.Background(),
		5*time.Second,
	)
	defer cancel()

	db, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open PostgreSQL: %v", err)
	}
	defer db.Close()

	repository := NewWalletRepository(db)

	walletID := "11111111-1111-4111-8111-111111111111"
	playerID := "22222222-2222-4222-8222-222222222222"

	_, err = db.ExecContext(ctx, `
		INSERT INTO wallets (
			id, player_id, currency, balance_minor_units, version
		)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (id) DO UPDATE SET
			player_id = EXCLUDED.player_id,
			currency = EXCLUDED.currency,
			balance_minor_units = EXCLUDED.balance_minor_units,
			version = EXCLUDED.version
	`, walletID, playerID, "BRL", int64(12345), int64(3))
	if err != nil {
		t.Fatalf("insert test wallet: %v", err)
	}

	defer func() {
		_, cleanupErr := db.ExecContext(
			context.Background(),
			"DELETE FROM wallets WHERE id = $1",
			walletID,
		)
		if cleanupErr != nil {
			t.Errorf("clean up test wallet: %v", cleanupErr)
		}
	}()

	got, err := repository.GetByID(ctx, walletID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}

	if got.ID() != walletID {
		t.Errorf("ID = %q, want %q", got.ID(), walletID)
	}
	if got.PlayerID() != playerID {
		t.Errorf("PlayerID = %q, want %q", got.PlayerID(), playerID)
	}
	if got.Balance().AmountInMinorUnits() != 12345 {
		t.Errorf(
			"balance = %d, want 12345",
			got.Balance().AmountInMinorUnits(),
		)
	}
	if got.Balance().Currency() != "BRL" {
		t.Errorf("currency = %q, want BRL", got.Balance().Currency())
	}
	if got.Version() != 3 {
		t.Errorf("version = %d, want 3", got.Version())
	}

	_ = money.Zero
	_ = fmt.Sprintf
	_ = sql.ErrNoRows
}
