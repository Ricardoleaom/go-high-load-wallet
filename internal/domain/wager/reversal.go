package wager

import "github.com/Ricardoleaom/go-high-load-wallet/internal/domain/ledger"

// Failure codes a transaction can be rejected with.
const (
	FailureInsufficientBalance         = "INSUFFICIENT_BALANCE"
	FailureRollbackInsufficientBalance = "ROLLBACK_INSUFFICIENT_BALANCE"
	FailureCurrencyMismatch            = "CURRENCY_MISMATCH"
	FailureReferenceNotProcessed       = "REFERENCE_NOT_PROCESSED"
	FailureReferenceMismatch           = "REFERENCE_MISMATCH"
	FailureReferenceKindNotReversible  = "REFERENCE_KIND_NOT_REVERSIBLE"
	FailureReferenceAlreadyReversed    = "REFERENCE_ALREADY_REVERSED"
	FailureReferenceNotFoundTimeout    = "REFERENCE_NOT_FOUND_TIMEOUT"
)

// ReversalDecision is what a REFUND or ROLLBACK does with the original
// transaction it names.
type ReversalDecision int

const (
	// ReversalWait: the original is unknown or not settled yet.
	ReversalWait ReversalDecision = iota
	// ReversalReject: the original can never be reversed by this request.
	ReversalReject
	// ReversalApply: the original can be reversed, as long as no other
	// reversal got there first and, for a debit, the balance covers it.
	ReversalApply
)

// DecideReversal checks a reversal against its original, found reporting
// whether the original exists at all. Rejections come with their failure
// code. Whether the original was already reversed, and whether the wallet
// covers a debit, depend on state beyond the two transactions and are left
// to the engine.
func DecideReversal(original Transaction, found bool, reversal Transaction) (ReversalDecision, string) {
	if !found || original.Status() == StatusPending || original.Status() == StatusPendingReference {
		return ReversalWait, ""
	}
	if original.Status() != StatusProcessed {
		return ReversalReject, FailureReferenceNotProcessed
	}
	if !referenceMatches(original, reversal) {
		return ReversalReject, FailureReferenceMismatch
	}
	if !reversibleKind(reversal.Kind(), original.Kind()) {
		return ReversalReject, FailureReferenceKindNotReversible
	}
	return ReversalApply, ""
}

func referenceMatches(original, reversal Transaction) bool {
	if original.ProviderID() != reversal.ProviderID() ||
		original.PlayerID() != reversal.PlayerID() ||
		original.WalletID() != reversal.WalletID() ||
		original.RoundID() != reversal.RoundID() ||
		original.Amount().Currency() != reversal.Amount().Currency() {
		return false
	}

	cmp, err := original.Amount().Compare(reversal.Amount())
	return err == nil && cmp == 0
}

func reversibleKind(reversalKind, originalKind Kind) bool {
	switch reversalKind {
	case KindRefund:
		return originalKind == KindBet
	case KindRollback:
		return originalKind == KindBet ||
			originalKind == KindWin ||
			originalKind == KindRefund
	default:
		return false
	}
}

// ReversalDirection reports which way money moves to undo the original:
// a REFUND always credits back a BET; a ROLLBACK undoes whatever the
// original movement was.
func ReversalDirection(reversalKind, originalKind Kind) ledger.Direction {
	if reversalKind == KindRefund || originalKind == KindBet {
		return ledger.DirectionCredit
	}
	return ledger.DirectionDebit
}
