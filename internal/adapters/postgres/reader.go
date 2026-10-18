package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/queries"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/ledger"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/wager"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/wallet"
)

// Reader serves queries.Reader with one statement per call.
type Reader struct {
	pool *pgxpool.Pool
}

func NewReader(pool *pgxpool.Pool) *Reader {
	return &Reader{pool: pool}
}

func (r *Reader) Wallet(ctx context.Context, id string) (wallet.Wallet, error) {
	w, err := r.wallet(ctx, id)
	return w, markTimeout(err)
}

func (r *Reader) wallet(ctx context.Context, id string) (wallet.Wallet, error) {
	var (
		playerID, currency   string
		balance, version     int64
		createdAt, updatedAt time.Time
	)
	err := r.pool.QueryRow(ctx, `
		SELECT player_id::text, currency, balance_minor_units, version, created_at, updated_at
		FROM wallets WHERE id = $1`, id,
	).Scan(&playerID, &currency, &balance, &version, &createdAt, &updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return wallet.Wallet{}, queries.ErrWalletNotFound
	}
	if err != nil {
		return wallet.Wallet{}, fmt.Errorf("read wallet: %w", err)
	}
	m, err := fromMinorUnits(balance, currency)
	if err != nil {
		return wallet.Wallet{}, fmt.Errorf("read wallet: %w", err)
	}
	return wallet.Rehydrate(id, playerID, m, version, createdAt, updatedAt)
}

// transactionQuery reads a transaction with the balance it left: its own
// ledger entry's for a movement, the wallet's current one for any other
// PROCESSED transaction.
const transactionQuery = `
	SELECT t.id::text, t.provider_id, t.external_id, t.idempotency_key, t.payload_hash,
	       t.wallet_id::text, t.player_id::text, t.round_id, t.game_id, t.kind,
	       t.amount_minor_units, t.currency, t.reference_external_id, t.reference_id,
	       t.status, t.failure_code, t.created_at, t.updated_at,
	       CASE WHEN t.status = 'PROCESSED' THEN COALESCE(le.balance_after_minor_units, w.balance_minor_units) END
	FROM transactions t
	LEFT JOIN ledger_entries le ON le.transaction_id = t.id
	LEFT JOIN wallets w ON w.id = t.wallet_id AND le.id IS NULL AND t.status = 'PROCESSED'
`

func (r *Reader) Transaction(ctx context.Context, id string) (queries.TransactionView, error) {
	view, err := scanTransaction(r.pool.QueryRow(ctx, transactionQuery+`WHERE t.id = $1`, id))
	return view, markTimeout(err)
}

func (r *Reader) TransactionByExternalID(ctx context.Context, providerID, externalID string) (queries.TransactionView, error) {
	view, err := scanTransaction(r.pool.QueryRow(ctx,
		transactionQuery+`WHERE t.provider_id = $1 AND t.external_id = $2 AND t.kind <> 'OPENING'`,
		providerID, externalID))
	return view, markTimeout(err)
}

func scanTransaction(row pgx.Row) (queries.TransactionView, error) {
	var (
		in                                         wager.Input
		kind, status, currency                     string
		amount                                     int64
		provider, external, key, hash, round, game *string
		reference, referenceID, failureCode        *string
		createdAt, updatedAt                       time.Time
		balance                                    *int64
	)
	err := row.Scan(&in.ID, &provider, &external, &key, &hash, &in.WalletID, &in.PlayerID, &round, &game,
		&kind, &amount, &currency, &reference, &referenceID, &status, &failureCode, &createdAt, &updatedAt, &balance)
	if errors.Is(err, pgx.ErrNoRows) {
		return queries.TransactionView{}, queries.ErrTransactionNotFound
	}
	if err != nil {
		return queries.TransactionView{}, fmt.Errorf("read transaction: %w", err)
	}

	in.ProviderID, in.ExternalID, in.IdempotencyKey, in.PayloadHash = deref(provider), deref(external), deref(key), deref(hash)
	in.RoundID, in.GameID, in.ReferenceExternalID, in.Kind = deref(round), deref(game), deref(reference), wager.Kind(kind)
	if in.Amount, err = fromMinorUnits(amount, currency); err != nil {
		return queries.TransactionView{}, fmt.Errorf("read transaction %s: %w", in.ID, err)
	}
	tx, err := wager.Rehydrate(in, wager.Status(status), deref(failureCode), deref(referenceID), createdAt, updatedAt)
	if err != nil {
		return queries.TransactionView{}, fmt.Errorf("read transaction %s: %w", in.ID, err)
	}

	view := queries.TransactionView{Transaction: tx}
	if balance != nil {
		if view.Balance, err = fromMinorUnits(*balance, currency); err != nil {
			return queries.TransactionView{}, fmt.Errorf("read transaction %s: %w", in.ID, err)
		}
	}
	return view, nil
}

func (r *Reader) Ledger(ctx context.Context, walletID string, after queries.Cursor, limit int) ([]ledger.Entry, error) {
	entries, err := r.ledger(ctx, walletID, after, limit)
	return entries, markTimeout(err)
}

func (r *Reader) ledger(ctx context.Context, walletID string, after queries.Cursor, limit int) ([]ledger.Entry, error) {
	var afterID *string
	if after.ID != "" {
		afterID = &after.ID
	}
	rows, err := r.pool.Query(ctx, `
		SELECT le.id::text, le.transaction_id::text, le.direction, le.amount_delta_minor_units,
		       le.balance_before_minor_units, le.balance_after_minor_units, le.created_at, w.currency
		FROM ledger_entries le
		JOIN wallets w ON w.id = le.wallet_id
		WHERE le.wallet_id = $1
		  AND ($2::uuid IS NULL OR (le.created_at, le.id) > ($3, $2::uuid))
		ORDER BY le.created_at, le.id
		LIMIT $4`, walletID, afterID, after.CreatedAt, limit)
	if err != nil {
		return nil, fmt.Errorf("read ledger: %w", err)
	}
	defer rows.Close()

	var entries []ledger.Entry
	for rows.Next() {
		var (
			id, transactionID, direction, currency string
			delta, before, afterBalance            int64
			createdAt                              time.Time
		)
		if err := rows.Scan(&id, &transactionID, &direction, &delta, &before, &afterBalance, &createdAt, &currency); err != nil {
			return nil, fmt.Errorf("read ledger: %w", err)
		}
		entry, err := ledgerEntry(id, walletID, transactionID, direction, delta, before, afterBalance, currency, createdAt)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read ledger: %w", err)
	}
	return entries, nil
}

func ledgerEntry(id, walletID, transactionID, direction string, delta, before, after int64, currency string,
	createdAt time.Time) (ledger.Entry, error) {
	amount, err := fromMinorUnits(max(delta, -delta), currency)
	if err != nil {
		return ledger.Entry{}, fmt.Errorf("read ledger entry %s: %w", id, err)
	}
	beforeMoney, err := fromMinorUnits(before, currency)
	if err != nil {
		return ledger.Entry{}, fmt.Errorf("read ledger entry %s: %w", id, err)
	}
	afterMoney, err := fromMinorUnits(after, currency)
	if err != nil {
		return ledger.Entry{}, fmt.Errorf("read ledger entry %s: %w", id, err)
	}
	return ledger.New(id, walletID, transactionID, ledger.Direction(direction), amount, beforeMoney, afterMoney, createdAt)
}

// Reconciliation reads the stored balance and the ledger total in one
// statement, hence one snapshot: both sides see the same committed data
// while other transactions keep moving the wallet.
func (r *Reader) Reconciliation(ctx context.Context, walletID string) (queries.Reconciliation, error) {
	snapshot, err := r.reconciliation(ctx, walletID)
	return snapshot, markTimeout(err)
}

func (r *Reader) reconciliation(ctx context.Context, walletID string) (queries.Reconciliation, error) {
	var (
		stored, calculated int64
		currency           string
		entries            int
	)
	err := r.pool.QueryRow(ctx, `
		SELECT w.balance_minor_units, w.currency,
		       COALESCE(sum(le.amount_delta_minor_units), 0)::bigint, count(le.id)
		FROM wallets w
		LEFT JOIN ledger_entries le ON le.wallet_id = w.id
		WHERE w.id = $1
		GROUP BY w.id`, walletID,
	).Scan(&stored, &currency, &calculated, &entries)
	if errors.Is(err, pgx.ErrNoRows) {
		return queries.Reconciliation{}, queries.ErrWalletNotFound
	}
	if err != nil {
		return queries.Reconciliation{}, fmt.Errorf("read reconciliation: %w", err)
	}

	storedMoney, err := fromMinorUnits(stored, currency)
	if err != nil {
		return queries.Reconciliation{}, err
	}
	// A corrupted ledger can sum below zero; report it rather than fail.
	calculatedMoney, err := fromMinorUnits(max(calculated, -calculated), currency)
	if err == nil && calculated < 0 {
		calculatedMoney, err = calculatedMoney.Negate()
	}
	if err != nil {
		return queries.Reconciliation{}, err
	}
	return queries.Reconciliation{StoredBalance: storedMoney, CalculatedBalance: calculatedMoney, CheckedEntries: entries}, nil
}

var _ queries.Reader = (*Reader)(nil)
