package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/money"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/wallet"
)

var (
	ErrWalletNotFound = errors.New("wallet not found")
	ErrWalletConflict = errors.New("wallet already exists for player and currency")
)

type WalletRepository struct {
	db *sql.DB
}

func NewWalletRepository(db *sql.DB) *WalletRepository {
	return &WalletRepository{db: db}
}

// Create persists a new wallet using the caller's database transaction.
func (r *WalletRepository) Create(
	ctx context.Context,
	tx *sql.Tx,
	w wallet.Wallet,
) error {
	if tx == nil {
		return errors.New("create wallet: nil database transaction")
	}

	const query = `
		INSERT INTO wallets (
			id, player_id, currency, balance_minor_units, version, created_at, updated_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`

	_, err := tx.ExecContext(
		ctx,
		query,
		w.ID(),
		w.PlayerID(),
		w.Balance().Currency(),
		w.Balance().AmountInMinorUnits(),
		w.Version(),
		w.CreatedAt(),
		w.UpdatedAt(),
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ErrWalletConflict
		}
		return fmt.Errorf("create wallet: %w", err)
	}

	return nil
}

// GetByPlayerAndCurrency finds a wallet by its natural key.
func (r *WalletRepository) GetByPlayerAndCurrency(
	ctx context.Context,
	playerID string,
	currency string,
) (wallet.Wallet, error) {
	var id string
	err := r.db.QueryRowContext(
		ctx,
		`SELECT id FROM wallets WHERE player_id = $1 AND currency = $2`,
		playerID,
		currency,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return wallet.Wallet{}, ErrWalletNotFound
	}
	if err != nil {
		return wallet.Wallet{}, fmt.Errorf("get wallet by player and currency: %w", err)
	}

	return r.GetByID(ctx, id)
}

func (r *WalletRepository) GetByID(
	ctx context.Context,
	id string,
) (wallet.Wallet, error) {
	const query = `
		SELECT
			id,
			player_id,
			currency,
			balance_minor_units,
			version,
			created_at,
			updated_at
		FROM wallets
		WHERE id = $1
	`

	var (
		walletID  string
		playerID  string
		currency  string
		balance   int64
		version   int64
		createdAt sql.NullTime
		updatedAt sql.NullTime
	)

	err := r.db.QueryRowContext(ctx, query, id).Scan(
		&walletID,
		&playerID,
		&currency,
		&balance,
		&version,
		&createdAt,
		&updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return wallet.Wallet{}, ErrWalletNotFound
	}
	if err != nil {
		return wallet.Wallet{}, fmt.Errorf("get wallet by ID: %w", err)
	}

	if !createdAt.Valid || !updatedAt.Valid {
		return wallet.Wallet{}, fmt.Errorf(
			"get wallet by ID: missing timestamps",
		)
	}

	if balance < 0 {
		return wallet.Wallet{}, fmt.Errorf(
			"get wallet by ID: negative persisted balance",
		)
	}

	amount := fmt.Sprintf("%d.%02d", balance/100, balance%100)

	moneyBalance, err := money.New(amount, currency)
	if err != nil {
		return wallet.Wallet{}, fmt.Errorf(
			"reconstruct wallet balance: %w",
			err,
		)
	}

	result, err := wallet.Rehydrate(
		walletID,
		playerID,
		moneyBalance,
		version,
		createdAt.Time,
		updatedAt.Time,
	)
	if err != nil {
		return wallet.Wallet{}, fmt.Errorf(
			"reconstruct wallet: %w",
			err,
		)
	}

	return result, nil
}
