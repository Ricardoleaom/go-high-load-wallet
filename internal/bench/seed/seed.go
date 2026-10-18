// Package seed loads benchmark wallets straight into PostgreSQL with COPY.
//
// Each wallet gets exactly what opening it through the API would persist —
// the wallet at version 1, a PROCESSED OPENING transaction and its CREDIT
// ledger entry — except the outbox events: seeded wallets stand for history
// that existed before the run, and must not show up as publishing backlog in
// the measurements.
package seed

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/money"
)

// ErrNotEmpty means the database already has wallets and Truncate was off.
var ErrNotEmpty = errors.New("database already has wallets; seed refuses to mix data (use truncate)")

type Options struct {
	Wallets  int
	Balance  money.Money
	Truncate bool
}

type Result struct {
	Wallets int
	Elapsed time.Duration
}

type seededWallet struct {
	id, playerID, openingID, entryID pgtype.UUID
}

// Run writes opts.Wallets wallets in one transaction and a walletId,playerId
// CSV (with header) to out.
func Run(ctx context.Context, conn *pgx.Conn, opts Options, out io.Writer) (Result, error) {
	if opts.Wallets <= 0 {
		return Result{}, fmt.Errorf("wallets must be positive, got %d", opts.Wallets)
	}
	if opts.Balance.AmountInMinorUnits() <= 0 {
		return Result{}, fmt.Errorf("balance must be positive, got %s", opts.Balance)
	}

	started := time.Now()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("begin seed: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := prepare(ctx, tx, opts.Truncate); err != nil {
		return Result{}, err
	}

	wallets := make([]seededWallet, opts.Wallets)
	for i := range wallets {
		wallets[i] = seededWallet{id: newUUID(), playerID: newUUID(), openingID: newUUID(), entryID: newUUID()}
	}

	now := time.Now().UTC()
	balance := opts.Balance.AmountInMinorUnits()
	currency := opts.Balance.Currency()

	copies := []struct {
		table   string
		columns []string
		row     func(w seededWallet) []any
	}{
		{"wallets", []string{"id", "player_id", "currency", "balance_minor_units", "version", "created_at", "updated_at"},
			func(w seededWallet) []any { return []any{w.id, w.playerID, currency, balance, int64(1), now, now} }},
		{"transactions", []string{"id", "wallet_id", "player_id", "kind", "amount_minor_units", "currency", "status", "created_at", "updated_at"},
			func(w seededWallet) []any {
				return []any{w.openingID, w.id, w.playerID, "OPENING", balance, currency, "PROCESSED", now, now}
			}},
		{"ledger_entries", []string{"id", "wallet_id", "transaction_id", "direction", "amount_delta_minor_units", "balance_before_minor_units", "balance_after_minor_units", "created_at"},
			func(w seededWallet) []any {
				return []any{w.entryID, w.id, w.openingID, "CREDIT", balance, int64(0), balance, now}
			}},
	}
	for _, c := range copies {
		i := 0
		_, err := tx.CopyFrom(ctx, pgx.Identifier{c.table}, c.columns, pgx.CopyFromFunc(func() ([]any, error) {
			if i == len(wallets) {
				return nil, nil
			}
			row := c.row(wallets[i])
			i++
			return row, nil
		}))
		if err != nil {
			return Result{}, fmt.Errorf("copy %s: %w", c.table, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return Result{}, fmt.Errorf("commit seed: %w", err)
	}
	// Fresh planner statistics, so the first measured queries do not run on
	// plans made for empty tables.
	if _, err := conn.Exec(ctx, "ANALYZE wallets, transactions, ledger_entries"); err != nil {
		return Result{}, fmt.Errorf("analyze seeded tables: %w", err)
	}

	if err := writeCSV(out, wallets); err != nil {
		return Result{}, err
	}
	return Result{Wallets: len(wallets), Elapsed: time.Since(started)}, nil
}

func prepare(ctx context.Context, tx pgx.Tx, truncate bool) error {
	if truncate {
		_, err := tx.Exec(ctx, "TRUNCATE wallets, transactions, ledger_entries, outbox_events, inbox_messages")
		if err != nil {
			return fmt.Errorf("truncate: %w", err)
		}
		return nil
	}

	var hasWallets bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM wallets)").Scan(&hasWallets); err != nil {
		return fmt.Errorf("check for existing wallets: %w", err)
	}
	if hasWallets {
		return ErrNotEmpty
	}
	return nil
}

func writeCSV(out io.Writer, wallets []seededWallet) error {
	w := csv.NewWriter(out)
	if err := w.Write([]string{"walletId", "playerId"}); err != nil {
		return fmt.Errorf("write CSV header: %w", err)
	}
	for _, wallet := range wallets {
		if err := w.Write([]string{uuidString(wallet.id), uuidString(wallet.playerID)}); err != nil {
			return fmt.Errorf("write CSV row: %w", err)
		}
	}
	w.Flush()
	return w.Error()
}

func newUUID() pgtype.UUID {
	return pgtype.UUID{Bytes: uuid.New(), Valid: true}
}

func uuidString(u pgtype.UUID) string {
	return uuid.UUID(u.Bytes).String()
}
