// Package engine is the port every money-moving implementation sits
// behind: the PostgreSQL engine, the in-memory reference model and, later,
// a single-writer engine. A command either takes effect completely, with
// its transaction, ledger entry and events, or not at all.
package engine

import (
	"context"
	"errors"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/money"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/wager"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/wallet"
)

type Engine interface {
	Execute(ctx context.Context, cmd Command) (Outcome, error)
}

// Command is one of OpenWallet, SubmitWager or RetryReference.
type Command interface{ isCommand() }

// OpenWallet creates a wallet and, for a positive initial balance, the
// OPENING transaction that credits it.
type OpenWallet struct {
	PlayerID       string
	InitialBalance money.Money
}

// SubmitWager applies a provider's transaction once. Its fields are already
// validated and canonical; PayloadHash identifies the business payload, so
// a resend under the same key with another payload is a conflict.
type SubmitWager struct {
	ProviderID          string
	ExternalID          string
	IdempotencyKey      string
	PayloadHash         string
	WalletID            string
	PlayerID            string
	RoundID             string
	GameID              string
	Kind                wager.Kind
	Amount              money.Money
	ReferenceExternalID string
	// CorrelationID ties the events to the request that caused them; empty
	// means the transaction's own ID.
	CorrelationID string
}

// RetryReference tries again to resolve a PENDING_REFERENCE reversal.
// Attempts is how many attempts it has had so far.
type RetryReference struct {
	TransactionID string
	Attempts      int
}

func (OpenWallet) isCommand()     {}
func (SubmitWager) isCommand()    {}
func (RetryReference) isCommand() {}

// Outcome is what a command left behind.
type Outcome struct {
	// TransactionID is the transaction the command created or replayed: the
	// OPENING one for OpenWallet, empty when it opened a zero balance.
	TransactionID string
	Status        wager.Status
	FailureCode   string
	// Balance is the wallet balance right after the command for a PROCESSED
	// transaction (the current balance for a LOSS), and the zero Money (no
	// currency) for any other status. A replay answers what the first
	// application answered.
	Balance money.Money
	// Replay means nothing new was written: the command had already been
	// applied, or the reversal it retries is already settled.
	Replay bool
	// Raced means the command lost a race against a concurrent one with the
	// same identity and is answered as a replay of it.
	Raced bool
	// Wallet is the opened wallet, for OpenWallet only.
	Wallet wallet.Wallet
}

var (
	ErrWalletNotFound         = errors.New("wallet not found")
	ErrWalletConflict         = errors.New("wallet already exists for player and currency")
	ErrIdempotencyConflict    = errors.New("idempotency key reused with different payload")
	ErrIdempotencyKeyMismatch = errors.New("transaction already recorded under a different idempotency key")
	ErrTransactionNotFound    = errors.New("transaction not found")
	ErrUnknownCommand         = errors.New("unknown engine command")
)
