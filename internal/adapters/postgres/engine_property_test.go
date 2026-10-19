package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/engine"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/engine/model"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/money"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/wager"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/testsupport/pgtest"
)

// sentEngine is one engine under comparison, with what it assigned.
type sentEngine struct {
	engine  engine.Engine
	wallets []string          // logical wallet index -> this engine's wallet ID
	ids     map[string]string // external ID -> this engine's transaction ID
}

// logical is a submission described independently of either engine.
type logical struct {
	wallet        int
	player        string
	kind          wager.Kind
	amount        money.Money
	external, key string
	reference     string
}

func (l logical) command(e *sentEngine, provider string) engine.SubmitWager {
	cmd := engine.SubmitWager{
		ProviderID: provider, ExternalID: l.external, IdempotencyKey: l.key,
		WalletID: e.wallets[l.wallet], PlayerID: l.player, RoundID: "round-1", GameID: "game-1",
		Kind: l.kind, Amount: l.amount, ReferenceExternalID: l.reference,
	}
	cmd.PayloadHash = fmt.Sprintf("%d|%s|%s|%s|%s|%s", l.wallet, l.player, l.kind, l.amount, l.external, l.reference)
	return cmd
}

// errorKind names an error by the sentinel it wraps, so the two engines'
// errors compare by meaning, not by text.
func errorKind(err error) string {
	for name, sentinel := range map[string]error{
		"wallet not found":   engine.ErrWalletNotFound,
		"conflict":           engine.ErrIdempotencyConflict,
		"key mismatch":       engine.ErrIdempotencyKeyMismatch,
		"transaction absent": engine.ErrTransactionNotFound,
		"invalid amount":     wager.ErrInvalidAmount,
		"invalid":            wager.ErrInvalidTransaction,
	} {
		if errors.Is(err, sentinel) {
			return name
		}
	}
	if err != nil {
		return "other: " + err.Error()
	}
	return ""
}

func describe(out engine.Outcome, err error) string {
	balance := "-"
	if out.Balance.Currency() != "" {
		balance = out.Balance.String()
	}
	return fmt.Sprintf("%s|%s|%s|replay=%v|%s", errorKind(err), out.Status, out.FailureCode, out.Replay, balance)
}

// TestEngineMatchesTheModel runs random sequences of commands, replays,
// conflicts and retries against the PostgreSQL engine and the reference
// model side by side: every answer and the final balances and ledgers must
// agree.
func TestEngineMatchesTheModel(t *testing.T) {
	h := newHarness(t, pgtest.FreshURL(t))
	ctx := context.Background()

	rapid.Check(t, func(rt *rapid.T) {
		reference := model.New()
		engines := []*sentEngine{
			{engine: reference, ids: map[string]string{}},
			{engine: h.engine, ids: map[string]string{}},
		}
		provider := "prop-" + uuid.NewString()[:8]
		players := []string{uuid.NewString(), uuid.NewString()}

		for w, player := range players {
			cents := rapid.Int64Range(0, 10_000).Draw(rt, fmt.Sprintf("opening%d", w))
			balance := brlCents(rt, cents)
			for _, e := range engines {
				out, err := e.engine.Execute(ctx, engine.OpenWallet{PlayerID: player, InitialBalance: balance})
				if err != nil {
					rt.Fatalf("open wallet: %v", err)
				}
				e.wallets = append(e.wallets, out.Wallet.ID())
			}
		}

		var sent []logical
		var pending []string // external IDs of reversals both engines left pending
		steps := rapid.IntRange(1, 40).Draw(rt, "steps")
		for step := range steps {
			var l logical
			action := rapid.SampledFrom([]string{
				"bet", "bet", "win", "loss", "refund", "rollback", "replay", "conflict", "mismatch",
				"currency", "foreign", "retry",
			}).Draw(rt, fmt.Sprintf("action%d", step))

			if action == "retry" {
				if len(pending) == 0 {
					continue
				}
				external := rapid.SampledFrom(pending).Draw(rt, fmt.Sprintf("retried%d", step))
				attempts := rapid.IntRange(1, wager.MaxReferenceAttempts+1).Draw(rt, fmt.Sprintf("attempts%d", step))
				var answers []string
				for _, e := range engines {
					out, err := e.engine.Execute(ctx, engine.RetryReference{TransactionID: e.ids[external], Attempts: attempts})
					answers = append(answers, describe(out, err))
				}
				if answers[0] != answers[1] {
					rt.Fatalf("step %d retry of %s: model %s, postgres %s", step, external, answers[0], answers[1])
				}
				continue
			}

			switch action {
			case "replay", "conflict", "mismatch":
				if len(sent) == 0 {
					continue
				}
				l = rapid.SampledFrom(sent).Draw(rt, fmt.Sprintf("previous%d", step))
				switch action {
				case "conflict":
					l.amount = brlCents(rt, l.amount.AmountInMinorUnits()+1)
				case "mismatch":
					l.key = "key-" + uuid.NewString()
				}
			default:
				l = logical{
					wallet:   rapid.IntRange(0, 1).Draw(rt, fmt.Sprintf("wallet%d", step)),
					external: "ext-" + uuid.NewString(),
				}
				l.key, l.player = "key-"+l.external, players[l.wallet]
				cents := rapid.Int64Range(1, 5_000).Draw(rt, fmt.Sprintf("cents%d", step))
				switch action {
				case "bet", "currency", "foreign":
					l.kind, l.amount = wager.KindBet, brlCents(rt, cents)
				case "win":
					l.kind, l.amount = wager.KindWin, brlCents(rt, cents)
				case "loss":
					l.kind, l.amount = wager.KindLoss, brlCents(rt, 0)
				case "refund", "rollback":
					l.kind = wager.KindRefund
					if action == "rollback" {
						l.kind = wager.KindRollback
					}
					l.amount, l.reference = brlCents(rt, cents), "ext-unknown-"+uuid.NewString()
					if len(sent) > 0 && rapid.Bool().Draw(rt, fmt.Sprintf("known%d", step)) {
						original := rapid.SampledFrom(sent).Draw(rt, fmt.Sprintf("original%d", step))
						l.wallet, l.player, l.reference = original.wallet, original.player, original.external
						if rapid.IntRange(0, 4).Draw(rt, fmt.Sprintf("same%d", step)) > 0 {
							l.amount = original.amount
						}
					}
				}
				if action == "currency" {
					usd, err := money.New(l.amount.DecimalString(), "USD")
					if err != nil {
						rt.Fatal(err)
					}
					l.amount = usd
				}
				if action == "foreign" {
					l.player = uuid.NewString()
				}
			}

			var answers []string
			var transactionIDs []string
			for _, e := range engines {
				out, err := e.engine.Execute(ctx, l.command(e, provider))
				answers = append(answers, describe(out, err))
				transactionIDs = append(transactionIDs, out.TransactionID)
			}
			if answers[0] != answers[1] {
				rt.Fatalf("step %d %s %s: model %s, postgres %s", step, action, l.kind, answers[0], answers[1])
			}
			if transactionIDs[0] == "" {
				continue
			}
			if _, known := engines[0].ids[l.external]; !known && action != "mismatch" {
				for i, e := range engines {
					e.ids[l.external] = transactionIDs[i]
				}
				sent = append(sent, l)
				status, _ := reference.Transaction(transactionIDs[0])
				if status.Status() == wager.StatusPendingReference {
					pending = append(pending, l.external)
				}
			}
		}

		for w := range players {
			modelBalance, modelVersion, _ := reference.WalletBalance(engines[0].wallets[w])
			pgBalance, pgVersion := h.Wallet(t, engines[1].wallets[w])
			if modelBalance.String() != pgBalance.String() || modelVersion != pgVersion {
				rt.Fatalf("wallet %d: model %s v%d, postgres %s v%d", w, modelBalance, modelVersion, pgBalance, pgVersion)
			}
			if m, p := reference.LedgerSum(engines[0].wallets[w]), h.LedgerSum(t, engines[1].wallets[w]); m != p {
				rt.Fatalf("wallet %d ledger: model %d, postgres %d", w, m, p)
			}
		}
	})
}

func brlCents(t interface{ Fatalf(string, ...any) }, cents int64) money.Money {
	m, err := money.New(fmt.Sprintf("%d.%02d", cents/100, cents%100), "BRL")
	if err != nil {
		t.Fatalf("money from %d cents: %v", cents, err)
	}
	return m
}
