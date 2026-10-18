package httpapi_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/adapters/httpapi"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/adapters/postgres"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/testsupport/pgtest"
)

// A request that cannot finish within its budget answers 503 and leaves
// nothing behind: its transaction rolls back and the next request works.
func TestSubmitTimesOutWith503WhenTheWalletIsLocked(t *testing.T) {
	h := newAPIHarness(t, func(d *httpapi.Dependencies) { d.RequestTimeout = 300 * time.Millisecond })
	provider := h.issuer.ProviderToken(t, "provider-a")
	internal := h.issuer.InternalToken(t)
	walletID, playerID := h.openWallet(t, "100.00")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	locker, err := postgres.Open(ctx, pgtest.URL(t))
	if err != nil {
		t.Fatalf("open PostgreSQL: %v", err)
	}
	defer locker.Close()
	lock, err := locker.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := lock.Exec(ctx, `SELECT 1 FROM wallets WHERE id = $1 FOR UPDATE`, walletID); err != nil {
		t.Fatalf("lock wallet: %v", err)
	}
	// Without a budget the bet would wait for the lock forever; release it
	// eventually so a regression fails the assertions instead of hanging.
	release := time.AfterFunc(3*time.Second, func() { _ = lock.Rollback(context.Background()) })
	defer release.Stop()

	started := time.Now()
	resp := h.submit(t, provider, wagerBody(walletID, playerID, "BET", "10.00"))
	elapsed := time.Since(started)
	_ = lock.Rollback(context.Background())

	requireError(t, resp, http.StatusServiceUnavailable, "TIMEOUT")
	if elapsed > 2*time.Second {
		t.Fatalf("answered after %v, want within the 300ms budget", elapsed)
	}

	wallet := h.do(t, http.MethodGet, "/wallets/"+walletID, internal, nil, nil)
	requireStatus(t, wallet, http.StatusOK)
	if balance := wallet.body["balance"].(map[string]any)["amount"]; balance != "100.00" {
		t.Fatalf("balance = %v after the timed-out bet, want 100.00", balance)
	}
	requireStatus(t, h.submit(t, provider, wagerBody(walletID, playerID, "BET", "10.00")), http.StatusOK)
}
