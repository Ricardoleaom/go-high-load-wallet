package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/adapters/postgres"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/engine"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/engine/enginetest"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/events"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/money"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/wager"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/testsupport/pgtest"
)

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// openPool opens a pool on url, closed when the test ends.
func openPool(t *testing.T, url string, opts ...postgres.Option) *pgxpool.Pool {
	t.Helper()
	pool, err := postgres.Open(testContext(t), url, opts...)
	if err != nil {
		t.Fatalf("open PostgreSQL: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// harness reads back what the engine wrote with plain SQL, independent of
// the adapter's own read side.
type harness struct {
	pool   *pgxpool.Pool
	engine *postgres.Engine
}

func newHarness(t *testing.T, url string, opts ...postgres.Option) harness {
	t.Helper()
	pool := openPool(t, url, opts...)
	return harness{pool: pool, engine: postgres.NewEngine(pool)}
}

func (h harness) Engine() engine.Engine { return h.engine }

func (h harness) Events(t *testing.T, transactionID string) []events.Envelope {
	t.Helper()
	rows, err := h.pool.Query(testContext(t), `
		SELECT payload FROM outbox_events
		WHERE payload->'data'->>'transactionId' = $1
		ORDER BY occurred_at, causation_id NULLS FIRST`, transactionID)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	payloads, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	out := make([]events.Envelope, 0, len(payloads))
	for _, p := range payloads {
		var e events.Envelope
		if err := json.Unmarshal(p, &e); err != nil {
			t.Fatalf("decode event %s: %v", p, err)
		}
		out = append(out, e)
	}
	return out
}

func (h harness) Transaction(t *testing.T, id string) wager.Transaction {
	t.Helper()
	var (
		in                                     wager.Input
		kind, status, currency                 string
		amount                                 int64
		provider, external, key, hash          *string
		round, game, reference, failure, refID *string
		createdAt, updatedAt                   time.Time
	)
	err := h.pool.QueryRow(testContext(t), `
		SELECT id::text, provider_id, external_id, idempotency_key, payload_hash, wallet_id::text, player_id::text,
		       round_id, game_id, kind, amount_minor_units, currency, reference_external_id, reference_id,
		       status, failure_code, created_at, updated_at
		FROM transactions WHERE id = $1`, id,
	).Scan(&in.ID, &provider, &external, &key, &hash, &in.WalletID, &in.PlayerID, &round, &game, &kind, &amount,
		&currency, &reference, &refID, &status, &failure, &createdAt, &updatedAt)
	if err != nil {
		t.Fatalf("read transaction %s: %v", id, err)
	}
	deref := func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	}
	in.ProviderID, in.ExternalID, in.IdempotencyKey, in.PayloadHash = deref(provider), deref(external), deref(key), deref(hash)
	in.RoundID, in.GameID, in.ReferenceExternalID, in.Kind = deref(round), deref(game), deref(reference), wager.Kind(kind)
	in.Amount = minorUnits(t, amount, currency)
	tx, err := wager.Rehydrate(in, wager.Status(status), deref(failure), deref(refID), createdAt, updatedAt)
	if err != nil {
		t.Fatalf("rehydrate transaction %s: %v", id, err)
	}
	return tx
}

func (h harness) Wallet(t *testing.T, id string) (money.Money, int64) {
	t.Helper()
	var balance, version int64
	var currency string
	if err := h.pool.QueryRow(testContext(t),
		`SELECT balance_minor_units, currency, version FROM wallets WHERE id = $1`, id,
	).Scan(&balance, &currency, &version); err != nil {
		t.Fatalf("read wallet %s: %v", id, err)
	}
	return minorUnits(t, balance, currency), version
}

func (h harness) LedgerSum(t *testing.T, walletID string) int64 {
	t.Helper()
	var sum int64
	if err := h.pool.QueryRow(testContext(t),
		`SELECT COALESCE(sum(amount_delta_minor_units), 0)::bigint FROM ledger_entries WHERE wallet_id = $1`, walletID,
	).Scan(&sum); err != nil {
		t.Fatalf("sum ledger of %s: %v", walletID, err)
	}
	return sum
}

func (h harness) LedgerCount(t *testing.T, transactionID string) int {
	t.Helper()
	var n int
	if err := h.pool.QueryRow(testContext(t),
		`SELECT count(*) FROM ledger_entries WHERE transaction_id = $1`, transactionID,
	).Scan(&n); err != nil {
		t.Fatalf("count ledger entries of %s: %v", transactionID, err)
	}
	return n
}

// minorUnits builds the Money a stored non-negative amount stands for.
func minorUnits(t *testing.T, units int64, currency string) money.Money {
	t.Helper()
	m, err := money.New(fmt.Sprintf("%d.%02d", units/100, units%100), currency)
	if err != nil {
		t.Fatalf("money from %d %s: %v", units, currency, err)
	}
	return m
}

func TestEngineHonoursTheEngineContract(t *testing.T) {
	enginetest.Run(t, newHarness(t, pgtest.FreshURL(t)))
}

func TestUnknownCommandIsRejected(t *testing.T) {
	h := newHarness(t, pgtest.URL(t))
	if _, err := h.engine.Execute(testContext(t), nil); !errors.Is(err, engine.ErrUnknownCommand) {
		t.Fatalf("err = %v, want ErrUnknownCommand", err)
	}
}
