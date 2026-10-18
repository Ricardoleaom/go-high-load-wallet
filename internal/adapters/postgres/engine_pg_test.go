package postgres_test

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/adapters/postgres"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/engine"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/bench/seed"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/money"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/wager"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/testsupport/pgtest"
)

// commands builds submissions for one provider of its own.
type commands struct {
	t        *testing.T
	provider string
}

func newCommands(t *testing.T) commands {
	return commands{t: t, provider: "provider-" + uuid.NewString()[:8]}
}

func (c commands) wager(walletID, playerID string, kind wager.Kind, amount, reference string) engine.SubmitWager {
	c.t.Helper()
	m, err := money.New(amount, "BRL")
	if err != nil {
		c.t.Fatal(err)
	}
	external := "ext-" + uuid.NewString()
	return engine.SubmitWager{
		ProviderID: c.provider, ExternalID: external, IdempotencyKey: "key-" + external,
		PayloadHash: "hash-" + external, WalletID: walletID, PlayerID: playerID,
		RoundID: "round-1", GameID: "game-1", Kind: kind, Amount: m, ReferenceExternalID: reference,
	}
}

func (c commands) bet(walletID, playerID, amount string) engine.SubmitWager {
	c.t.Helper()
	return c.wager(walletID, playerID, wager.KindBet, amount, "")
}

func (c commands) reversal(kind wager.Kind, of engine.SubmitWager) engine.SubmitWager {
	c.t.Helper()
	return c.wager(of.WalletID, of.PlayerID, kind, of.Amount.DecimalString(), of.ExternalID)
}

func mustExecute(t *testing.T, e engine.Engine, cmd engine.Command) engine.Outcome {
	t.Helper()
	out, err := e.Execute(testContext(t), cmd)
	if err != nil {
		t.Fatalf("Execute(%T): %v", cmd, err)
	}
	return out
}

func openWallet(t *testing.T, e engine.Engine, balance string) (walletID, playerID string) {
	t.Helper()
	m, err := money.New(balance, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	playerID = uuid.NewString()
	return mustExecute(t, e, engine.OpenWallet{PlayerID: playerID, InitialBalance: m}).Wallet.ID(), playerID
}

// requireBalance checks a wallet's stored balance and that its ledger adds
// up to it.
func requireBalance(t *testing.T, h harness, walletID, want string) {
	t.Helper()
	balance, _ := h.Wallet(t, walletID)
	if balance.DecimalString() != want {
		t.Errorf("balance = %s, want %s", balance.DecimalString(), want)
	}
	if sum := h.LedgerSum(t, walletID); sum != balance.AmountInMinorUnits() {
		t.Errorf("ledger sums to %d, balance is %d", sum, balance.AmountInMinorUnits())
	}
}

// The deadlock 002 found came from holding one pooled connection while
// waiting for another. With one connection in the pool, any command that
// still did that would hang until its deadline.
func TestEngineWorksWithASingleConnection(t *testing.T) {
	h := newHarness(t, pgtest.URL(t), postgres.WithMaxConns(1))
	c := newCommands(t)
	walletID, playerID := openWallet(t, h.engine, "100.00")

	bet := c.bet(walletID, playerID, "30.00")
	mustExecute(t, h.engine, bet)
	mustExecute(t, h.engine, bet)
	mustExecute(t, h.engine, c.reversal(wager.KindRefund, bet))

	later := c.bet(walletID, playerID, "10.00")
	pending := mustExecute(t, h.engine, c.reversal(wager.KindRollback, later))
	mustExecute(t, h.engine, later)
	resolved := mustExecute(t, h.engine, engine.RetryReference{TransactionID: pending.TransactionID, Attempts: 1})
	if resolved.Status != wager.StatusProcessed {
		t.Fatalf("retried rollback = %s", resolved.Status)
	}
	requireBalance(t, h, walletID, "100.00")
}

// countingTracer counts the statements sent to PostgreSQL.
type countingTracer struct{ queries atomic.Int64 }

func (c *countingTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.queries.Add(1)
	return ctx
}

func (*countingTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// Every command is one statement in autocommit: one round trip and one
// commit, where phase 2a needed about eleven round trips and four commits
// for a bet. A BEGIN or COMMIT would be counted as a statement too.
func TestEveryCommandIsOneStatement(t *testing.T) {
	config, err := pgxpool.ParseConfig(pgtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	tracer := &countingTracer{}
	config.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(testContext(t), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	e := postgres.NewEngine(pool)
	c := newCommands(t)

	statements := func(name string, cmd engine.Command) engine.Outcome {
		t.Helper()
		before := tracer.queries.Load()
		out, err := e.Execute(testContext(t), cmd)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if n := tracer.queries.Load() - before; n != 1 {
			t.Errorf("%s took %d statements, want 1", name, n)
		}
		return out
	}

	m, _ := money.New("100.00", "BRL")
	playerID := uuid.NewString()
	walletID := statements("open wallet", engine.OpenWallet{PlayerID: playerID, InitialBalance: m}).Wallet.ID()
	bet := c.bet(walletID, playerID, "30.00")
	statements("bet", bet)
	statements("replay", bet)
	statements("rejected bet", c.bet(walletID, playerID, "500.00"))
	statements("win", c.wager(walletID, playerID, wager.KindWin, "5.00", ""))
	statements("loss", c.wager(walletID, playerID, wager.KindLoss, "0.00", ""))
	statements("refund", c.reversal(wager.KindRefund, bet))
	later := c.bet(walletID, playerID, "10.00")
	pending := statements("pending rollback", c.reversal(wager.KindRollback, later))
	statements("retry", engine.RetryReference{TransactionID: pending.TransactionID, Attempts: 1})
}

// threeEngines returns engines on three separate pools over one database,
// standing for three instances of the service.
func threeEngines(t *testing.T, url string) (harness, []engine.Engine) {
	t.Helper()
	h := newHarness(t, url)
	return h, []engine.Engine{h.engine, newHarness(t, url).engine, newHarness(t, url).engine}
}

func TestConcurrentBetsAcrossThreeIndependentPools(t *testing.T) {
	h, engines := threeEngines(t, pgtest.URL(t))
	c := newCommands(t)
	walletID, playerID := openWallet(t, h.engine, "100.00")

	var processed atomic.Int64
	var wg sync.WaitGroup
	for i := range 30 {
		bet := c.bet(walletID, playerID, "30.00")
		wg.Go(func() {
			out, err := engines[i%3].Execute(testContext(t), bet)
			if err != nil {
				t.Errorf("bet %d: %v", i, err)
				return
			}
			if out.Status == wager.StatusProcessed {
				processed.Add(1)
			}
		})
	}
	wg.Wait()
	if processed.Load() != 3 {
		t.Errorf("%d bets processed, want 3", processed.Load())
	}
	requireBalance(t, h, walletID, "10.00")
}

func TestDuplicatesAcrossThreeIndependentPoolsMoveMoneyOnce(t *testing.T) {
	h, engines := threeEngines(t, pgtest.URL(t))
	c := newCommands(t)
	walletID, playerID := openWallet(t, h.engine, "100.00")
	bet := c.bet(walletID, playerID, "30.00")

	ids := make([]string, 50)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Go(func() {
			out, err := engines[i%3].Execute(testContext(t), bet)
			if err != nil {
				t.Errorf("submission %d: %v", i, err)
				return
			}
			ids[i] = out.TransactionID
		})
	}
	wg.Wait()
	for i, id := range ids {
		if id != ids[0] {
			t.Fatalf("submission %d answered %s, the first %s", i, id, ids[0])
		}
	}
	requireBalance(t, h, walletID, "70.00")
	if n := h.LedgerCount(t, ids[0]); n != 1 {
		t.Errorf("bet has %d ledger entries, want 1", n)
	}
}

func TestDistinctWalletsProcessConcurrently(t *testing.T) {
	h, engines := threeEngines(t, pgtest.URL(t))
	c := newCommands(t)

	walletIDs := make([]string, 10)
	bets := make([]engine.SubmitWager, len(walletIDs))
	for i := range walletIDs {
		walletID, playerID := openWallet(t, h.engine, "100.00")
		walletIDs[i], bets[i] = walletID, c.bet(walletID, playerID, "30.00")
	}
	var wg sync.WaitGroup
	for i, bet := range bets {
		wg.Go(func() {
			if out, err := engines[i%3].Execute(testContext(t), bet); err != nil || out.Status != wager.StatusProcessed {
				t.Errorf("bet on wallet %d = %+v, %v", i, out, err)
			}
		})
	}
	wg.Wait()
	for _, walletID := range walletIDs {
		requireBalance(t, h, walletID, "70.00")
	}
}

// A fresh ROLLBACK of a REFUND and the retry worker resolving that same
// REFUND lock the same rows. Both take the REFUND's row before the wallet,
// so they queue instead of deadlocking (40P01) under PostgreSQL's detector.
func TestFreshRollbackRacesTheRetryWorkerWithoutDeadlock(t *testing.T) {
	h, engines := threeEngines(t, pgtest.URL(t))
	c := newCommands(t)

	for round := range 5 {
		walletID, playerID := openWallet(t, h.engine, "100.00")
		bet := c.bet(walletID, playerID, "30.00")
		refund := c.reversal(wager.KindRefund, bet)
		refundID := mustExecute(t, h.engine, refund).TransactionID
		mustExecute(t, h.engine, bet)

		var rollbacks atomic.Int64
		var wg sync.WaitGroup
		for i := range 40 {
			wg.Go(func() {
				var cmd engine.Command = engine.RetryReference{TransactionID: refundID, Attempts: 1}
				if i%2 == 1 {
					cmd = c.reversal(wager.KindRollback, refund)
				}
				out, err := engines[i%3].Execute(testContext(t), cmd)
				var pgErr *pgconn.PgError
				switch {
				case errors.As(err, &pgErr) && pgErr.Code == "40P01":
					t.Errorf("round %d: deadlock: %v", round, err)
				case err != nil:
					t.Errorf("round %d: %v", round, err)
				case i%2 == 1 && out.Status == wager.StatusProcessed:
					rollbacks.Add(1)
				}
			})
		}
		wg.Wait()

		if got := h.Transaction(t, refundID).Status(); got != wager.StatusProcessed {
			t.Errorf("round %d: refund = %s, want PROCESSED", round, got)
		}
		if rollbacks.Load() > 1 {
			t.Errorf("round %d: %d rollbacks of one refund processed", round, rollbacks.Load())
		}
		want := "100.00"
		if rollbacks.Load() == 1 {
			want = "70.00"
		}
		requireBalance(t, h, walletID, want)
	}
}

// seedWallets loads n wallets with openings through the benchmark seeder
// and returns their wallet and player IDs.
func seedWallets(t *testing.T, pool *pgxpool.Pool, n int) [][2]string {
	t.Helper()
	conn, err := pool.Acquire(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	balance, _ := money.New("10000.00", "BRL")
	var out bytes.Buffer
	if _, err := seed.Run(testContext(t), conn.Conn(), seed.Options{Wallets: n, Balance: balance}, &out); err != nil {
		t.Fatalf("seed: %v", err)
	}
	records, err := csv.NewReader(&out).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	ids := make([][2]string, 0, n)
	for _, r := range records[1:] {
		ids = append(ids, [2]string{r[0], r[1]})
	}
	return ids
}

// tableStats reads a table's counters after flushing this connection's
// pending statistics; the pool must have a single connection, so every
// statement the test ran went through it.
func tableStats(t *testing.T, pool *pgxpool.Pool, table string) (seqScans, updates, hotUpdates int64) {
	t.Helper()
	ctx := testContext(t)
	if _, err := pool.Exec(ctx, `SELECT pg_stat_force_next_flush()`); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT seq_scan, n_tup_upd, n_tup_hot_upd FROM pg_stat_user_tables WHERE relname = $1`, table,
	).Scan(&seqScans, &updates, &hotUpdates); err != nil {
		t.Fatalf("read %s statistics: %v", table, err)
	}
	return seqScans, updates, hotUpdates
}

func indexScans(t *testing.T, pool *pgxpool.Pool, index string) int64 {
	t.Helper()
	var scans int64
	if err := pool.QueryRow(testContext(t),
		`SELECT idx_scan FROM pg_stat_user_indexes WHERE indexrelname = $1`, index,
	).Scan(&scans); err != nil {
		t.Fatalf("read %s statistics: %v", index, err)
	}
	return scans
}

// Phase 2a found the idempotency lookups reading the whole transactions
// table. Inside the functions, every lookup must go through an index.
func TestSubmitNeverScansTransactions(t *testing.T) {
	h := newHarness(t, pgtest.FreshURL(t), postgres.WithMaxConns(1), postgres.WithMinConns(1))
	c := newCommands(t)
	wallets := seedWallets(t, h.pool, 2000)
	for i := range 300 {
		w := wallets[i%len(wallets)]
		mustExecute(t, h.engine, c.bet(w[0], w[1], "1.00"))
	}
	if _, err := h.pool.Exec(testContext(t), `ANALYZE`); err != nil {
		t.Fatal(err)
	}

	scansBefore, _, _ := tableStats(t, h.pool, "transactions")
	keyScansBefore := indexScans(t, h.pool, "idx_transactions_idempotency_unique")

	for i := range 10 {
		w := wallets[1000+i]
		bet := c.bet(w[0], w[1], "2.00")
		mustExecute(t, h.engine, bet)
		mustExecute(t, h.engine, bet)
		mustExecute(t, h.engine, c.wager(w[0], w[1], wager.KindWin, "1.00", ""))
		mustExecute(t, h.engine, c.wager(w[0], w[1], wager.KindLoss, "0.00", ""))
		refund := c.reversal(wager.KindRefund, bet)
		mustExecute(t, h.engine, refund)
		pending := mustExecute(t, h.engine, c.reversal(wager.KindRollback, c.bet(w[0], w[1], "3.00")))
		mustExecute(t, h.engine, engine.RetryReference{TransactionID: pending.TransactionID, Attempts: 1})
	}

	scansAfter, _, _ := tableStats(t, h.pool, "transactions")
	if scansAfter != scansBefore {
		t.Errorf("transactions was scanned sequentially %d times", scansAfter-scansBefore)
	}
	if keyScans := indexScans(t, h.pool, "idx_transactions_idempotency_unique") - keyScansBefore; keyScans < 60 {
		t.Errorf("the idempotency index was used %d times for 60 submissions", keyScans)
	}
}

// With room left on every page, a balance update writes no index entry.
func TestBalanceUpdatesAreHOT(t *testing.T) {
	h := newHarness(t, pgtest.FreshURL(t), postgres.WithMaxConns(1), postgres.WithMinConns(1))
	c := newCommands(t)
	wallets := seedWallets(t, h.pool, 2000)

	_, updatesBefore, hotBefore := tableStats(t, h.pool, "wallets")
	for i := range 100 {
		w := wallets[(i*37)%len(wallets)]
		mustExecute(t, h.engine, c.bet(w[0], w[1], "1.00"))
	}
	_, updatesAfter, hotAfter := tableStats(t, h.pool, "wallets")

	updates, hot := updatesAfter-updatesBefore, hotAfter-hotBefore
	if updates != 100 || hot != updates {
		t.Errorf("%d of %d wallet updates were HOT, want all 100", hot, updates)
	}
}

func TestEngineFailsWithoutEffectWhenTheVersionWouldOverflow(t *testing.T) {
	h := newHarness(t, pgtest.URL(t))
	c := newCommands(t)
	walletID, playerID := openWallet(t, h.engine, "100.00")
	if _, err := h.pool.Exec(testContext(t),
		`UPDATE wallets SET version = 9223372036854775807 WHERE id = $1`, walletID); err != nil {
		t.Fatal(err)
	}

	bet := c.bet(walletID, playerID, "10.00")
	if _, err := h.engine.Execute(testContext(t), bet); err == nil {
		t.Fatal("a bet past the largest version succeeded")
	}
	var recorded int
	if err := h.pool.QueryRow(testContext(t),
		`SELECT count(*) FROM transactions WHERE provider_id = $1`, c.provider).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if recorded != 0 {
		t.Errorf("%d transactions recorded by a failed bet", recorded)
	}
	requireBalance(t, h, walletID, "100.00")
}

// The key and external-ID indexes both used to start with provider_id, so
// with statistics that did not know a provider's rows yet (a new provider,
// rows that arrived after the last ANALYZE) the planner could look a key up
// through the external-ID index and read every transaction of the
// provider. Each lookup must reach its own index whatever the statistics.
func TestLookupsUseTheirOwnIndexEvenWithStaleStatistics(t *testing.T) {
	h := newHarness(t, pgtest.FreshURL(t), postgres.WithMaxConns(1))
	wallets := seedWallets(t, h.pool, 2000)
	if _, err := h.pool.Exec(testContext(t), `ANALYZE`); err != nil {
		t.Fatal(err)
	}
	c := newCommands(t)
	for i := range 2000 {
		w := wallets[i%len(wallets)]
		mustExecute(t, h.engine, c.bet(w[0], w[1], "1.00"))
	}

	for name, query := range map[string]string{
		"by key and external ID": `SELECT * FROM wager_find($1, 'no-such-key', 'no-such-external-id')`,
		"by external ID":         `SELECT * FROM transactions WHERE provider_id = $1 AND external_id = 'no-such-external-id' AND kind <> 'OPENING'`,
	} {
		var plan []map[string]any
		if err := h.pool.QueryRow(testContext(t), `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) `+query, c.provider).Scan(&plan); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		root := plan[0]["Plan"].(map[string]any)
		if hits := root["Shared Hit Blocks"].(float64) + root["Shared Read Blocks"].(float64); hits > 20 {
			t.Errorf("%s read %v blocks for a lookup that should touch a handful", name, hits)
		}
	}
}

// Helper functions the engine calls on every bet must be inlined into the
// statements that use them: before PostgreSQL 18, a SQL function that is
// not inlined is planned again on every call. One may still run while a
// statement is planned, folding a constant, but never once per bet.
func TestEngineHelpersAreInlinedOrCached(t *testing.T) {
	h := newHarness(t, pgtest.FreshURL(t)+"&track_functions=all", postgres.WithMaxConns(1), postgres.WithMinConns(1))
	c := newCommands(t)
	walletID, playerID := openWallet(t, h.engine, "100.00")
	const bets = 100
	for range bets {
		mustExecute(t, h.engine, c.bet(walletID, playerID, "0.01"))
	}
	if _, err := h.pool.Exec(testContext(t), `SELECT pg_stat_force_next_flush()`); err != nil {
		t.Fatal(err)
	}
	rows, err := h.pool.Query(testContext(t), `
		SELECT p.proname, f.calls
		FROM pg_stat_user_functions f JOIN pg_proc p ON p.oid = f.funcid
		WHERE p.prolang = (SELECT oid FROM pg_language WHERE lanname = 'sql')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var calls int64
		if err := rows.Scan(&name, &calls); err != nil {
			t.Fatal(err)
		}
		if calls >= bets {
			t.Errorf("SQL function %s ran %d times for %d bets instead of being inlined", name, calls, bets)
		}
	}
}
