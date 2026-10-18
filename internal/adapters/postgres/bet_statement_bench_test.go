package postgres_test

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/adapters/postgres"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/engine"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/bench/seed"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/money"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/wager"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/testsupport/pgtest"
)

// betCTE is the alternative ADR 0014 measures the function against: a BET
// as one statement of data-modifying CTEs, with the function's semantics
// for a bet (lookup before the lock, the wallet locked FOR NO KEY UPDATE,
// classification on the locked row, the transaction in its final status,
// the movement, the ledger entry and both events). A lost race, which the
// function settles in the same call, would cost the CTE a second round
// trip; reversals do not fit one CTE at all.
const betCTE = `
WITH existing AS (
    SELECT f.id, f.status, f.failure_code, f.idempotency_key, f.payload_hash
    FROM (
        SELECT t.*, 0 AS rank FROM transactions t
         WHERE t.provider_id = $5 AND t.idempotency_key = $7 AND t.kind <> 'OPENING'
        UNION ALL
        SELECT t.*, 1 AS rank FROM transactions t
         WHERE t.provider_id = $5 AND t.external_id = $6 AND t.kind <> 'OPENING'
    ) f
    ORDER BY f.rank
    LIMIT 1
),
w AS (
    SELECT id, player_id, currency, balance_minor_units
    FROM wallets
    WHERE id = $9 AND NOT EXISTS (SELECT 1 FROM existing)
    FOR NO KEY UPDATE
),
decided AS (
    SELECT CASE WHEN w.currency <> $14 THEN 'CURRENCY_MISMATCH'
                WHEN w.balance_minor_units < $13 THEN 'INSUFFICIENT_BALANCE' END AS failure,
           clock_timestamp() AS at
    FROM w
    WHERE w.player_id = $10
),
tx AS (
    INSERT INTO transactions (id, provider_id, external_id, idempotency_key, payload_hash, wallet_id, player_id,
        round_id, game_id, kind, amount_minor_units, currency, status, failure_code, created_at, updated_at)
    SELECT $1, $5, $6, $7, $8, $9, $10, $11, $12, 'BET', $13, $14,
           CASE WHEN d.failure IS NULL THEN 'PROCESSED' ELSE 'REJECTED' END, d.failure, d.at, d.at
    FROM decided d
    ON CONFLICT DO NOTHING
    RETURNING status, failure_code, created_at
),
moved AS (
    UPDATE wallets
       SET balance_minor_units = balance_minor_units - $13, version = version + 1, updated_at = tx.created_at
      FROM tx
     WHERE wallets.id = $9 AND tx.status = 'PROCESSED'
    RETURNING wallets.balance_minor_units AS after, wallets.version, tx.created_at AS at
),
entry AS (
    INSERT INTO ledger_entries (id, wallet_id, transaction_id, direction, amount_delta_minor_units,
        balance_before_minor_units, balance_after_minor_units, created_at)
    SELECT $2, $9, $1, 'DEBIT', -$13, m.after + $13, m.after, m.at FROM moved m
),
events AS (
    INSERT INTO outbox_events (id, aggregate_id, event_type, payload, correlation_id, causation_id, occurred_at)
    SELECT $3::uuid, $1::uuid::text, e.type,
           jsonb_build_object('eventId', $3::uuid, 'eventType', e.type, 'aggregateId', $1::uuid,
               'correlationId', $15::text,
               'occurredAt', to_char(tx.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
               'version', 1, 'data', e.data),
           $15, NULL, tx.created_at
    FROM tx, LATERAL (
        SELECT CASE WHEN tx.status = 'PROCESSED' THEN 'WagerTransactionProcessed' ELSE 'WagerTransactionRejected' END AS type,
               CASE WHEN tx.status = 'PROCESSED'
                    THEN jsonb_build_object('transactionId', $1::uuid, 'walletId', $9::uuid, 'kind', 'BET',
                         'money', jsonb_build_object('amount', wallet_money_text($13), 'currency', $14::text))
                    ELSE jsonb_build_object('transactionId', $1::uuid, 'failureCode', tx.failure_code) END AS data
    ) e
    UNION ALL
    SELECT $4::uuid, $9::uuid::text, 'WalletBalanceChanged',
           jsonb_build_object('eventId', $4::uuid, 'eventType', 'WalletBalanceChanged', 'aggregateId', $9::uuid,
               'correlationId', $15::text, 'causationId', $3::uuid,
               'occurredAt', to_char(m.at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'), 'version', 1,
               'data', jsonb_build_object('walletId', $9::uuid, 'transactionId', $1::uuid, 'direction', 'DEBIT',
                   'money', jsonb_build_object('amount', wallet_money_text($13), 'currency', $14::text),
                   'balanceBefore', jsonb_build_object('amount', wallet_money_text(m.after + $13), 'currency', $14::text),
                   'balanceAfter', jsonb_build_object('amount', wallet_money_text(m.after), 'currency', $14::text),
                   'walletVersion', m.version)),
           $15, $3::uuid::text, m.at
    FROM moved m
)
SELECT
    CASE WHEN EXISTS (SELECT 1 FROM existing WHERE idempotency_key <> $7) THEN 'KEY_MISMATCH'
         WHEN EXISTS (SELECT 1 FROM existing WHERE payload_hash <> $8) THEN 'IDEMPOTENCY_CONFLICT'
         WHEN EXISTS (SELECT 1 FROM existing) THEN 'REPLAY'
         WHEN NOT EXISTS (SELECT 1 FROM decided) THEN 'WALLET_NOT_FOUND'
         WHEN NOT EXISTS (SELECT 1 FROM tx) THEN 'RACED'
         ELSE 'APPLIED' END,
    COALESCE((SELECT status FROM tx), (SELECT status FROM existing)),
    COALESCE((SELECT failure_code FROM tx), (SELECT failure_code FROM existing)),
    COALESCE((SELECT after FROM moved),
             (SELECT le.balance_after_minor_units FROM ledger_entries le JOIN existing e ON le.transaction_id = e.id))
`

type cteAnswer struct {
	outcome string
	status  *string
	failure *string
	balance *int64
}

func betByCTE(ctx context.Context, pool *pgxpool.Pool, cmd engine.SubmitWager) (cteAnswer, error) {
	var a cteAnswer
	correlation := cmd.CorrelationID
	transactionID := uuid.Must(uuid.NewV7()).String()
	if correlation == "" {
		correlation = transactionID
	}
	err := pool.QueryRow(ctx, betCTE,
		transactionID, uuid.Must(uuid.NewV7()).String(), uuid.Must(uuid.NewV7()).String(), uuid.Must(uuid.NewV7()).String(),
		cmd.ProviderID, cmd.ExternalID, cmd.IdempotencyKey, cmd.PayloadHash,
		cmd.WalletID, cmd.PlayerID, cmd.RoundID, cmd.GameID,
		cmd.Amount.AmountInMinorUnits(), cmd.Amount.Currency(), correlation,
	).Scan(&a.outcome, &a.status, &a.failure, &a.balance)
	return a, err
}

func (a cteAnswer) String() string {
	deref := func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	}
	balance := "-"
	if a.balance != nil {
		balance = fmt.Sprint(*a.balance)
	}
	return fmt.Sprintf("%s|%s|%s", deref(a.status), deref(a.failure), balance)
}

func functionAnswer(out engine.Outcome) string {
	balance := "-"
	if out.Balance.Currency() != "" {
		balance = fmt.Sprint(out.Balance.AmountInMinorUnits())
	}
	return fmt.Sprintf("%s|%s|%s", out.Status, out.FailureCode, balance)
}

// The CTE only counts as an alternative if it does what the function does.
func TestBetStatementVariantsAgree(t *testing.T) {
	h := newHarness(t, pgtest.FreshURL(t))
	c := newCommands(t)
	ctx := testContext(t)
	byFunction, playerA := openWallet(t, h.engine, "100.00")
	byCTE, playerB := openWallet(t, h.engine, "100.00")

	for i := range 200 {
		amount := fmt.Sprintf("%d.%02d", (i*37%1500+1)/100, (i*37%1500+1)%100)
		viaFunction := mustExecute(t, h.engine, c.bet(byFunction, playerA, amount))
		cteCmd := c.bet(byCTE, playerB, amount)
		viaCTE, err := betByCTE(ctx, h.pool, cteCmd)
		if err != nil {
			t.Fatalf("bet %d by CTE: %v", i, err)
		}
		if viaCTE.outcome != "APPLIED" || viaCTE.String() != functionAnswer(viaFunction) {
			t.Fatalf("bet %d of %s: function %s, CTE %s %s", i, amount, functionAnswer(viaFunction), viaCTE.outcome, viaCTE)
		}
		if i%3 == 0 {
			if replay, err := betByCTE(ctx, h.pool, cteCmd); err != nil || replay.outcome != "REPLAY" || replay.String() != viaCTE.String() {
				t.Fatalf("CTE replay of bet %d = %s %s, %v", i, replay.outcome, replay, err)
			}
		}
		if i%5 == 0 {
			if win := mustExecute(t, h.engine, c.wager(byFunction, playerA, wager.KindWin, "3.00", "")); win.Status != wager.StatusProcessed {
				t.Fatal(win)
			}
			if _, err := h.engine.Execute(ctx, c.wager(byCTE, playerB, wager.KindWin, "3.00", "")); err != nil {
				t.Fatal(err)
			}
		}
	}

	balanceA, versionA := h.Wallet(t, byFunction)
	balanceB, versionB := h.Wallet(t, byCTE)
	if balanceA.String() != balanceB.String() || versionA != versionB {
		t.Errorf("function left %s v%d, CTE %s v%d", balanceA, versionA, balanceB, versionB)
	}
	if h.LedgerSum(t, byFunction) != h.LedgerSum(t, byCTE) {
		t.Error("the two ledgers differ")
	}

	// The events agree too, apart from the identifiers and times.
	const shape = `
		SELECT string_agg(event_type || ':' || ((payload - 'eventId' - 'occurredAt' - 'aggregateId' - 'correlationId' - 'causationId')
		       || jsonb_build_object('data', (payload->'data') - 'transactionId' - 'walletId'))::text, ',' ORDER BY occurred_at, causation_id NULLS FIRST)
		FROM outbox_events WHERE payload->'data'->>'walletId' = $1
		   OR payload->'data'->>'transactionId' IN (SELECT id::text FROM transactions WHERE wallet_id::text = $1)`
	var eventsA, eventsB string
	if err := h.pool.QueryRow(ctx, shape, byFunction).Scan(&eventsA); err != nil {
		t.Fatal(err)
	}
	if err := h.pool.QueryRow(ctx, shape, byCTE).Scan(&eventsB); err != nil {
		t.Fatal(err)
	}
	if eventsA != eventsB {
		t.Errorf("the events differ:\nfunction %.300s\nCTE      %.300s", eventsA, eventsB)
	}
}

// BenchmarkBetStatement measures a fresh bet, and the replay of one, as the
// engine's function and as the CTE, on one connection and on eight, over
// 10,000 seeded wallets, with every commit durable as in production.
func BenchmarkBetStatement(b *testing.B) {
	benchmarkBetStatement(b, pgtest.FreshURL(b))
}

func benchmarkBetStatement(b *testing.B, url string) {
	pool := openPool(b, url, postgres.WithMaxConns(8), postgres.WithMinConns(8))
	conn, err := pool.Acquire(context.Background())
	if err != nil {
		b.Fatal(err)
	}
	balance, _ := money.New("1000000000.00", "BRL")
	var out bytes.Buffer
	if _, err := seed.Run(context.Background(), conn.Conn(), seed.Options{Wallets: 10_000, Balance: balance}, &out); err != nil {
		b.Fatal(err)
	}
	conn.Release()
	records, err := csv.NewReader(&out).ReadAll()
	if err != nil {
		b.Fatal(err)
	}
	wallets := records[1:]
	if _, err := pool.Exec(context.Background(), `VACUUM ANALYZE`); err != nil {
		b.Fatal(err)
	}

	e := postgres.NewEngine(pool)
	amount, _ := money.New("1.00", "BRL")
	bet := func() engine.SubmitWager {
		w := wallets[rand.IntN(len(wallets))] //nolint:gosec // spreads load over wallets; nothing secret
		external := "ext-" + uuid.NewString()
		return engine.SubmitWager{
			ProviderID: "bench", ExternalID: external, IdempotencyKey: "key-" + external, PayloadHash: "hash-" + external,
			WalletID: w[0], PlayerID: w[1], RoundID: "round", GameID: "game", Kind: wager.KindBet, Amount: amount,
		}
	}
	variants := map[string]func(engine.SubmitWager) error{
		"function": func(cmd engine.SubmitWager) error {
			out, err := e.Execute(context.Background(), cmd)
			if err == nil && out.Status != wager.StatusProcessed {
				err = fmt.Errorf("bet %s", out.Status)
			}
			return err
		},
		"cte": func(cmd engine.SubmitWager) error {
			a, err := betByCTE(context.Background(), pool, cmd)
			if err == nil && a.outcome != "APPLIED" && a.outcome != "REPLAY" {
				err = fmt.Errorf("bet %s", a.outcome)
			}
			return err
		},
	}

	names := []string{"function", "cte"}
	if os.Getenv("BET_STATEMENT_FIRST") == "cte" {
		names = []string{"cte", "function"}
	}
	for _, name := range names {
		run := variants[name]
		b.Run(name+"/bet/serial", func(b *testing.B) {
			for range b.N {
				if err := run(bet()); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(name+"/bet/parallel8", func(b *testing.B) {
			b.SetParallelism(2)
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					if err := run(bet()); err != nil {
						b.Error(err)
						return
					}
				}
			})
		})
		b.Run(name+"/replay/serial", func(b *testing.B) {
			cmd := bet()
			if err := run(cmd); err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			for range b.N {
				if err := run(cmd); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// inServerBets is a test-only function that places n bets, or replays one
// bet n times, in one call: one round trip and one commit for all of them,
// so what it measures is executing each form, without the network and the
// WAL flush that cost both forms the same.
func inServerBets(cte string) string {
	// The CTE's parameters become the loop's variables.
	body := strings.NewReplacer(
		"$15", "p15", "$14", "p14", "$13", "p13", "$12", "p12", "$11", "p11", "$10", "p10",
		"$9", "p9", "$8", "p8", "$7", "p7", "$6", "p6", "$5", "p5", "$4", "p4", "$3", "p3", "$2", "p2", "$1", "p1",
	).Replace(strings.TrimSpace(cte))
	return `
CREATE FUNCTION bench_bets(n int, variant text, replay boolean) RETURNS void LANGUAGE plpgsql AS $fn$
DECLARE
    wallet_ids uuid[];
    player_ids uuid[];
    pick int;
    p1 uuid; p2 uuid; p3 uuid; p4 uuid; p5 text; p6 text; p7 text; p8 text;
    p9 uuid; p10 uuid; p11 text; p12 text; p13 bigint; p14 text; p15 text;
    v_outcome text; v_status text; v_failure text; v_balance bigint;
    r wager_result;
BEGIN
    SELECT array_agg(id ORDER BY id), array_agg(player_id ORDER BY id) INTO wallet_ids, player_ids FROM wallets;
    p5 := 'bench'; p8 := 'hash'; p11 := 'round'; p12 := 'game'; p13 := 100; p14 := 'BRL';
    FOR i IN 1..n LOOP
        IF NOT replay OR i = 1 THEN
            pick := 1 + floor(random() * array_length(wallet_ids, 1))::int;
            p9 := wallet_ids[pick]; p10 := player_ids[pick];
            p6 := 'ext-' || gen_random_uuid(); p7 := 'key-' || p6; p8 := 'hash-' || p6;
        END IF;
        p1 := gen_random_uuid(); p2 := gen_random_uuid(); p3 := gen_random_uuid(); p4 := gen_random_uuid();
        p15 := p1::text;
        IF variant = 'function' THEN
            SELECT * INTO r FROM wager_submit(p1, p2, p3, p4, p5, p6, p7, p8, p9, p10, p11, p12, 'BET', p13, p14, '', p15,
                                              interval '5 seconds');
        ELSE
            ` + body + `
            INTO v_outcome, v_status, v_failure, v_balance;
        END IF;
    END LOOP;
END
$fn$`
}

// BenchmarkBetStatementInServer runs b.N bets, or b.N replays of one bet,
// inside PostgreSQL in one call per form.
func BenchmarkBetStatementInServer(b *testing.B) {
	pool := openPool(b, pgtest.FreshURL(b), postgres.WithMaxConns(2))
	conn, err := pool.Acquire(context.Background())
	if err != nil {
		b.Fatal(err)
	}
	balance, _ := money.New("1000000000.00", "BRL")
	if _, err := seed.Run(context.Background(), conn.Conn(), seed.Options{Wallets: 10_000, Balance: balance}, &bytes.Buffer{}); err != nil {
		b.Fatal(err)
	}
	conn.Release()
	for _, statement := range []string{inServerBets(betCTE), `VACUUM ANALYZE`} {
		if _, err := pool.Exec(context.Background(), statement); err != nil {
			b.Fatal(err)
		}
	}

	names := []string{"function", "cte"}
	if os.Getenv("BET_STATEMENT_FIRST") == "cte" {
		names = []string{"cte", "function"}
	}
	for _, name := range names {
		for _, replay := range []bool{false, true} {
			path := "bet"
			if replay {
				path = "replay"
			}
			b.Run(name+"/"+path, func(b *testing.B) {
				if _, err := pool.Exec(context.Background(), `SELECT bench_bets($1, $2, $3)`, b.N, name, replay); err != nil {
					b.Fatal(err)
				}
			})
		}
	}
}
