package postgres

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/testsupport/pgtest"
)

// The idempotency lookups run before every submission, so a lookup that
// cannot use its unique index reads the whole transactions table each time.
func TestLookupQueriesUseUniqueIndexes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db, err := Open(ctx, pgtest.FreshURL(t))
	if err != nil {
		t.Fatalf("open PostgreSQL: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Plans depend on statistics: on an empty table the planner cannot tell
	// the two unique indexes apart. Give it the shape the service sees, many
	// wallets with their openings and one provider's bets, and analyze.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO wallets (id, player_id, currency, balance_minor_units)
		SELECT gen_random_uuid(), gen_random_uuid(), 'BRL', 100000 FROM generate_series(1, 2000);
		INSERT INTO transactions (id, wallet_id, player_id, kind, amount_minor_units, currency, status)
		SELECT gen_random_uuid(), id, player_id, 'OPENING', 100000, 'BRL', 'PROCESSED' FROM wallets;
		INSERT INTO transactions (id, provider_id, external_id, idempotency_key, payload_hash,
		                          wallet_id, player_id, round_id, game_id, kind, amount_minor_units, currency, status)
		SELECT gen_random_uuid(), 'provider', 'external-' || n, 'key-' || n, 'hash',
		       w.id, w.player_id, 'round', 'game', 'BET', 100, 'BRL', 'PROCESSED'
		FROM (SELECT id, player_id, row_number() OVER () AS n FROM wallets) w;
		ANALYZE wallets, transactions;
	`); err != nil {
		t.Fatalf("seed representative data: %v", err)
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("take a connection: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// With sequential scans priced out, the planner picks an index whenever
	// one can serve the query; a plan without one means none can.
	if _, err := conn.ExecContext(ctx, "SET enable_seqscan = off"); err != nil {
		t.Fatalf("disable sequential scans: %v", err)
	}

	for _, tc := range []struct {
		name  string
		query string
		index string
	}{
		{"idempotency key", transactionIDByIdempotencyKeyQuery, "idx_transactions_idempotency_unique"},
		{"external ID", transactionIDByExternalIDQuery, "idx_transactions_external_id_unique"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var raw []byte
			if err := conn.QueryRowContext(ctx, "EXPLAIN (FORMAT JSON) "+tc.query, "provider", "key").Scan(&raw); err != nil {
				t.Fatalf("explain: %v", err)
			}

			var plans []struct {
				Plan planNode `json:"Plan"`
			}
			if err := json.Unmarshal(raw, &plans); err != nil {
				t.Fatalf("decode plan: %v", err)
			}

			indexes := plans[0].Plan.indexNames()
			if !slices.Contains(indexes, tc.index) {
				t.Fatalf("plan uses indexes %v, want %s:\n%s", indexes, tc.index, raw)
			}
		})
	}
}

type planNode struct {
	IndexName string     `json:"Index Name"`
	Plans     []planNode `json:"Plans"`
}

func (n planNode) indexNames() []string {
	var names []string
	if n.IndexName != "" {
		names = append(names, n.IndexName)
	}
	for _, child := range n.Plans {
		names = append(names, child.indexNames()...)
	}
	return names
}
