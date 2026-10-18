// Command seed loads benchmark wallets into PostgreSQL with COPY and writes
// the walletId,playerId CSV the k6 scenarios read.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jackc/pgx/v5"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/bench/seed"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/money"
)

func main() {
	wallets := flag.Int("wallets", 100000, "number of wallets to create")
	balance := flag.String("balance", "1000000.00", "initial balance of every wallet")
	currency := flag.String("currency", "BRL", "ISO 4217 currency of every wallet")
	out := flag.String("out", "bench/data/wallets.csv", "where to write the walletId,playerId CSV")
	databaseURL := flag.String("database-url", os.Getenv("DATABASE_URL"), "PostgreSQL URL (defaults to DATABASE_URL)")
	truncate := flag.Bool("truncate", false, "empty the wallet tables first instead of refusing to seed a non-empty database")
	flag.Parse()

	if err := run(*databaseURL, *wallets, *balance, *currency, *out, *truncate); err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		os.Exit(1)
	}
}

func run(databaseURL string, wallets int, balance, currency, out string, truncate bool) error {
	if databaseURL == "" {
		return fmt.Errorf("no database: set DATABASE_URL or -database-url")
	}
	amount, err := money.New(balance, currency)
	if err != nil {
		return fmt.Errorf("balance: %w", err)
	}

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	if err := os.MkdirAll(filepath.Dir(out), 0o750); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(out), err)
	}
	file, err := os.Create(out) //nolint:gosec // the output path is the operator's own flag
	if err != nil {
		return fmt.Errorf("create %s: %w", out, err)
	}
	defer func() { _ = file.Close() }()

	result, err := seed.Run(ctx, conn, seed.Options{Wallets: wallets, Balance: amount, Truncate: truncate}, file)
	if err != nil {
		return err
	}
	fmt.Printf("seeded %d wallets in %s; CSV at %s\n", result.Wallets, result.Elapsed.Round(1e6), out)
	return nil
}
