// Package queries is the read side: what the API shows about wallets and
// transactions, read straight from storage without going through an
// engine.
package queries

import (
	"context"
	"errors"
	"time"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/ledger"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/money"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/wager"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/wallet"
)

var (
	ErrWalletNotFound      = errors.New("wallet not found")
	ErrTransactionNotFound = errors.New("transaction not found")
)

type Reader interface {
	Wallet(ctx context.Context, id string) (wallet.Wallet, error)
	Transaction(ctx context.Context, id string) (TransactionView, error)
	TransactionByExternalID(ctx context.Context, providerID, externalID string) (TransactionView, error)
	// Ledger returns up to limit entries of a wallet, oldest first, strictly
	// after the cursor; the zero Cursor starts from the first entry.
	Ledger(ctx context.Context, walletID string, after Cursor, limit int) ([]ledger.Entry, error)
	Reconciliation(ctx context.Context, walletID string) (Reconciliation, error)
}

// TransactionView is a transaction with the balance it left the wallet
// at: the balance right after it for a movement, the current balance for
// any other PROCESSED transaction, and the zero Money otherwise.
type TransactionView struct {
	Transaction wager.Transaction
	Balance     money.Money
}

// Cursor is a position in a wallet's ledger.
type Cursor struct {
	CreatedAt time.Time
	ID        string
}

// Reconciliation compares a wallet's stored balance with the sum of its
// ledger, both read in one snapshot.
type Reconciliation struct {
	StoredBalance     money.Money
	CalculatedBalance money.Money
	CheckedEntries    int
}
