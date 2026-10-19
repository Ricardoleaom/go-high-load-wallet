package wager

import (
	"testing"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/ledger"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/money"
)

// processed rehydrates a PROCESSED transaction from in, as the original a
// reversal names would be read back.
func processed(t *testing.T, in Input) Transaction {
	t.Helper()
	tx, err := New(in)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.MarkProcessed(); err != nil {
		t.Fatal(err)
	}
	return tx
}

func reversalOf(t *testing.T, kind Kind, original Input) Transaction {
	t.Helper()
	in := original
	in.ID = "reversal-1"
	in.ExternalID = "reversal-external-1"
	in.IdempotencyKey = "reversal-key-1"
	in.Kind = kind
	in.ReferenceExternalID = original.ExternalID
	tx, err := New(in)
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

func TestDecideReversal(t *testing.T) {
	bet := testInput(t, KindBet, "25.00", "")
	win := testInput(t, KindWin, "25.00", "")

	pending, err := New(bet)
	if err != nil {
		t.Fatal(err)
	}
	pendingReference, err := New(testInput(t, KindRefund, "25.00", "elsewhere"))
	if err != nil {
		t.Fatal(err)
	}
	if err := pendingReference.MarkPendingReference(); err != nil {
		t.Fatal(err)
	}
	rejected, err := New(bet)
	if err != nil {
		t.Fatal(err)
	}
	if err := rejected.Reject(FailureInsufficientBalance); err != nil {
		t.Fatal(err)
	}
	refund := processed(t, func() Input {
		in := testInput(t, KindRefund, "25.00", "some-bet")
		return in
	}())

	differs := func(change func(*Input)) Transaction {
		in := bet
		change(&in)
		return processed(t, in)
	}
	otherCurrency, err := money.New("25.00", "USD")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		original Transaction
		found    bool
		reversal Transaction
		want     ReversalDecision
		failure  string
	}{
		{"original not found", Transaction{}, false, reversalOf(t, KindRefund, bet), ReversalWait, ""},
		{"original still pending", pending, true, reversalOf(t, KindRefund, bet), ReversalWait, ""},
		{"original waiting for its own reference", pendingReference, true, reversalOf(t, KindRollback, bet), ReversalWait, ""},
		{"original rejected", rejected, true, reversalOf(t, KindRefund, bet), ReversalReject, FailureReferenceNotProcessed},
		{"another provider", differs(func(in *Input) { in.ProviderID = "provider-2" }), true, reversalOf(t, KindRefund, bet), ReversalReject, FailureReferenceMismatch},
		{"another player", differs(func(in *Input) { in.PlayerID = "player-2" }), true, reversalOf(t, KindRefund, bet), ReversalReject, FailureReferenceMismatch},
		{"another wallet", differs(func(in *Input) { in.WalletID = "wallet-2" }), true, reversalOf(t, KindRefund, bet), ReversalReject, FailureReferenceMismatch},
		{"another round", differs(func(in *Input) { in.RoundID = "round-2" }), true, reversalOf(t, KindRefund, bet), ReversalReject, FailureReferenceMismatch},
		{"another currency", differs(func(in *Input) { in.Amount = otherCurrency }), true, reversalOf(t, KindRefund, bet), ReversalReject, FailureReferenceMismatch},
		{"another amount", differs(func(in *Input) { in.Amount = mustMoney(t, "24.99") }), true, reversalOf(t, KindRefund, bet), ReversalReject, FailureReferenceMismatch},
		{"refund of a win", processed(t, win), true, reversalOf(t, KindRefund, win), ReversalReject, FailureReferenceKindNotReversible},
		{"refund of a bet", processed(t, bet), true, reversalOf(t, KindRefund, bet), ReversalApply, ""},
		{"rollback of a bet", processed(t, bet), true, reversalOf(t, KindRollback, bet), ReversalApply, ""},
		{"rollback of a win", processed(t, win), true, reversalOf(t, KindRollback, win), ReversalApply, ""},
		{"rollback of a refund", refund, true, reversalOf(t, KindRollback, testInput(t, KindRefund, "25.00", "some-bet")), ReversalApply, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, failure := DecideReversal(tt.original, tt.found, tt.reversal)
			if got != tt.want || failure != tt.failure {
				t.Errorf("DecideReversal = (%v, %q), want (%v, %q)", got, failure, tt.want, tt.failure)
			}
		})
	}
}

func TestReversalDirection(t *testing.T) {
	tests := []struct {
		reversal, original Kind
		want               ledger.Direction
	}{
		{KindRefund, KindBet, ledger.DirectionCredit},
		{KindRollback, KindBet, ledger.DirectionCredit},
		{KindRollback, KindWin, ledger.DirectionDebit},
		{KindRollback, KindRefund, ledger.DirectionDebit},
	}
	for _, tt := range tests {
		if got := ReversalDirection(tt.reversal, tt.original); got != tt.want {
			t.Errorf("ReversalDirection(%s, %s) = %s, want %s", tt.reversal, tt.original, got, tt.want)
		}
	}
}

func mustMoney(t *testing.T, amount string) money.Money {
	t.Helper()
	m, err := money.New(amount, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return m
}
