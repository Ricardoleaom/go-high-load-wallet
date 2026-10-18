package wager

import (
	"errors"
	"testing"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/money"
)

func testInput(t *testing.T, kind Kind, amount string, reference string) Input {
	t.Helper()

	value, err := money.New(amount, "BRL")
	if err != nil {
		t.Fatal(err)
	}

	return Input{
		ID:                  "tx-1",
		ProviderID:          "provider-1",
		ExternalID:          "external-1",
		IdempotencyKey:      "provider-1:external-1",
		PayloadHash:         "hash-1",
		WalletID:            "wallet-1",
		PlayerID:            "player-1",
		RoundID:             "round-1",
		GameID:              "game-1",
		Kind:                kind,
		Amount:              value,
		ReferenceExternalID: reference,
	}
}

func TestNewTransaction(t *testing.T) {
	tx, err := New(testInput(t, KindBet, "25.00", ""))
	if err != nil {
		t.Fatal(err)
	}

	if tx.Status() != StatusPending {
		t.Errorf("expected PENDING, got %s", tx.Status())
	}
	if tx.Kind() != KindBet {
		t.Errorf("expected BET, got %s", tx.Kind())
	}
	if tx.Amount().AmountInMinorUnits() != 2500 {
		t.Errorf("unexpected amount: %s", tx.Amount())
	}
}

func TestNewValidatesOperationAmounts(t *testing.T) {
	tests := []struct {
		name      string
		kind      Kind
		amount    string
		reference string
		wantErr   bool
	}{
		{"positive bet", KindBet, "10.00", "", false},
		{"positive win", KindWin, "10.00", "", false},
		{"zero loss", KindLoss, "0.00", "", false},
		{"positive refund with reference", KindRefund, "10.00", "bet-1", false},
		{"positive rollback with reference", KindRollback, "10.00", "bet-1", false},
		{"zero bet", KindBet, "0.00", "", true},
		{"loss must be zero", KindLoss, "1.00", "", true},
		{"refund requires reference", KindRefund, "10.00", "", true},
		{"rollback requires reference", KindRollback, "10.00", "", true},
		{"unknown kind", Kind("UNKNOWN"), "10.00", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(testInput(t, tt.kind, tt.amount, tt.reference))

			if (err != nil) != tt.wantErr {
				t.Fatalf("expected error=%v, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestTransactionTransitions(t *testing.T) {
	tx, err := New(testInput(t, KindRefund, "10.00", "bet-1"))
	if err != nil {
		t.Fatal(err)
	}

	if err := tx.MarkPendingReference(); err != nil {
		t.Fatal(err)
	}
	if tx.Status() != StatusPendingReference {
		t.Fatalf("expected PENDING_REFERENCE, got %s", tx.Status())
	}

	if err := tx.MarkProcessed(); err != nil {
		t.Fatal(err)
	}
	if tx.Status() != StatusProcessed {
		t.Fatalf("expected PROCESSED, got %s", tx.Status())
	}

	if err := tx.Reject("INSUFFICIENT_BALANCE"); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("expected invalid transition, got %v", err)
	}
}

func TestRejectedTransactionIsTerminal(t *testing.T) {
	tx, err := New(testInput(t, KindBet, "10.00", ""))
	if err != nil {
		t.Fatal(err)
	}

	if err := tx.Reject("INSUFFICIENT_BALANCE"); err != nil {
		t.Fatal(err)
	}

	if tx.Status() != StatusRejected {
		t.Fatalf("expected REJECTED, got %s", tx.Status())
	}
	if tx.FailureCode() != "INSUFFICIENT_BALANCE" {
		t.Errorf("unexpected failure code: %s", tx.FailureCode())
	}
	if err := tx.MarkProcessed(); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("expected invalid transition, got %v", err)
	}
}

func TestFailedTransactionIsTerminal(t *testing.T) {
	tx, err := New(testInput(t, KindBet, "10.00", ""))
	if err != nil {
		t.Fatal(err)
	}

	if err := tx.Fail("DATABASE_UNAVAILABLE"); err != nil {
		t.Fatal(err)
	}

	if tx.Status() != StatusFailed {
		t.Fatalf("expected FAILED, got %s", tx.Status())
	}
	if err := tx.MarkPendingReference(); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("expected invalid transition, got %v", err)
	}
}

func TestNewRejectsMissingRequiredFields(t *testing.T) {
	input := testInput(t, KindBet, "10.00", "")
	input.ProviderID = ""

	_, err := New(input)
	if !errors.Is(err, ErrInvalidTransaction) {
		t.Fatalf("expected invalid transaction, got %v", err)
	}
}

func TestNewRejectsNegativeAmount(t *testing.T) {
	zero, err := money.New("0.00", "BRL")
	if err != nil {
		t.Fatal(err)
	}

	one, err := money.New("1.00", "BRL")
	if err != nil {
		t.Fatal(err)
	}

	negative, err := zero.Subtract(one)
	if err != nil {
		t.Fatal(err)
	}

	input := testInput(t, KindBet, "10.00", "")
	input.Amount = negative

	_, err = New(input)
	if !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("expected invalid amount, got %v", err)
	}
}

func TestKindKnown(t *testing.T) {
	for _, kind := range []Kind{KindOpening, KindBet, KindWin, KindLoss, KindRefund, KindRollback} {
		if !kind.Known() {
			t.Errorf("%s reported as unknown", kind)
		}
	}
	for _, kind := range []Kind{"", "bet", "JACKPOT"} {
		if kind.Known() {
			t.Errorf("%q reported as known", kind)
		}
	}
}

// ValidateRequest checks what a provider sends, before the service assigns
// the transaction ID and the payload hash.
func TestValidateRequest(t *testing.T) {
	valid := testInput(t, KindBet, "25.00", "")
	valid.ID, valid.PayloadHash = "", ""
	if err := ValidateRequest(valid); err != nil {
		t.Fatalf("a valid bet without ID and hash: %v", err)
	}

	for name, tc := range map[string]struct {
		change func(*Input)
		want   error
	}{
		"no wallet":                  {func(in *Input) { in.WalletID = "" }, ErrInvalidTransaction},
		"no player":                  {func(in *Input) { in.PlayerID = "" }, ErrInvalidTransaction},
		"no provider":                {func(in *Input) { in.ProviderID = "" }, ErrInvalidTransaction},
		"no external ID":             {func(in *Input) { in.ExternalID = "" }, ErrInvalidTransaction},
		"no idempotency key":         {func(in *Input) { in.IdempotencyKey = "" }, ErrInvalidTransaction},
		"no round":                   {func(in *Input) { in.RoundID = "" }, ErrInvalidTransaction},
		"no game":                    {func(in *Input) { in.GameID = "" }, ErrInvalidTransaction},
		"unknown kind":               {func(in *Input) { in.Kind = "JACKPOT" }, ErrInvalidTransaction},
		"zero bet":                   {func(in *Input) { in.Amount = mustMoney(t, "0.00") }, ErrInvalidAmount},
		"loss with an amount":        {func(in *Input) { in.Kind = KindLoss }, ErrInvalidAmount},
		"refund without reference":   {func(in *Input) { in.Kind = KindRefund }, ErrInvalidAmount},
		"rollback without reference": {func(in *Input) { in.Kind = KindRollback }, ErrInvalidAmount},
	} {
		in := valid
		tc.change(&in)
		if err := ValidateRequest(in); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
}
