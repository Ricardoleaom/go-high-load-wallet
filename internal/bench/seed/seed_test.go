package seed_test

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/adapters/postgres"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/bench/seed"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/money"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/testsupport/pgtest"
)

func connect(ctx context.Context, t *testing.T, url string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

func options(t *testing.T, wallets int, truncate bool) seed.Options {
	t.Helper()
	balance, err := money.New("1000.00", "BRL")
	if err != nil {
		t.Fatalf("money: %v", err)
	}
	return seed.Options{Wallets: wallets, Balance: balance, Truncate: truncate}
}

func count(ctx context.Context, t *testing.T, conn *pgx.Conn, query string) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(ctx, query).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func TestSeedWritesWalletsOpeningsAndLedger(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	url := pgtest.FreshURL(t)
	conn := connect(ctx, t, url)

	var out bytes.Buffer
	result, err := seed.Run(ctx, conn, options(t, 250, false), &out)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Wallets != 250 {
		t.Fatalf("Result.Wallets = %d, want 250", result.Wallets)
	}

	for query, want := range map[string]int{
		`SELECT count(*) FROM wallets WHERE version = 1 AND balance_minor_units = 100000`:                   250,
		`SELECT count(*) FROM transactions WHERE kind = 'OPENING' AND status = 'PROCESSED'`:                 250,
		`SELECT count(*) FROM ledger_entries WHERE direction = 'CREDIT' AND balance_before_minor_units = 0`: 250,
		`SELECT count(*) FROM outbox_events`:                                                                0,
	} {
		if got := count(ctx, t, conn, query); got != want {
			t.Errorf("%s = %d, want %d", query, got, want)
		}
	}

	rows, err := csv.NewReader(&out).ReadAll()
	if err != nil {
		t.Fatalf("read CSV: %v", err)
	}
	if len(rows) != 251 || rows[0][0] != "walletId" || rows[0][1] != "playerId" {
		t.Fatalf("CSV has %d rows starting with %v, want a header and 250 wallets", len(rows), rows[0])
	}

	pool, err := postgres.Open(ctx, url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()
	snapshot, err := postgres.NewReader(pool).Reconciliation(ctx, rows[1][0])
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if difference, _ := snapshot.StoredBalance.Subtract(snapshot.CalculatedBalance); difference.AmountInMinorUnits() != 0 {
		t.Fatalf("seeded wallet does not reconcile: stored %s, ledger %s", snapshot.StoredBalance, snapshot.CalculatedBalance)
	}
}

// Seeding on top of existing data would mix benchmark wallets with whatever
// was there; it must refuse unless asked to truncate.
func TestSeedRefusesANonEmptyDatabase(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	conn := connect(ctx, t, pgtest.FreshURL(t))

	if _, err := seed.Run(ctx, conn, options(t, 250, false), &bytes.Buffer{}); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	_, err := seed.Run(ctx, conn, options(t, 250, false), &bytes.Buffer{})
	if !errors.Is(err, seed.ErrNotEmpty) {
		t.Fatalf("second Run error = %v, want ErrNotEmpty", err)
	}
	if got := count(ctx, t, conn, `SELECT count(*) FROM wallets`); got != 250 {
		t.Fatalf("wallets after a refused Run = %d, want 250", got)
	}

	if _, err := seed.Run(ctx, conn, options(t, 250, true), &bytes.Buffer{}); err != nil {
		t.Fatalf("truncating Run: %v", err)
	}
	if got := count(ctx, t, conn, `SELECT count(*) FROM wallets`); got != 250 {
		t.Fatalf("wallets after a truncating Run = %d, want 250", got)
	}
}
