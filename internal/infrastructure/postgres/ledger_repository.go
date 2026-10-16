package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/ledger"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/money"
)

var ErrLedgerEntryNotFound = errors.New("ledger entry not found")

type LedgerRepository struct {
	db *sql.DB
}

func NewLedgerRepository(db *sql.DB) *LedgerRepository {
	return &LedgerRepository{db: db}
}

// Append inserts a ledger entry using the caller's database transaction.
// The amount_delta column keeps a signed value (negative for debit) to make
// reconciliation sums (credits - debits) a plain SUM() query.
func (r *LedgerRepository) Append(
	ctx context.Context,
	tx *sql.Tx,
	entry ledger.Entry,
) error {
	if tx == nil {
		return errors.New("append ledger entry: nil database transaction")
	}

	delta := entry.Amount().AmountInMinorUnits()
	if entry.Direction() == ledger.DirectionDebit {
		delta = -delta
	}

	const query = `
		INSERT INTO ledger_entries (
			id,
			wallet_id,
			transaction_id,
			direction,
			amount_delta_minor_units,
			balance_before_minor_units,
			balance_after_minor_units
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`

	_, err := tx.ExecContext(
		ctx,
		query,
		entry.ID(),
		entry.WalletID(),
		entry.TransactionID(),
		string(entry.Direction()),
		delta,
		entry.BalanceBefore().AmountInMinorUnits(),
		entry.BalanceAfter().AmountInMinorUnits(),
	)
	if err != nil {
		return fmt.Errorf("append ledger entry: %w", err)
	}

	return nil
}

// GetByTransactionID retrieves the ledger entry for a transaction.
func (r *LedgerRepository) GetByTransactionID(
	ctx context.Context,
	transactionID string,
) (ledger.Entry, error) {
	const query = `
		SELECT
			le.id,
			le.wallet_id,
			le.transaction_id,
			le.direction,
			le.balance_before_minor_units,
			le.balance_after_minor_units,
			le.created_at,
			w.currency
		FROM ledger_entries le
		JOIN wallets w ON w.id = le.wallet_id
		WHERE le.transaction_id = $1
	`

	return r.scanEntry(r.db.QueryRowContext(ctx, query, transactionID))
}

// ListByWallet returns ledger entries for a wallet, oldest first, for
// cursor-based pagination and reconciliation. When afterID is non-empty, it
// resumes strictly after that entry's (createdAt, id) position.
func (r *LedgerRepository) ListByWallet(
	ctx context.Context,
	walletID string,
	afterCreatedAt time.Time,
	afterID string,
	limit int,
) ([]ledger.Entry, error) {
	const query = `
		SELECT
			le.id,
			le.wallet_id,
			le.transaction_id,
			le.direction,
			le.balance_before_minor_units,
			le.balance_after_minor_units,
			le.created_at,
			w.currency
		FROM ledger_entries le
		JOIN wallets w ON w.id = le.wallet_id
		WHERE le.wallet_id = $1
		  AND (
		    $2::uuid IS NULL
		    OR le.created_at > $3
		    OR (le.created_at = $3 AND le.id > $2::uuid)
		  )
		ORDER BY le.created_at, le.id
		LIMIT $4
	`

	var afterIDParam any
	if afterID != "" {
		afterIDParam = afterID
	}

	rows, err := r.db.QueryContext(ctx, query, walletID, afterIDParam, afterCreatedAt, limit)
	if err != nil {
		return nil, fmt.Errorf("list ledger entries: %w", err)
	}
	defer rows.Close()

	var entries []ledger.Entry
	for rows.Next() {
		entry, err := r.scanEntryRow(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate ledger entries: %w", err)
	}

	return entries, nil
}

type ReconciliationSnapshot struct {
	StoredBalance     money.Money
	CalculatedBalance money.Money
	CheckedEntries    int
}

// ReconciliationSnapshot reads the stored balance and the ledger totals in a
// single statement. Postgres gives one statement one snapshot, so both sides
// of the comparison see the same committed data even while other
// transactions keep moving the wallet.
func (r *LedgerRepository) ReconciliationSnapshot(ctx context.Context, walletID string) (ReconciliationSnapshot, error) {
	const query = `
		SELECT
			w.balance_minor_units,
			w.currency,
			COALESCE(SUM(le.amount_delta_minor_units), 0)::BIGINT,
			COUNT(le.id)
		FROM wallets w
		LEFT JOIN ledger_entries le ON le.wallet_id = w.id
		WHERE w.id = $1
		GROUP BY w.id
	`

	var (
		stored     int64
		currency   string
		calculated int64
		count      int
	)
	err := r.db.QueryRowContext(ctx, query, walletID).Scan(&stored, &currency, &calculated, &count)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ReconciliationSnapshot{}, ErrWalletNotFound
		}
		return ReconciliationSnapshot{}, fmt.Errorf("read reconciliation snapshot: %w", err)
	}

	storedMoney, err := money.New(formatMinorUnits(stored), currency)
	if err != nil {
		return ReconciliationSnapshot{}, fmt.Errorf("reconstruct stored balance: %w", err)
	}
	calculatedMoney, err := signedMoney(calculated, currency)
	if err != nil {
		return ReconciliationSnapshot{}, fmt.Errorf("reconstruct calculated balance: %w", err)
	}

	return ReconciliationSnapshot{
		StoredBalance:     storedMoney,
		CalculatedBalance: calculatedMoney,
		CheckedEntries:    count,
	}, nil
}

// signedMoney allows a negative result: a corrupted ledger could sum below
// zero, and reconciliation must report that instead of failing to build it.
func signedMoney(units int64, currency string) (money.Money, error) {
	if units >= 0 {
		return money.New(formatMinorUnits(units), currency)
	}
	positive, err := money.New(formatMinorUnits(-units), currency)
	if err != nil {
		return money.Money{}, err
	}
	return positive.Negate()
}

type scannable interface {
	Scan(dest ...any) error
}

func (r *LedgerRepository) scanEntry(row scannable) (ledger.Entry, error) {
	entry, err := r.scanEntryRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ledger.Entry{}, ErrLedgerEntryNotFound
	}
	return entry, err
}

func (r *LedgerRepository) scanEntryRow(row scannable) (ledger.Entry, error) {
	var (
		id            string
		walletID      string
		transactionID string
		direction     string
		balanceBefore int64
		balanceAfter  int64
		createdAt     sql.NullTime
		currency      string
	)

	err := row.Scan(
		&id,
		&walletID,
		&transactionID,
		&direction,
		&balanceBefore,
		&balanceAfter,
		&createdAt,
		&currency,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ledger.Entry{}, err
		}
		return ledger.Entry{}, fmt.Errorf("get ledger entry: %w", err)
	}

	before, err := money.New(formatMinorUnits(balanceBefore), currency)
	if err != nil {
		return ledger.Entry{}, fmt.Errorf("reconstruct ledger balance before: %w", err)
	}
	after, err := money.New(formatMinorUnits(balanceAfter), currency)
	if err != nil {
		return ledger.Entry{}, fmt.Errorf("reconstruct ledger balance after: %w", err)
	}

	var amount money.Money
	switch ledger.Direction(direction) {
	case ledger.DirectionDebit:
		amount, err = before.Subtract(after)
	case ledger.DirectionCredit:
		amount, err = after.Subtract(before)
	default:
		return ledger.Entry{}, fmt.Errorf("unknown ledger direction: %s", direction)
	}
	if err != nil {
		return ledger.Entry{}, fmt.Errorf("reconstruct ledger amount: %w", err)
	}

	return ledger.New(
		id, walletID, transactionID,
		ledger.Direction(direction),
		amount, before, after,
		createdAt.Time,
	)
}

func formatMinorUnits(units int64) string {
	return fmt.Sprintf("%d.%02d", units/100, units%100)
}
