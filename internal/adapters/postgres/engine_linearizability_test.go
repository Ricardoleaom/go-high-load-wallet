package postgres_test

import (
	"fmt"
	"maps"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/anishathalye/porcupine"
	"github.com/google/uuid"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/adapters/postgres"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/engine"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/wager"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/testsupport/pgtest"
)

// walletOp is one operation on the wallet under test: a bet or a win under
// a key, or a read of the balance.
type walletOp struct {
	kind  string // BET, WIN or READ
	key   string
	cents int64
}

type walletResult struct {
	status wager.Status
	cents  int64 // balance after a PROCESSED movement, or the balance read
	replay bool
}

// walletState is the sequential specification: one balance, and what each
// key was answered the first time.
type walletState struct {
	cents   int64
	applied map[string]walletResult
}

var walletModel = porcupine.Model{
	Init: func() any { return walletState{cents: 5000, applied: map[string]walletResult{}} },
	Step: func(state, input, output any) (bool, any) {
		st, op, got := state.(walletState), input.(walletOp), output.(walletResult)
		if op.kind == "READ" {
			return got.cents == st.cents, st
		}
		if first, seen := st.applied[op.key]; seen {
			return got.replay && got.status == first.status && got.cents == first.cents, st
		}
		want := walletResult{status: wager.StatusProcessed}
		switch {
		case op.kind == "WIN":
			want.cents = st.cents + op.cents
		case st.cents >= op.cents:
			want.cents = st.cents - op.cents
		default:
			want.status = wager.StatusRejected
		}
		if got.replay || got.status != want.status || got.cents != want.cents {
			return false, st
		}
		next := walletState{cents: st.cents, applied: maps.Clone(st.applied)}
		if want.status == wager.StatusProcessed {
			next.cents = want.cents
		}
		next.applied[op.key] = want
		return true, next
	},
	Equal: func(a, b any) bool {
		x, y := a.(walletState), b.(walletState)
		return x.cents == y.cents && maps.Equal(x.applied, y.applied)
	},
	DescribeOperation: func(input, output any) string {
		op, got := input.(walletOp), output.(walletResult)
		if op.kind == "READ" {
			return fmt.Sprintf("read -> %d", got.cents)
		}
		return fmt.Sprintf("%s %d [%s] -> %s %d replay=%v", op.kind, op.cents, op.key[len(op.key)-4:], got.status, got.cents, got.replay)
	},
	DescribeState: func(state any) string { return fmt.Sprintf("%d", state.(walletState).cents) },
}

// TestEngineIsLinearizable has eight clients bet, win, resend each other's
// bets and read one low-balance wallet at once, against the real database,
// and checks the history against a single-threaded wallet: every answer
// must be explained by some order of the operations that respects real
// time.
func TestEngineIsLinearizable(t *testing.T) {
	h := newHarness(t, pgtest.URL(t))
	reader := postgres.NewReader(h.pool)
	c := newCommands(t)
	walletID, playerID := openWallet(t, h.engine, "50.00")

	const clients, perClient = 8, 40
	// Clients 2k and 2k+1 share a set of keys, so each shared bet is sent
	// twice at about the same time.
	shared := make([][]walletOp, clients/2)
	for pair := range shared {
		for i := range perClient / 4 {
			shared[pair] = append(shared[pair], walletOp{kind: "BET", key: fmt.Sprintf("shared-%d-%d-%s", pair, i, uuid.NewString()), cents: int64(100 + 37*i%400)})
		}
	}

	start := time.Now()
	var mu sync.Mutex
	var history []porcupine.Operation
	var wg sync.WaitGroup
	for client := range clients {
		wg.Go(func() {
			for i := range perClient {
				var op walletOp
				switch i % 4 {
				case 0:
					op = shared[client/2][i/4]
				case 1:
					op = walletOp{kind: "BET", key: uuid.NewString(), cents: int64(50 + (client*131+i*17)%600)}
				case 2:
					op = walletOp{kind: "WIN", key: uuid.NewString(), cents: int64(20 + (client*53+i*29)%300)}
				default:
					op = walletOp{kind: "READ"}
				}

				call := time.Since(start).Nanoseconds()
				result := runOp(t, h, reader, c, walletID, playerID, op)
				ret := time.Since(start).Nanoseconds()

				mu.Lock()
				history = append(history, porcupine.Operation{ClientId: client, Input: op, Call: call, Output: result, Return: ret})
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	result, info := porcupine.CheckOperationsVerbose(walletModel, history, 60*time.Second)
	if result != porcupine.Ok {
		path := filepath.Join(t.TempDir(), "history.html")
		if err := porcupine.VisualizePath(walletModel, info, path); err != nil {
			t.Logf("visualize: %v", err)
		}
		t.Fatalf("history of %d operations is %v; see %s", len(history), result, path)
	}
}

func runOp(t *testing.T, h harness, reader *postgres.Reader, c commands, walletID, playerID string, op walletOp) walletResult {
	t.Helper()
	if op.kind == "READ" {
		w, err := reader.Wallet(testContext(t), walletID)
		if err != nil {
			t.Errorf("read: %v", err)
		}
		return walletResult{cents: w.Balance().AmountInMinorUnits()}
	}

	cmd := c.wager(walletID, playerID, wager.Kind(op.kind), fmt.Sprintf("%d.%02d", op.cents/100, op.cents%100), "")
	cmd.IdempotencyKey, cmd.ExternalID, cmd.PayloadHash = op.key, "ext-"+op.key, "hash-"+op.key
	out, err := h.engine.Execute(testContext(t), engine.Command(cmd))
	if err != nil {
		t.Errorf("%s %s: %v", op.kind, op.key, err)
		return walletResult{}
	}
	result := walletResult{status: out.Status, replay: out.Replay}
	if out.Status == wager.StatusProcessed {
		result.cents = out.Balance.AmountInMinorUnits()
	}
	return result
}
