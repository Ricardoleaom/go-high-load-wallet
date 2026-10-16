package wallet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/application/outboxevents"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/ledger"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/money"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/wager"
	domainwallet "github.com/Ricardoleaom/go-high-load-wallet/internal/domain/wallet"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/infrastructure/postgres"
)

var ErrWalletConflict = postgres.ErrWalletConflict

type Service struct {
	db           *sql.DB
	wallets      *postgres.WalletRepository
	transactions *postgres.TransactionRepository
	ledger       *postgres.LedgerRepository
	outbox       *postgres.OutboxRepository
}

func NewService(
	db *sql.DB,
	wallets *postgres.WalletRepository,
	transactions *postgres.TransactionRepository,
	ledgerRepo *postgres.LedgerRepository,
	outbox *postgres.OutboxRepository,
) *Service {
	return &Service{
		db:           db,
		wallets:      wallets,
		transactions: transactions,
		ledger:       ledgerRepo,
		outbox:       outbox,
	}
}

type OpenInput struct {
	PlayerID       string
	InitialBalance money.Money
}

// Open creates a wallet and, when the initial balance is positive, the
// OPENING transaction, its ledger credit and the matching events, all in a
// single commit. A zero initial balance creates only the wallet row.
func (s *Service) Open(ctx context.Context, input OpenInput) (domainwallet.Wallet, error) {
	w, err := domainwallet.New(uuid.NewString(), input.PlayerID, input.InitialBalance)
	if err != nil {
		return domainwallet.Wallet{}, err
	}

	dbTx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domainwallet.Wallet{}, fmt.Errorf("begin open wallet: %w", err)
	}
	defer dbTx.Rollback()

	if err := s.wallets.Create(ctx, dbTx, w); err != nil {
		if errors.Is(err, postgres.ErrWalletConflict) {
			return domainwallet.Wallet{}, ErrWalletConflict
		}
		return domainwallet.Wallet{}, fmt.Errorf("create wallet: %w", err)
	}

	if input.InitialBalance.AmountInMinorUnits() > 0 {
		if err := s.recordOpening(ctx, dbTx, w, input.InitialBalance); err != nil {
			return domainwallet.Wallet{}, err
		}
	}

	if err := dbTx.Commit(); err != nil {
		return domainwallet.Wallet{}, fmt.Errorf("commit open wallet: %w", err)
	}

	return w, nil
}

func (s *Service) recordOpening(
	ctx context.Context,
	dbTx *sql.Tx,
	w domainwallet.Wallet,
	initialBalance money.Money,
) error {
	zero, err := money.Zero(initialBalance.Currency())
	if err != nil {
		return err
	}

	openingTx, err := wager.New(wager.Input{
		ID:       uuid.NewString(),
		WalletID: w.ID(),
		PlayerID: w.PlayerID(),
		Kind:     wager.KindOpening,
		Amount:   initialBalance,
	})
	if err != nil {
		return err
	}

	if err := s.transactions.Create(ctx, dbTx, openingTx); err != nil {
		return fmt.Errorf("create opening transaction: %w", err)
	}

	entry, err := ledger.New(
		uuid.NewString(), w.ID(), openingTx.ID(),
		ledger.DirectionCredit,
		initialBalance, zero, initialBalance,
		openingTx.CreatedAt(),
	)
	if err != nil {
		return err
	}

	if err := s.ledger.Append(ctx, dbTx, entry); err != nil {
		return fmt.Errorf("append opening ledger entry: %w", err)
	}

	if err := openingTx.MarkProcessed(); err != nil {
		return err
	}
	if err := s.transactions.UpdateStatus(ctx, dbTx, openingTx); err != nil {
		return fmt.Errorf("mark opening processed: %w", err)
	}

	if err := outboxevents.AppendProcessed(
		ctx, dbTx, s.outbox, openingTx, &entry, w.Version(), openingTx.ID(),
	); err != nil {
		return err
	}

	return nil
}
