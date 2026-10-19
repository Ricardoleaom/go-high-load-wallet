package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/adapters/postgres"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/wager"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/testsupport/pgtest"
)

// isTimeout is how callers outside the adapter recognize a timeout, the
// same convention net.Error follows.
func isTimeout(err error) bool {
	var timeout interface{ Timeout() bool }
	return errors.As(err, &timeout) && timeout.Timeout()
}

func TestTimeoutsSayTheyAreTimeouts(t *testing.T) {
	h := newHarness(t, pgtest.URL(t), postgres.WithSessionTimeouts(0, 0, 100*time.Millisecond))
	c := newCommands(t)
	walletID, playerID := openWallet(t, h.engine, "100.00")

	lock, err := h.pool.Begin(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Rollback(context.Background()) }()
	if _, err := lock.Exec(testContext(t), `SELECT 1 FROM wallets WHERE id = $1 FOR UPDATE`, walletID); err != nil {
		t.Fatal(err)
	}

	_, err = h.engine.Execute(testContext(t), c.bet(walletID, playerID, "10.00"))
	if !isTimeout(err) {
		t.Errorf("a bet that hit lock_timeout: err = %v, not a timeout", err)
	}

	expired, cancel := context.WithTimeout(testContext(t), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)
	if _, err := postgres.NewReader(h.pool).Wallet(expired, walletID); !isTimeout(err) {
		t.Errorf("a read past its deadline: err = %v, not a timeout", err)
	}

	if _, err := h.engine.Execute(testContext(t), c.bet(walletID, "not-a-uuid", "10.00")); err == nil || isTimeout(err) {
		t.Errorf("a malformed command: err = %v, want an error that is not a timeout", err)
	}
}

// A bet that runs out of time while another transaction holds its wallet
// leaves nothing behind: the statement is cancelled on the server, the
// connection stays in the pool, and resending the same command once the
// wallet is free applies it exactly once.
func TestTimedOutBetUnderTheWalletLockAppliesOnceWhenResent(t *testing.T) {
	h := newHarness(t, pgtest.URL(t), postgres.WithMaxConns(2), postgres.WithMinConns(2))
	c := newCommands(t)
	walletID, playerID := openWallet(t, h.engine, "100.00")
	opened := h.pool.Stat().NewConnsCount()

	blocker, err := postgres.Open(testContext(t), pgtest.URL(t), postgres.WithMaxConns(1), postgres.WithMinConns(1))
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	lock, err := blocker.Begin(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Rollback(context.Background()) }()
	if _, err := lock.Exec(testContext(t), `SELECT 1 FROM wallets WHERE id = $1 FOR UPDATE`, walletID); err != nil {
		t.Fatal(err)
	}

	bet := c.bet(walletID, playerID, "30.00")
	ctx, cancel := context.WithTimeout(testContext(t), 300*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := h.engine.Execute(ctx, bet); !isTimeout(err) {
		t.Fatalf("a bet waiting for a locked wallet past its deadline: err = %v, want a timeout", err)
	}
	if took := time.Since(started); took > 2*time.Second {
		t.Fatalf("the bet answered after %s", took)
	}

	// While the wallet is still locked, the server has dropped the statement
	// instead of leaving it waiting for the lock.
	deadline := time.Now().Add(2 * time.Second)
	for {
		var waiting int
		if err := h.pool.QueryRow(testContext(t), `
			SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND state = 'active' AND query LIKE '%wager_submit%'
			  AND pid <> pg_backend_pid()`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d timed-out submissions still waiting for the wallet on the server", waiting)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := lock.Rollback(testContext(t)); err != nil {
		t.Fatal(err)
	}
	requireBalance(t, h, walletID, "100.00")

	first := mustExecute(t, h.engine, bet)
	again := mustExecute(t, h.engine, bet)
	if first.Status != wager.StatusProcessed || first.Replay || !again.Replay || again.TransactionID != first.TransactionID {
		t.Fatalf("resend after the timeout = %+v, then %+v; want applied once, then a replay", first, again)
	}
	requireBalance(t, h, walletID, "70.00")
	if n := h.LedgerCount(t, first.TransactionID); n != 1 {
		t.Errorf("the bet has %d ledger entries, want 1", n)
	}
	// The connection the bet timed out on was kept: the pool neither lost
	// one nor opened a replacement.
	if total, reopened := h.pool.Stat().TotalConns(), h.pool.Stat().NewConnsCount()-opened; total != 2 || reopened != 0 {
		t.Errorf("after the timeout the pool holds %d connections and opened %d, want 2 and 0", total, reopened)
	}
}
