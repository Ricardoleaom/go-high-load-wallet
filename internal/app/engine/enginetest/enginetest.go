// Package enginetest is the contract every engine.Engine honours. Each
// implementation runs Run against a Harness that can read back what the
// engine wrote; the in-memory model and the PostgreSQL engine both pass the
// same suite, which is what lets the model stand in for the real thing in
// property tests.
package enginetest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/engine"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/events"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/money"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/wager"
)

// Harness gives the suite an engine and a read-only view of what it wrote.
type Harness interface {
	Engine() engine.Engine
	// Events returns the events about a transaction (those whose data names
	// it as transactionId), oldest first; an event caused by another comes
	// after it.
	Events(t *testing.T, transactionID string) []events.Envelope
	Transaction(t *testing.T, id string) wager.Transaction
	Wallet(t *testing.T, id string) (balance money.Money, version int64)
	LedgerSum(t *testing.T, walletID string) int64
	LedgerCount(t *testing.T, transactionID string) int
}

// Run runs every contract case against h. Cases use their own wallets and
// identifiers, so they share one harness and run in parallel.
func Run(t *testing.T, h Harness) {
	cases := []struct {
		name string
		run  func(*testing.T, *suite)
	}{
		{"OpenWalletWithBalanceRecordsOpening", openWalletWithBalanceRecordsOpening},
		{"OpenWalletWithZeroBalanceRecordsOnlyTheWallet", openWalletWithZeroBalanceRecordsOnlyTheWallet},
		{"OpenWalletRejectsDuplicatePlayerCurrency", openWalletRejectsDuplicatePlayerCurrency},
		{"OpenWalletAllowsAnotherCurrency", openWalletAllowsAnotherCurrency},
		{"BetDebits", betDebits},
		{"BetBeyondBalanceIsRejected", betBeyondBalanceIsRejected},
		{"BetInAnotherCurrencyIsRejectedAndKeepsItsCurrency", betInAnotherCurrencyIsRejectedAndKeepsItsCurrency},
		{"UnknownWalletOrForeignPlayerIsNotFound", unknownWalletOrForeignPlayerIsNotFound},
		{"ReplayReturnsTheFirstResult", replayReturnsTheFirstResult},
		{"SameKeyDifferentPayloadConflicts", sameKeyDifferentPayloadConflicts},
		{"SameExternalIDDifferentKeyMismatches", sameExternalIDDifferentKeyMismatches},
		{"KeyTakesPrecedenceOverExternalID", keyTakesPrecedenceOverExternalID},
		{"WinCredits", winCredits},
		{"WinThatWouldOverflowFailsWithoutEffect", winThatWouldOverflowFailsWithoutEffect},
		{"LossDoesNotMoveMoney", lossDoesNotMoveMoney},
		{"LossInAnotherCurrencyIsRejected", lossInAnotherCurrencyIsRejected},
		{"RefundReturnsTheBet", refundReturnsTheBet},
		{"SecondReversalIsRejected", secondReversalIsRejected},
		{"RollbackUndoesAWin", rollbackUndoesAWin},
		{"RollbackOfAWinWithoutBalanceIsRejected", rollbackOfAWinWithoutBalanceIsRejected},
		{"RollbackUndoesARefund", rollbackUndoesARefund},
		{"RefundOfAWinIsNotReversible", refundOfAWinIsNotReversible},
		{"ReversalOfARejectedBetIsNotProcessed", reversalOfARejectedBetIsNotProcessed},
		{"ReversalWithADifferentAmountMismatches", reversalWithADifferentAmountMismatches},
		{"ReversalBeforeItsReferenceIsPending", reversalBeforeItsReferenceIsPending},
		{"RetryResolvesAPendingReversal", retryResolvesAPendingReversal},
		{"RetryKeepsWaitingBelowTheLimit", retryKeepsWaitingBelowTheLimit},
		{"RetryGivesUpAtTheLimit", retryGivesUpAtTheLimit},
		{"RetryOfASettledTransactionChangesNothing", retryOfASettledTransactionChangesNothing},
		{"RetryOfAnUnknownTransactionIsNotFound", retryOfAnUnknownTransactionIsNotFound},
		{"ConcurrentBetsNeverOverdraw", concurrentBetsNeverOverdraw},
		{"ConcurrentDuplicatesMoveMoneyOnce", concurrentDuplicatesMoveMoneyOnce},
		{"ConcurrentKeysForOneExternalIDApplyOnce", concurrentKeysForOneExternalIDApplyOnce},
		{"ConcurrentReversalsOfOneBetApplyOnce", concurrentReversalsOfOneBetApplyOnce},
		{"EventsDescribeEveryOutcome", eventsDescribeEveryOutcome},
		{"IDsAreVersion7", idsAreVersion7},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			c.run(t, &suite{h: h, provider: "provider-" + newID()[:8]})
		})
	}
}

// suite carries the harness and a provider of its own, so no two cases
// can collide on an external ID.
type suite struct {
	h        Harness
	provider string
}

func (s *suite) execute(t *testing.T, cmd engine.Command) (engine.Outcome, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return s.h.Engine().Execute(ctx, cmd)
}

func (s *suite) mustExecute(t *testing.T, cmd engine.Command) engine.Outcome {
	t.Helper()
	out, err := s.execute(t, cmd)
	if err != nil {
		t.Fatalf("Execute(%T): %v", cmd, err)
	}
	return out
}

// openWallet opens a wallet for a fresh player and returns its ID and the
// player's.
func (s *suite) openWallet(t *testing.T, balance string) (walletID, playerID string) {
	t.Helper()
	playerID = newID()
	out := s.mustExecute(t, engine.OpenWallet{PlayerID: playerID, InitialBalance: brl(t, balance)})
	return out.Wallet.ID(), playerID
}

// wager builds a submission with fresh external ID and key, hashed over
// its business fields.
func (s *suite) wager(t *testing.T, walletID, playerID string, kind wager.Kind, amount money.Money, reference string) engine.SubmitWager {
	t.Helper()
	cmd := engine.SubmitWager{
		ProviderID:          s.provider,
		ExternalID:          "ext-" + newID(),
		IdempotencyKey:      "key-" + newID(),
		WalletID:            walletID,
		PlayerID:            playerID,
		RoundID:             "round-1",
		GameID:              "game-1",
		Kind:                kind,
		Amount:              amount,
		ReferenceExternalID: reference,
	}
	return rehash(cmd)
}

// rehash recomputes the payload hash after a test changed a field.
func rehash(cmd engine.SubmitWager) engine.SubmitWager {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		cmd.ProviderID, cmd.ExternalID, cmd.WalletID, cmd.PlayerID, cmd.RoundID, cmd.GameID,
		string(cmd.Kind), cmd.Amount.DecimalString(), cmd.Amount.Currency(), cmd.ReferenceExternalID,
	}, "|")))
	cmd.PayloadHash = hex.EncodeToString(sum[:])
	return cmd
}

func (s *suite) bet(t *testing.T, walletID, playerID, amount string) engine.SubmitWager {
	t.Helper()
	return s.wager(t, walletID, playerID, wager.KindBet, brl(t, amount), "")
}

func (s *suite) win(t *testing.T, walletID, playerID, amount string) engine.SubmitWager {
	t.Helper()
	return s.wager(t, walletID, playerID, wager.KindWin, brl(t, amount), "")
}

func (s *suite) reversal(t *testing.T, kind wager.Kind, of engine.SubmitWager) engine.SubmitWager {
	t.Helper()
	return s.wager(t, of.WalletID, of.PlayerID, kind, of.Amount, of.ExternalID)
}

// requireWallet checks the stored balance and that the ledger adds up to it
// (invariant I2).
func (s *suite) requireWallet(t *testing.T, walletID, balance string, version int64) {
	t.Helper()
	gotBalance, gotVersion := s.h.Wallet(t, walletID)
	if gotBalance.DecimalString() != balance || gotVersion != version {
		t.Errorf("wallet = %s v%d, want %s v%d", gotBalance.DecimalString(), gotVersion, balance, version)
	}
	if sum := s.h.LedgerSum(t, walletID); sum != gotBalance.AmountInMinorUnits() {
		t.Errorf("ledger sums to %d, balance is %d", sum, gotBalance.AmountInMinorUnits())
	}
}

func requireOutcome(t *testing.T, out engine.Outcome, status wager.Status, failureCode, balance string) {
	t.Helper()
	if out.Status != status || out.FailureCode != failureCode {
		t.Errorf("outcome = %s %q, want %s %q", out.Status, out.FailureCode, status, failureCode)
	}
	gotBalance := ""
	if out.Balance.Currency() != "" {
		gotBalance = out.Balance.DecimalString()
	}
	if gotBalance != balance {
		t.Errorf("outcome balance = %q, want %q", gotBalance, balance)
	}
}

// eventTypes lists the types of the events about a transaction.
func (s *suite) eventTypes(t *testing.T, transactionID string) []string {
	t.Helper()
	var types []string
	for _, e := range s.h.Events(t, transactionID) {
		types = append(types, e.EventType)
	}
	return types
}

func (s *suite) requireEventTypes(t *testing.T, transactionID string, want ...string) {
	t.Helper()
	got := s.eventTypes(t, transactionID)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("events of %s = %v, want %v", transactionID, got, want)
	}
}

func openWalletWithBalanceRecordsOpening(t *testing.T, s *suite) {
	playerID := newID()
	out := s.mustExecute(t, engine.OpenWallet{PlayerID: playerID, InitialBalance: brl(t, "100.00")})

	w := out.Wallet
	if w.ID() == "" || w.PlayerID() != playerID || w.Balance().DecimalString() != "100.00" ||
		w.Balance().Currency() != "BRL" || w.Version() != 1 {
		t.Fatalf("opened wallet = %s %s %s v%d", w.ID(), w.PlayerID(), w.Balance(), w.Version())
	}
	requireOutcome(t, out, wager.StatusProcessed, "", "100.00")
	s.requireWallet(t, w.ID(), "100.00", 1)

	opening := s.h.Transaction(t, out.TransactionID)
	if opening.Kind() != wager.KindOpening || opening.Status() != wager.StatusProcessed ||
		opening.WalletID() != w.ID() || opening.Amount().DecimalString() != "100.00" {
		t.Errorf("opening = %s %s %s %s", opening.Kind(), opening.Status(), opening.WalletID(), opening.Amount())
	}
	if n := s.h.LedgerCount(t, out.TransactionID); n != 1 {
		t.Errorf("opening has %d ledger entries, want 1", n)
	}
	s.requireEventTypes(t, out.TransactionID, events.TypeWagerTransactionProcessed, events.TypeWalletBalanceChanged)
}

func openWalletWithZeroBalanceRecordsOnlyTheWallet(t *testing.T, s *suite) {
	out := s.mustExecute(t, engine.OpenWallet{PlayerID: newID(), InitialBalance: brl(t, "0.00")})
	if out.TransactionID != "" {
		t.Errorf("zero opening recorded transaction %s", out.TransactionID)
	}
	if out.Wallet.Balance().DecimalString() != "0.00" || out.Wallet.Version() != 1 {
		t.Errorf("opened wallet = %s v%d", out.Wallet.Balance(), out.Wallet.Version())
	}
	s.requireWallet(t, out.Wallet.ID(), "0.00", 1)
}

func openWalletRejectsDuplicatePlayerCurrency(t *testing.T, s *suite) {
	_, playerID := s.openWallet(t, "10.00")
	_, err := s.execute(t, engine.OpenWallet{PlayerID: playerID, InitialBalance: brl(t, "20.00")})
	if !errors.Is(err, engine.ErrWalletConflict) {
		t.Fatalf("second wallet in the same currency: err = %v, want ErrWalletConflict", err)
	}
}

func openWalletAllowsAnotherCurrency(t *testing.T, s *suite) {
	_, playerID := s.openWallet(t, "10.00")
	usd, err := money.New("5.00", "USD")
	if err != nil {
		t.Fatal(err)
	}
	out := s.mustExecute(t, engine.OpenWallet{PlayerID: playerID, InitialBalance: usd})
	if out.Wallet.Balance().Currency() != "USD" {
		t.Errorf("second wallet currency = %s, want USD", out.Wallet.Balance().Currency())
	}
}

func betDebits(t *testing.T, s *suite) {
	walletID, playerID := s.openWallet(t, "100.00")
	out := s.mustExecute(t, s.bet(t, walletID, playerID, "30.00"))

	requireOutcome(t, out, wager.StatusProcessed, "", "70.00")
	if out.Replay || out.Raced {
		t.Errorf("first application reported replay=%v raced=%v", out.Replay, out.Raced)
	}
	s.requireWallet(t, walletID, "70.00", 2)
	if n := s.h.LedgerCount(t, out.TransactionID); n != 1 {
		t.Errorf("bet has %d ledger entries, want 1", n)
	}
	tx := s.h.Transaction(t, out.TransactionID)
	if tx.Kind() != wager.KindBet || tx.Status() != wager.StatusProcessed {
		t.Errorf("stored bet = %s %s", tx.Kind(), tx.Status())
	}
	s.requireEventTypes(t, out.TransactionID, events.TypeWagerTransactionProcessed, events.TypeWalletBalanceChanged)
}

func betBeyondBalanceIsRejected(t *testing.T, s *suite) {
	walletID, playerID := s.openWallet(t, "10.00")
	out := s.mustExecute(t, s.bet(t, walletID, playerID, "30.00"))

	requireOutcome(t, out, wager.StatusRejected, wager.FailureInsufficientBalance, "")
	s.requireWallet(t, walletID, "10.00", 1)
	if n := s.h.LedgerCount(t, out.TransactionID); n != 0 {
		t.Errorf("rejected bet has %d ledger entries", n)
	}
	s.requireEventTypes(t, out.TransactionID, events.TypeWagerTransactionRejected)
}

func betInAnotherCurrencyIsRejectedAndKeepsItsCurrency(t *testing.T, s *suite) {
	walletID, playerID := s.openWallet(t, "100.00")
	usd, err := money.New("30.00", "USD")
	if err != nil {
		t.Fatal(err)
	}
	out := s.mustExecute(t, s.wager(t, walletID, playerID, wager.KindBet, usd, ""))

	requireOutcome(t, out, wager.StatusRejected, wager.FailureCurrencyMismatch, "")
	if got := s.h.Transaction(t, out.TransactionID).Amount(); got.Currency() != "USD" || got.DecimalString() != "30.00" {
		t.Errorf("stored amount = %s, want 30.00 USD", got)
	}
	s.requireWallet(t, walletID, "100.00", 1)
}

func unknownWalletOrForeignPlayerIsNotFound(t *testing.T, s *suite) {
	walletID, playerID := s.openWallet(t, "100.00")
	stranger := newID()

	for name, cmd := range map[string]engine.SubmitWager{
		"unknown wallet":           s.bet(t, newID(), playerID, "10.00"),
		"bet by another player":    s.bet(t, walletID, stranger, "10.00"),
		"loss by another player":   s.wager(t, walletID, stranger, wager.KindLoss, brl(t, "0.00"), ""),
		"win by another player":    s.win(t, walletID, stranger, "10.00"),
		"refund by another player": s.wager(t, walletID, stranger, wager.KindRefund, brl(t, "10.00"), "ext-unknown"),
	} {
		if _, err := s.execute(t, cmd); !errors.Is(err, engine.ErrWalletNotFound) {
			t.Errorf("%s: err = %v, want ErrWalletNotFound", name, err)
		}
	}
	s.requireWallet(t, walletID, "100.00", 1)

	// Nothing was recorded: the same key works once the request is right.
	retry := s.bet(t, walletID, stranger, "10.00")
	if _, err := s.execute(t, retry); !errors.Is(err, engine.ErrWalletNotFound) {
		t.Fatalf("foreign player: err = %v", err)
	}
	retry.PlayerID = playerID
	out := s.mustExecute(t, rehash(retry))
	requireOutcome(t, out, wager.StatusProcessed, "", "90.00")
}

func replayReturnsTheFirstResult(t *testing.T, s *suite) {
	walletID, playerID := s.openWallet(t, "100.00")
	first := s.bet(t, walletID, playerID, "30.00")
	applied := s.mustExecute(t, first)
	s.mustExecute(t, s.bet(t, walletID, playerID, "10.00"))

	replayed := s.mustExecute(t, first)
	if !replayed.Replay || replayed.TransactionID != applied.TransactionID {
		t.Fatalf("replay = %+v, want replay of %s", replayed, applied.TransactionID)
	}
	requireOutcome(t, replayed, wager.StatusProcessed, "", "70.00")
	s.requireWallet(t, walletID, "60.00", 3)
	if n := s.h.LedgerCount(t, applied.TransactionID); n != 1 {
		t.Errorf("replayed bet has %d ledger entries, want 1", n)
	}

	rejected := s.bet(t, walletID, playerID, "500.00")
	s.mustExecute(t, rejected)
	again := s.mustExecute(t, rejected)
	if !again.Replay {
		t.Error("resent rejection was not a replay")
	}
	requireOutcome(t, again, wager.StatusRejected, wager.FailureInsufficientBalance, "")

	loss := s.wager(t, walletID, playerID, wager.KindLoss, brl(t, "0.00"), "")
	s.mustExecute(t, loss)
	s.mustExecute(t, s.win(t, walletID, playerID, "5.00"))
	lossAgain := s.mustExecute(t, loss)
	requireOutcome(t, lossAgain, wager.StatusProcessed, "", "65.00")
}

func sameKeyDifferentPayloadConflicts(t *testing.T, s *suite) {
	walletID, playerID := s.openWallet(t, "100.00")
	first := s.bet(t, walletID, playerID, "30.00")
	s.mustExecute(t, first)

	changed := first
	changed.Amount = brl(t, "31.00")
	if _, err := s.execute(t, rehash(changed)); !errors.Is(err, engine.ErrIdempotencyConflict) {
		t.Fatalf("same key, other payload: err = %v, want ErrIdempotencyConflict", err)
	}
	s.requireWallet(t, walletID, "70.00", 2)
}

func sameExternalIDDifferentKeyMismatches(t *testing.T, s *suite) {
	walletID, playerID := s.openWallet(t, "100.00")
	first := s.bet(t, walletID, playerID, "30.00")
	s.mustExecute(t, first)

	other := first
	other.IdempotencyKey = "key-" + newID()
	if _, err := s.execute(t, other); !errors.Is(err, engine.ErrIdempotencyKeyMismatch) {
		t.Fatalf("same external ID, other key: err = %v, want ErrIdempotencyKeyMismatch", err)
	}
	s.requireWallet(t, walletID, "70.00", 2)
}

// A submission whose key names one recorded transaction and whose external
// ID names another is judged by its key: a conflict, not a key mismatch.
func keyTakesPrecedenceOverExternalID(t *testing.T, s *suite) {
	walletID, playerID := s.openWallet(t, "100.00")
	first := s.bet(t, walletID, playerID, "10.00")
	second := s.bet(t, walletID, playerID, "20.00")
	s.mustExecute(t, first)
	s.mustExecute(t, second)

	crossed := first
	crossed.ExternalID = second.ExternalID
	if _, err := s.execute(t, rehash(crossed)); !errors.Is(err, engine.ErrIdempotencyConflict) {
		t.Fatalf("key of one, external ID of another: err = %v, want ErrIdempotencyConflict", err)
	}
	s.requireWallet(t, walletID, "70.00", 3)
}

func winCredits(t *testing.T, s *suite) {
	walletID, playerID := s.openWallet(t, "100.00")
	out := s.mustExecute(t, s.win(t, walletID, playerID, "25.50"))
	requireOutcome(t, out, wager.StatusProcessed, "", "125.50")
	s.requireWallet(t, walletID, "125.50", 2)
	s.requireEventTypes(t, out.TransactionID, events.TypeWagerTransactionProcessed, events.TypeWalletBalanceChanged)
}

func winThatWouldOverflowFailsWithoutEffect(t *testing.T, s *suite) {
	walletID, playerID := s.openWallet(t, "92233720368547758.00")
	win := s.win(t, walletID, playerID, "1.00")

	for range 2 {
		_, err := s.execute(t, win)
		if err == nil {
			t.Fatal("a win past the largest balance succeeded")
		}
		for _, sentinel := range []error{engine.ErrWalletNotFound, engine.ErrIdempotencyConflict, engine.ErrIdempotencyKeyMismatch} {
			if errors.Is(err, sentinel) {
				t.Fatalf("overflow reported as %v", sentinel)
			}
		}
	}
	s.requireWallet(t, walletID, "92233720368547758.00", 1)
}

func lossDoesNotMoveMoney(t *testing.T, s *suite) {
	walletID, playerID := s.openWallet(t, "100.00")
	out := s.mustExecute(t, s.wager(t, walletID, playerID, wager.KindLoss, brl(t, "0.00"), ""))
	requireOutcome(t, out, wager.StatusProcessed, "", "100.00")
	s.requireWallet(t, walletID, "100.00", 1)
	if n := s.h.LedgerCount(t, out.TransactionID); n != 0 {
		t.Errorf("loss has %d ledger entries", n)
	}
	s.requireEventTypes(t, out.TransactionID, events.TypeWagerTransactionProcessed)
}

func lossInAnotherCurrencyIsRejected(t *testing.T, s *suite) {
	walletID, playerID := s.openWallet(t, "100.00")
	usd, err := money.New("0.00", "USD")
	if err != nil {
		t.Fatal(err)
	}
	out := s.mustExecute(t, s.wager(t, walletID, playerID, wager.KindLoss, usd, ""))
	requireOutcome(t, out, wager.StatusRejected, wager.FailureCurrencyMismatch, "")
	s.requireEventTypes(t, out.TransactionID, events.TypeWagerTransactionRejected)
}

func refundReturnsTheBet(t *testing.T, s *suite) {
	walletID, playerID := s.openWallet(t, "100.00")
	bet := s.bet(t, walletID, playerID, "30.00")
	placed := s.mustExecute(t, bet)

	out := s.mustExecute(t, s.reversal(t, wager.KindRefund, bet))
	requireOutcome(t, out, wager.StatusProcessed, "", "100.00")
	s.requireWallet(t, walletID, "100.00", 3)
	if ref := s.h.Transaction(t, out.TransactionID).ResolvedReferenceID(); ref != placed.TransactionID {
		t.Errorf("refund references %q, want %s", ref, placed.TransactionID)
	}
	s.requireEventTypes(t, out.TransactionID, events.TypeWagerTransactionProcessed, events.TypeWalletBalanceChanged)
}

func secondReversalIsRejected(t *testing.T, s *suite) {
	walletID, playerID := s.openWallet(t, "100.00")
	bet := s.bet(t, walletID, playerID, "30.00")
	s.mustExecute(t, bet)
	s.mustExecute(t, s.reversal(t, wager.KindRefund, bet))

	for _, kind := range []wager.Kind{wager.KindRollback, wager.KindRefund} {
		out := s.mustExecute(t, s.reversal(t, kind, bet))
		requireOutcome(t, out, wager.StatusRejected, wager.FailureReferenceAlreadyReversed, "")
		s.requireEventTypes(t, out.TransactionID, events.TypeWagerTransactionRejected)
	}
	s.requireWallet(t, walletID, "100.00", 3)
}

func rollbackUndoesAWin(t *testing.T, s *suite) {
	walletID, playerID := s.openWallet(t, "100.00")
	win := s.win(t, walletID, playerID, "20.00")
	s.mustExecute(t, win)
	out := s.mustExecute(t, s.reversal(t, wager.KindRollback, win))
	requireOutcome(t, out, wager.StatusProcessed, "", "100.00")
	s.requireWallet(t, walletID, "100.00", 3)
}

func rollbackOfAWinWithoutBalanceIsRejected(t *testing.T, s *suite) {
	walletID, playerID := s.openWallet(t, "0.00")
	win := s.win(t, walletID, playerID, "20.00")
	s.mustExecute(t, win)
	s.mustExecute(t, s.bet(t, walletID, playerID, "15.00"))

	out := s.mustExecute(t, s.reversal(t, wager.KindRollback, win))
	requireOutcome(t, out, wager.StatusRejected, wager.FailureRollbackInsufficientBalance, "")
	s.requireWallet(t, walletID, "5.00", 3)
}

func rollbackUndoesARefund(t *testing.T, s *suite) {
	walletID, playerID := s.openWallet(t, "100.00")
	bet := s.bet(t, walletID, playerID, "30.00")
	s.mustExecute(t, bet)
	refund := s.reversal(t, wager.KindRefund, bet)
	s.mustExecute(t, refund)

	out := s.mustExecute(t, s.reversal(t, wager.KindRollback, refund))
	requireOutcome(t, out, wager.StatusProcessed, "", "70.00")
	s.requireWallet(t, walletID, "70.00", 4)
}

func refundOfAWinIsNotReversible(t *testing.T, s *suite) {
	walletID, playerID := s.openWallet(t, "100.00")
	win := s.win(t, walletID, playerID, "20.00")
	s.mustExecute(t, win)
	out := s.mustExecute(t, s.reversal(t, wager.KindRefund, win))
	requireOutcome(t, out, wager.StatusRejected, wager.FailureReferenceKindNotReversible, "")
	s.requireWallet(t, walletID, "120.00", 2)
}

func reversalOfARejectedBetIsNotProcessed(t *testing.T, s *suite) {
	walletID, playerID := s.openWallet(t, "10.00")
	bet := s.bet(t, walletID, playerID, "30.00")
	s.mustExecute(t, bet)
	out := s.mustExecute(t, s.reversal(t, wager.KindRefund, bet))
	requireOutcome(t, out, wager.StatusRejected, wager.FailureReferenceNotProcessed, "")
	s.requireWallet(t, walletID, "10.00", 1)
}

func reversalWithADifferentAmountMismatches(t *testing.T, s *suite) {
	walletID, playerID := s.openWallet(t, "100.00")
	bet := s.bet(t, walletID, playerID, "30.00")
	s.mustExecute(t, bet)
	refund := s.reversal(t, wager.KindRefund, bet)
	refund.Amount = brl(t, "20.00")
	out := s.mustExecute(t, rehash(refund))
	requireOutcome(t, out, wager.StatusRejected, wager.FailureReferenceMismatch, "")
	s.requireWallet(t, walletID, "70.00", 2)
}

func reversalBeforeItsReferenceIsPending(t *testing.T, s *suite) {
	walletID, playerID := s.openWallet(t, "100.00")
	refund := s.wager(t, walletID, playerID, wager.KindRefund, brl(t, "30.00"), "ext-not-yet-"+newID())
	out := s.mustExecute(t, refund)

	requireOutcome(t, out, wager.StatusPendingReference, "", "")
	if got := s.h.Transaction(t, out.TransactionID).Status(); got != wager.StatusPendingReference {
		t.Errorf("stored status = %s", got)
	}
	if n := s.h.LedgerCount(t, out.TransactionID); n != 0 {
		t.Errorf("pending reversal has %d ledger entries", n)
	}
	s.requireEventTypes(t, out.TransactionID, events.TypeWagerTransactionPendingReference)
	s.requireWallet(t, walletID, "100.00", 1)
}

// pendingRefund submits a REFUND for a bet that does not exist yet and
// returns it, the bet it waits for and the refund's transaction ID.
func (s *suite) pendingRefund(t *testing.T, walletID, playerID string) (refund, bet engine.SubmitWager, refundID string) {
	t.Helper()
	bet = s.bet(t, walletID, playerID, "30.00")
	refund = s.reversal(t, wager.KindRefund, bet)
	out := s.mustExecute(t, refund)
	if out.Status != wager.StatusPendingReference {
		t.Fatalf("refund before its bet: status %s", out.Status)
	}
	return refund, bet, out.TransactionID
}

func retryResolvesAPendingReversal(t *testing.T, s *suite) {
	walletID, playerID := s.openWallet(t, "100.00")
	_, bet, refundID := s.pendingRefund(t, walletID, playerID)
	placed := s.mustExecute(t, bet)

	out := s.mustExecute(t, engine.RetryReference{TransactionID: refundID, Attempts: 1})
	if out.TransactionID != refundID || out.Replay {
		t.Errorf("retry outcome = %+v", out)
	}
	requireOutcome(t, out, wager.StatusProcessed, "", "100.00")
	s.requireWallet(t, walletID, "100.00", 3)
	if ref := s.h.Transaction(t, refundID).ResolvedReferenceID(); ref != placed.TransactionID {
		t.Errorf("resolved reference = %q, want %s", ref, placed.TransactionID)
	}
	s.requireEventTypes(t, refundID,
		events.TypeWagerTransactionPendingReference, events.TypeWagerTransactionProcessed, events.TypeWalletBalanceChanged)
}

func retryKeepsWaitingBelowTheLimit(t *testing.T, s *suite) {
	walletID, playerID := s.openWallet(t, "100.00")
	_, _, refundID := s.pendingRefund(t, walletID, playerID)

	for _, attempts := range []int{1, wager.MaxReferenceAttempts - 1} {
		out := s.mustExecute(t, engine.RetryReference{TransactionID: refundID, Attempts: attempts})
		requireOutcome(t, out, wager.StatusPendingReference, "", "")
	}
	s.requireEventTypes(t, refundID, events.TypeWagerTransactionPendingReference)
}

func retryGivesUpAtTheLimit(t *testing.T, s *suite) {
	walletID, playerID := s.openWallet(t, "100.00")
	_, _, refundID := s.pendingRefund(t, walletID, playerID)

	out := s.mustExecute(t, engine.RetryReference{TransactionID: refundID, Attempts: wager.MaxReferenceAttempts})
	requireOutcome(t, out, wager.StatusRejected, wager.FailureReferenceNotFoundTimeout, "")
	if got := s.h.Transaction(t, refundID); got.Status() != wager.StatusRejected ||
		got.FailureCode() != wager.FailureReferenceNotFoundTimeout {
		t.Errorf("stored = %s %s", got.Status(), got.FailureCode())
	}
	s.requireEventTypes(t, refundID, events.TypeWagerTransactionPendingReference, events.TypeWagerTransactionRejected)
	s.requireWallet(t, walletID, "100.00", 1)
}

func retryOfASettledTransactionChangesNothing(t *testing.T, s *suite) {
	walletID, playerID := s.openWallet(t, "100.00")
	placed := s.mustExecute(t, s.bet(t, walletID, playerID, "30.00"))

	out := s.mustExecute(t, engine.RetryReference{TransactionID: placed.TransactionID, Attempts: 1})
	if !out.Replay || out.TransactionID != placed.TransactionID {
		t.Errorf("retry of a processed bet = %+v", out)
	}
	requireOutcome(t, out, wager.StatusProcessed, "", "70.00")

	_, bet, refundID := s.pendingRefund(t, walletID, playerID)
	s.mustExecute(t, bet)
	s.mustExecute(t, engine.RetryReference{TransactionID: refundID, Attempts: 1})
	again := s.mustExecute(t, engine.RetryReference{TransactionID: refundID, Attempts: 2})
	if !again.Replay {
		t.Error("retry of a resolved reversal was not a replay")
	}
	requireOutcome(t, again, wager.StatusProcessed, "", "70.00")
	s.requireWallet(t, walletID, "70.00", 4)
}

func retryOfAnUnknownTransactionIsNotFound(t *testing.T, s *suite) {
	_, err := s.execute(t, engine.RetryReference{TransactionID: newID(), Attempts: 1})
	if !errors.Is(err, engine.ErrTransactionNotFound) {
		t.Fatalf("err = %v, want ErrTransactionNotFound", err)
	}
}

// concurrently runs n copies of fn and collects their outcomes and errors.
func concurrently(n int, fn func(i int) (engine.Outcome, error)) ([]engine.Outcome, []error) {
	outs := make([]engine.Outcome, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			outs[i], errs[i] = fn(i)
		})
	}
	close(start)
	wg.Wait()
	return outs, errs
}

func concurrentBetsNeverOverdraw(t *testing.T, s *suite) {
	walletID, playerID := s.openWallet(t, "100.00")
	outs, errs := concurrently(20, func(int) (engine.Outcome, error) {
		return s.execute(t, s.bet(t, walletID, playerID, "30.00"))
	})

	processed := 0
	for i, out := range outs {
		if errs[i] != nil {
			t.Fatalf("bet %d: %v", i, errs[i])
		}
		switch {
		case out.Status == wager.StatusProcessed:
			processed++
		case out.Status != wager.StatusRejected || out.FailureCode != wager.FailureInsufficientBalance:
			t.Errorf("bet %d = %s %s", i, out.Status, out.FailureCode)
		}
	}
	if processed != 3 {
		t.Errorf("%d bets processed, want 3", processed)
	}
	s.requireWallet(t, walletID, "10.00", 4)
}

func concurrentDuplicatesMoveMoneyOnce(t *testing.T, s *suite) {
	walletID, playerID := s.openWallet(t, "100.00")
	bet := s.bet(t, walletID, playerID, "30.00")
	outs, errs := concurrently(50, func(int) (engine.Outcome, error) { return s.execute(t, bet) })

	firsts := 0
	for i, out := range outs {
		if errs[i] != nil {
			t.Fatalf("submission %d: %v", i, errs[i])
		}
		if out.TransactionID != outs[0].TransactionID {
			t.Fatalf("submission %d answered %s, others %s", i, out.TransactionID, outs[0].TransactionID)
		}
		requireOutcome(t, out, wager.StatusProcessed, "", "70.00")
		if !out.Replay {
			firsts++
		}
	}
	if firsts != 1 {
		t.Errorf("%d submissions applied the bet, want 1", firsts)
	}
	s.requireWallet(t, walletID, "70.00", 2)
	if n := s.h.LedgerCount(t, outs[0].TransactionID); n != 1 {
		t.Errorf("bet has %d ledger entries, want 1", n)
	}
}

func concurrentKeysForOneExternalIDApplyOnce(t *testing.T, s *suite) {
	walletID, playerID := s.openWallet(t, "100.00")
	bet := s.bet(t, walletID, playerID, "30.00")
	outs, errs := concurrently(10, func(i int) (engine.Outcome, error) {
		cmd := bet
		cmd.IdempotencyKey = fmt.Sprintf("%s-%d", bet.IdempotencyKey, i)
		return s.execute(t, cmd)
	})

	applied := 0
	for i := range outs {
		switch {
		case errs[i] == nil && outs[i].Status == wager.StatusProcessed && !outs[i].Replay:
			applied++
		case errors.Is(errs[i], engine.ErrIdempotencyKeyMismatch):
		default:
			t.Errorf("submission %d = %+v, %v", i, outs[i], errs[i])
		}
	}
	if applied != 1 {
		t.Errorf("%d submissions applied the bet, want 1", applied)
	}
	s.requireWallet(t, walletID, "70.00", 2)
}

func concurrentReversalsOfOneBetApplyOnce(t *testing.T, s *suite) {
	walletID, playerID := s.openWallet(t, "100.00")
	bet := s.bet(t, walletID, playerID, "30.00")
	s.mustExecute(t, bet)

	outs, errs := concurrently(10, func(int) (engine.Outcome, error) {
		return s.execute(t, s.reversal(t, wager.KindRefund, bet))
	})
	applied := 0
	for i, out := range outs {
		if errs[i] != nil {
			t.Fatalf("refund %d: %v", i, errs[i])
		}
		switch {
		case out.Status == wager.StatusProcessed:
			applied++
		case out.FailureCode != wager.FailureReferenceAlreadyReversed:
			t.Errorf("refund %d = %s %s", i, out.Status, out.FailureCode)
		}
	}
	if applied != 1 {
		t.Errorf("%d refunds applied, want 1", applied)
	}
	s.requireWallet(t, walletID, "100.00", 3)
}

func eventsDescribeEveryOutcome(t *testing.T, s *suite) {
	started := time.Now().Add(-time.Minute)
	walletID, playerID := s.openWallet(t, "100.00")

	bet := s.bet(t, walletID, playerID, "30.00")
	bet.CorrelationID = "corr-" + newID()
	placed := s.mustExecute(t, bet)
	evs := s.h.Events(t, placed.TransactionID)
	if len(evs) != 2 {
		t.Fatalf("bet events = %d, want 2", len(evs))
	}
	processed, balance := evs[0], evs[1]
	requireEnvelope(t, processed, events.TypeWagerTransactionProcessed, placed.TransactionID, bet.CorrelationID, "", started)
	requireEnvelope(t, balance, events.TypeWalletBalanceChanged, walletID, bet.CorrelationID, processed.EventID, started)
	requireData(t, processed, events.WagerTransactionProcessedData{
		TransactionID: placed.TransactionID, WalletID: walletID, Kind: "BET",
		Money: events.MoneyDTO{Amount: "30.00", Currency: "BRL"},
	})
	requireData(t, balance, events.WalletBalanceChangedData{
		WalletID: walletID, TransactionID: placed.TransactionID, Direction: "DEBIT",
		Money:         events.MoneyDTO{Amount: "30.00", Currency: "BRL"},
		BalanceBefore: events.MoneyDTO{Amount: "100.00", Currency: "BRL"},
		BalanceAfter:  events.MoneyDTO{Amount: "70.00", Currency: "BRL"},
		WalletVersion: 2,
	})

	drained := s.mustExecute(t, s.bet(t, walletID, playerID, "70.00"))
	evs = s.h.Events(t, drained.TransactionID)
	requireData(t, evs[1], events.WalletBalanceChangedData{
		WalletID: walletID, TransactionID: drained.TransactionID, Direction: "DEBIT",
		Money:         events.MoneyDTO{Amount: "70.00", Currency: "BRL"},
		BalanceBefore: events.MoneyDTO{Amount: "70.00", Currency: "BRL"},
		BalanceAfter:  events.MoneyDTO{Amount: "0.00", Currency: "BRL"},
		WalletVersion: 3,
	})

	rejected := s.mustExecute(t, s.bet(t, walletID, playerID, "1.00"))
	evs = s.h.Events(t, rejected.TransactionID)
	requireEnvelope(t, evs[0], events.TypeWagerTransactionRejected, rejected.TransactionID, rejected.TransactionID, "", started)
	requireData(t, evs[0], events.WagerTransactionRejectedData{
		TransactionID: rejected.TransactionID, FailureCode: wager.FailureInsufficientBalance,
	})

	reference := "ext-later-" + newID()
	pending := s.mustExecute(t, s.wager(t, walletID, playerID, wager.KindRollback, brl(t, "5.00"), reference))
	evs = s.h.Events(t, pending.TransactionID)
	requireEnvelope(t, evs[0], events.TypeWagerTransactionPendingReference, pending.TransactionID, pending.TransactionID, "", started)
	requireData(t, evs[0], events.WagerTransactionPendingReferenceData{
		TransactionID: pending.TransactionID, ReferenceExternalTransactionID: reference,
	})

	rich, richPlayer := s.openWallet(t, "92233720368547758.07")
	lost := s.mustExecute(t, s.wager(t, rich, richPlayer, wager.KindLoss, brl(t, "0.00"), ""))
	evs = s.h.Events(t, lost.TransactionID)
	requireData(t, evs[0], events.WagerTransactionProcessedData{
		TransactionID: lost.TransactionID, WalletID: rich, Kind: "LOSS",
		Money: events.MoneyDTO{Amount: "0.00", Currency: "BRL"},
	})
	requireOutcome(t, lost, wager.StatusProcessed, "", "92233720368547758.07")
}

func requireEnvelope(t *testing.T, e events.Envelope, eventType, aggregateID, correlationID, causationID string, notBefore time.Time) {
	t.Helper()
	if e.EventType != eventType || e.AggregateID != aggregateID || e.CorrelationID != correlationID ||
		e.CausationID != causationID || e.Version != 1 {
		t.Errorf("envelope = %s agg=%s corr=%s cause=%q v%d, want %s agg=%s corr=%s cause=%q v1",
			e.EventType, e.AggregateID, e.CorrelationID, e.CausationID, e.Version,
			eventType, aggregateID, correlationID, causationID)
	}
	requireVersion7(t, "event ID", e.EventID)
	at, err := time.Parse(time.RFC3339Nano, e.OccurredAt)
	if err != nil {
		t.Errorf("occurredAt %q: %v", e.OccurredAt, err)
	} else if at.Before(notBefore) || at.After(time.Now().Add(time.Minute)) || at.Location() != time.UTC {
		t.Errorf("occurredAt %s outside the test's run or not UTC", e.OccurredAt)
	}
}

// requireData decodes the event's data strictly into want's type and
// compares the two.
func requireData[T comparable](t *testing.T, e events.Envelope, want T) {
	t.Helper()
	var got T
	decoder := json.NewDecoder(strings.NewReader(string(e.Data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&got); err != nil {
		t.Errorf("%s data %s: %v", e.EventType, e.Data, err)
		return
	}
	if got != want {
		t.Errorf("%s data = %+v, want %+v", e.EventType, got, want)
	}
}

func idsAreVersion7(t *testing.T, s *suite) {
	walletID, playerID := s.openWallet(t, "100.00")
	requireVersion7(t, "wallet ID", walletID)
	out := s.mustExecute(t, s.bet(t, walletID, playerID, "10.00"))
	requireVersion7(t, "transaction ID", out.TransactionID)
	for _, e := range s.h.Events(t, out.TransactionID) {
		requireVersion7(t, "event ID", e.EventID)
	}
}

func requireVersion7(t *testing.T, what, id string) {
	t.Helper()
	parsed, err := uuid.Parse(id)
	if err != nil || parsed.Version() != 7 {
		t.Errorf("%s %q is not a version 7 UUID", what, id)
	}
}

func brl(t *testing.T, amount string) money.Money {
	t.Helper()
	m, err := money.New(amount, "BRL")
	if err != nil {
		t.Fatalf("money.New(%q): %v", amount, err)
	}
	return m
}

func newID() string {
	return uuid.NewString()
}
