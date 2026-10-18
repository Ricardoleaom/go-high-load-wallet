package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/adapters/postgres"
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
