package postgres_test

import (
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/adapters/postgres"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/engine"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/wagering"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/wager"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/testsupport/pgtest"
)

func TestInboxRecordsOnceAndDetectsPayloadMismatch(t *testing.T) {
	inbox := postgres.NewInbox(openPool(t, pgtest.URL(t)))
	ctx := testContext(t)
	messageID := "message-" + uuid.NewString()

	completed, err := inbox.Record(ctx, "consumer", messageID, "hash-1")
	if err != nil || completed {
		t.Fatalf("first delivery: completed=%v err=%v", completed, err)
	}
	// Redelivered before completion: process it again.
	if completed, err = inbox.Record(ctx, "consumer", messageID, "hash-1"); err != nil || completed {
		t.Fatalf("redelivery before completion: completed=%v err=%v", completed, err)
	}
	if err := inbox.Complete(ctx, "consumer", messageID); err != nil {
		t.Fatal(err)
	}
	if completed, err = inbox.Record(ctx, "consumer", messageID, "hash-1"); err != nil || !completed {
		t.Fatalf("redelivery after completion: completed=%v err=%v", completed, err)
	}
	if _, err := inbox.Record(ctx, "consumer", messageID, "hash-2"); !errors.Is(err, postgres.ErrInboxPayloadMismatch) {
		t.Fatalf("same message, other payload: err = %v", err)
	}
	// Another consumer keeps its own record of the same message.
	if completed, err = inbox.Record(ctx, "other-consumer", messageID, "hash-2"); err != nil || completed {
		t.Fatalf("another consumer: completed=%v err=%v", completed, err)
	}
}

// pendingRefunds submits n refunds of bets that never arrive.
func pendingRefunds(t *testing.T, h harness, n int) []string {
	t.Helper()
	c := newCommands(t)
	walletID, playerID := openWallet(t, h.engine, "100.00")
	ids := make([]string, n)
	for i := range ids {
		refund := c.wager(walletID, playerID, wager.KindRefund, "1.00", "ext-never-"+uuid.NewString())
		ids[i] = mustExecute(t, h.engine, refund).TransactionID
	}
	return ids
}

func makeDue(t *testing.T, h harness, ids []string) {
	t.Helper()
	if _, err := h.pool.Exec(testContext(t),
		`UPDATE transactions SET reference_next_attempt_at = now() - interval '1 second' WHERE id = ANY($1)`, ids); err != nil {
		t.Fatal(err)
	}
}

// Two workers claiming together split the due reversals; a claim is a
// lease, so a claimed reversal is not handed out again until it expires.
func TestClaimDueClaimsEachRowOnce(t *testing.T) {
	url := pgtest.FreshURL(t)
	h := newHarness(t, url)
	ids := pendingRefunds(t, h, 30)

	queues := []*postgres.ReferenceQueue{postgres.NewReferenceQueue(h.pool), postgres.NewReferenceQueue(openPool(t, url))}
	if claims, err := queues[0].ClaimDue(testContext(t), 50); err != nil || len(claims) != 0 {
		t.Fatalf("before they are due: %d claims, %v", len(claims), err)
	}
	makeDue(t, h, ids)

	var mu sync.Mutex
	counts := map[string]int{}
	var wg sync.WaitGroup
	for _, q := range queues {
		wg.Go(func() {
			for range 3 {
				claims, err := q.ClaimDue(testContext(t), 7)
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				for _, c := range claims {
					counts[c.TransactionID]++
					if c.Attempts != 1 {
						t.Errorf("claim %s has %d attempts, want 1", c.TransactionID, c.Attempts)
					}
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	if len(counts) != 30 {
		t.Errorf("%d reversals claimed, want 30", len(counts))
	}
	for id, n := range counts {
		if n != 1 {
			t.Errorf("reversal %s claimed %d times", id, n)
		}
	}
	if claims, _ := queues[0].ClaimDue(testContext(t), 50); len(claims) != 0 {
		t.Errorf("%d reversals claimed again within the lease", len(claims))
	}
}

// Claims feed RetryReference: what a claim hands out, the engine settles.
func TestClaimedReversalsAreSettledByTheEngine(t *testing.T) {
	h := newHarness(t, pgtest.FreshURL(t))
	ids := pendingRefunds(t, h, 3)
	makeDue(t, h, ids)

	claims, err := postgres.NewReferenceQueue(h.pool).ClaimDue(testContext(t), 10)
	if err != nil || len(claims) != 3 {
		t.Fatalf("ClaimDue = %d claims, %v", len(claims), err)
	}
	for _, claim := range claims {
		out := mustExecute(t, h.engine, engine.RetryReference{TransactionID: claim.TransactionID, Attempts: wager.MaxReferenceAttempts})
		if out.Status != wager.StatusRejected || out.FailureCode != wager.FailureReferenceNotFoundTimeout {
			t.Errorf("claim %s settled as %s %s", claim.TransactionID, out.Status, out.FailureCode)
		}
	}
	var _ wagering.ReferenceQueue = postgres.NewReferenceQueue(h.pool)
}
