package verify_test

import (
	"bytes"
	"context"
	"database/sql"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/application/wager"
	walletapp "github.com/Ricardoleaom/go-high-load-wallet/internal/application/wallet"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/bench/seed"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/bench/verify"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/money"
	domainwager "github.com/Ricardoleaom/go-high-load-wallet/internal/domain/wager"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/infrastructure/postgres"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/testsupport/pgtest"
)

// seeded returns a fresh database holding n seeded wallets, and their IDs.
func seeded(ctx context.Context, t *testing.T, n int) (*sql.DB, []string) {
	t.Helper()
	url := pgtest.FreshURL(t)

	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	balance := mustMoney(t, "1000.00")
	if _, err := seed.Run(ctx, conn, seed.Options{Wallets: n, Balance: balance}, &bytes.Buffer{}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	db, err := postgres.Open(ctx, url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	rows, err := db.QueryContext(ctx, `SELECT id::text FROM wallets ORDER BY id`)
	if err != nil {
		t.Fatalf("list wallets: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan wallet: %v", err)
		}
		ids = append(ids, id)
	}
	return db, ids
}

func mustMoney(t *testing.T, amount string) money.Money {
	t.Helper()
	m, err := money.New(amount, "BRL")
	if err != nil {
		t.Fatalf("money %s: %v", amount, err)
	}
	return m
}

func check(t *testing.T, report verify.Report, name string) verify.Check {
	t.Helper()
	for _, c := range report.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("report has no %q check: %+v", name, report.Checks)
	return verify.Check{}
}

func TestVerifyPassesOnASeededDatabase(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	db, _ := seeded(ctx, t, 20)

	report, err := verify.Run(ctx, db)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.OK() {
		t.Fatalf("seeded database violates invariants: %+v", report.Checks)
	}
	if len(report.Checks) != 8 {
		t.Fatalf("ran %d checks, want 8", len(report.Checks))
	}
}

func TestVerifyPassesAfterRealTraffic(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	db, _ := seeded(ctx, t, 1)

	wallets := postgres.NewWalletRepository(db)
	transactions := postgres.NewTransactionRepository(db)
	ledgerRepo := postgres.NewLedgerRepository(db)
	outbox := postgres.NewOutboxRepository(db)
	opened, err := walletapp.NewService(db, wallets, transactions, ledgerRepo, outbox).
		Open(ctx, walletapp.OpenInput{PlayerID: uuid.NewString(), InitialBalance: mustMoney(t, "100.00")})
	if err != nil {
		t.Fatalf("open wallet: %v", err)
	}
	service := wager.NewService(db, wallets, transactions, ledgerRepo, outbox)

	submit := func(kind domainwager.Kind, amount, reference string) string {
		externalID := uuid.NewString()
		_, err := service.Submit(ctx, wager.SubmitInput{
			ProviderID: "provider-a", ExternalID: externalID, IdempotencyKey: "provider-a:" + externalID,
			PlayerID: opened.PlayerID(), WalletID: opened.ID(), RoundID: "round", GameID: "game",
			Kind: kind, Amount: mustMoney(t, amount), ReferenceExternalID: reference,
		})
		if err != nil {
			t.Fatalf("submit %s: %v", kind, err)
		}
		return externalID
	}
	bet := submit(domainwager.KindBet, "10.00", "")
	submit(domainwager.KindWin, "5.00", "")
	submit(domainwager.KindLoss, "0.00", "")
	submit(domainwager.KindRefund, "10.00", bet)
	submit(domainwager.KindBet, "999.00", "") // rejected: insufficient balance

	report, err := verify.Run(ctx, db)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.OK() {
		t.Fatalf("real traffic violates invariants: %+v", report.Checks)
	}
}

func TestVerifyDetectsEachViolation(t *testing.T) {
	insertTransaction := func(ctx context.Context, t *testing.T, db *sql.DB, walletID, kind, status string) string {
		t.Helper()
		id := uuid.NewString()
		_, err := db.ExecContext(ctx, `
			INSERT INTO transactions (id, provider_id, external_id, idempotency_key, payload_hash,
				wallet_id, player_id, round_id, game_id, kind, amount_minor_units, status)
			SELECT $1::uuid, 'provider-a', $1::text, $1::text, 'hash', w.id, w.player_id, 'round', 'game', $3, 100, $4
			FROM wallets w WHERE w.id = $2
		`, id, walletID, kind, status)
		if err != nil {
			t.Fatalf("insert %s %s transaction: %v", status, kind, err)
		}
		return id
	}

	cases := []struct {
		name    string
		checks  []string
		corrupt func(ctx context.Context, t *testing.T, db *sql.DB, walletID string) string
	}{
		{"balance changed outside the ledger", []string{"balance_matches_ledger"},
			func(ctx context.Context, t *testing.T, db *sql.DB, walletID string) string {
				if _, err := db.ExecContext(ctx, `UPDATE wallets SET balance_minor_units = balance_minor_units + 1 WHERE id = $1`, walletID); err != nil {
					t.Fatalf("tamper balance: %v", err)
				}
				return walletID
			}},
		{"processed bet without entry or event", []string{"processed_movement_has_one_entry", "terminal_transaction_has_event"},
			func(ctx context.Context, t *testing.T, db *sql.DB, walletID string) string {
				return insertTransaction(ctx, t, db, walletID, "BET", "PROCESSED")
			}},
		{"transaction left pending", []string{"no_pending_transactions"},
			func(ctx context.Context, t *testing.T, db *sql.DB, walletID string) string {
				return insertTransaction(ctx, t, db, walletID, "BET", "PENDING")
			}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			db, wallets := seeded(ctx, t, 5)
			culprit := c.corrupt(ctx, t, db, wallets[0])

			report, err := verify.Run(ctx, db)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if report.OK() {
				t.Fatal("verify passed a corrupted database")
			}
			for _, name := range c.checks {
				got := check(t, report, name)
				if got.Violations < 1 || !slices.Contains(got.Samples, culprit) {
					t.Fatalf("%s: violations=%d samples=%v, want %s among them", name, got.Violations, got.Samples, culprit)
				}
			}
		})
	}
}
