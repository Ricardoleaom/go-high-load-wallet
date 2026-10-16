package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/money"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/wager"
)

var (
	ErrTransactionNotFound = errors.New("transaction not found")
	ErrTransactionConflict = errors.New("transaction uniqueness conflict")
)

type TransactionRepository struct {
	db *sql.DB
}

func NewTransactionRepository(db *sql.DB) *TransactionRepository {
	return &TransactionRepository{db: db}
}

// Create persists a transaction using the caller's database transaction.
func (r *TransactionRepository) Create(
	ctx context.Context,
	tx *sql.Tx,
	transaction wager.Transaction,
) error {
	if tx == nil {
		return errors.New("create transaction: nil database transaction")
	}

	const query = `
		INSERT INTO transactions (
			id,
			provider_id,
			external_id,
			idempotency_key,
			payload_hash,
			wallet_id,
			player_id,
			round_id,
			game_id,
			kind,
			amount_minor_units,
			reference_external_id,
			reference_id,
			status,
			failure_code,
			created_at,
			updated_at
		)
		VALUES (
			$1, NULLIF($2, ''), NULLIF($3, ''), NULLIF($4, ''), NULLIF($5, ''),
			$6, $7, NULLIF($8, ''), NULLIF($9, ''),
			$10, $11, NULLIF($12, ''), NULLIF($13, ''), $14,
			NULLIF($15, ''), $16, $17
		)
	`

	_, err := tx.ExecContext(
		ctx,
		query,
		transaction.ID(),
		transaction.ProviderID(),
		transaction.ExternalID(),
		transaction.IdempotencyKey(),
		transaction.PayloadHash(),
		transaction.WalletID(),
		transaction.PlayerID(),
		transaction.RoundID(),
		transaction.GameID(),
		string(transaction.Kind()),
		transaction.Amount().AmountInMinorUnits(),
		transaction.ReferenceExternalID(),
		transaction.ResolvedReferenceID(),
		string(transaction.Status()),
		transaction.FailureCode(),
		transaction.CreatedAt(),
		transaction.UpdatedAt(),
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return fmt.Errorf("%w: %s", ErrTransactionConflict, pgErr.ConstraintName)
		}

		return fmt.Errorf("create transaction: %w", err)
	}

	return nil
}

// GetByID retrieves a transaction and reconstructs its domain representation.
func (r *TransactionRepository) GetByID(
	ctx context.Context,
	id string,
) (wager.Transaction, error) {
	const query = `
		SELECT
			t.id,
			t.provider_id,
			t.external_id,
			t.idempotency_key,
			t.payload_hash,
			t.wallet_id,
			t.player_id,
			t.round_id,
			t.game_id,
			t.kind,
			t.amount_minor_units,
			w.currency,
			t.reference_external_id,
			t.reference_id,
			t.status,
			t.failure_code,
			t.created_at,
			t.updated_at
		FROM transactions t
		JOIN wallets w ON w.id = t.wallet_id
		WHERE t.id = $1
	`

	var (
		input               wager.Input
		kind                string
		status              string
		amountUnits         int64
		currency            string
		referenceExternalID sql.NullString
		resolvedReferenceID sql.NullString
		failureCode         sql.NullString
		providerID          sql.NullString
		externalID          sql.NullString
		idempotencyKey      sql.NullString
		payloadHash         sql.NullString
		roundID             sql.NullString
		gameID              sql.NullString
		createdAt           sql.NullTime
		updatedAt           sql.NullTime
	)

	err := r.db.QueryRowContext(ctx, query, id).Scan(
		&input.ID,
		&providerID,
		&externalID,
		&idempotencyKey,
		&payloadHash,
		&input.WalletID,
		&input.PlayerID,
		&roundID,
		&gameID,
		&kind,
		&amountUnits,
		&currency,
		&referenceExternalID,
		&resolvedReferenceID,
		&status,
		&failureCode,
		&createdAt,
		&updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return wager.Transaction{}, ErrTransactionNotFound
	}
	if err != nil {
		return wager.Transaction{}, fmt.Errorf("get transaction by ID: %w", err)
	}

	if amountUnits < 0 || !createdAt.Valid || !updatedAt.Valid {
		return wager.Transaction{}, errors.New("invalid persisted transaction data")
	}

	amountText := fmt.Sprintf("%d.%02d", amountUnits/100, amountUnits%100)
	input.Amount, err = money.New(amountText, currency)
	if err != nil {
		return wager.Transaction{}, fmt.Errorf("reconstruct transaction amount: %w", err)
	}

	input.Kind = wager.Kind(kind)
	input.ProviderID = providerID.String
	input.ExternalID = externalID.String
	input.IdempotencyKey = idempotencyKey.String
	input.PayloadHash = payloadHash.String
	input.RoundID = roundID.String
	input.GameID = gameID.String
	input.ReferenceExternalID = referenceExternalID.String

	transaction, err := wager.Rehydrate(
		input,
		wager.Status(status),
		failureCode.String,
		resolvedReferenceID.String,
		createdAt.Time,
		updatedAt.Time,
	)
	if err != nil {
		return wager.Transaction{}, fmt.Errorf("reconstruct transaction: %w", err)
	}

	return transaction, nil
}

// GetByIdempotencyKey retrieves a transaction using its provider and key.
func (r *TransactionRepository) GetByIdempotencyKey(
	ctx context.Context,
	providerID string,
	idempotencyKey string,
) (wager.Transaction, error) {
	const query = `
		SELECT id
		FROM transactions
		WHERE provider_id = $1 AND idempotency_key = $2
	`

	var id string
	err := r.db.QueryRowContext(
		ctx, query, providerID, idempotencyKey,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return wager.Transaction{}, ErrTransactionNotFound
	}
	if err != nil {
		return wager.Transaction{}, fmt.Errorf("find transaction by idempotency key: %w", err)
	}

	return r.GetByID(ctx, id)
}

// GetByExternalID retrieves a transaction using its provider and external ID.
func (r *TransactionRepository) GetByExternalID(
	ctx context.Context,
	providerID string,
	externalID string,
) (wager.Transaction, error) {
	const query = `
		SELECT id
		FROM transactions
		WHERE provider_id = $1 AND external_id = $2
	`

	var id string
	err := r.db.QueryRowContext(
		ctx, query, providerID, externalID,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return wager.Transaction{}, ErrTransactionNotFound
	}
	if err != nil {
		return wager.Transaction{}, fmt.Errorf("find transaction by external ID: %w", err)
	}

	return r.GetByID(ctx, id)
}

// UpdateStatus persists the current status of a transaction.
func (r *TransactionRepository) UpdateStatus(
	ctx context.Context,
	tx *sql.Tx,
	transaction wager.Transaction,
) error {
	if tx == nil {
		return errors.New("update transaction status: nil database transaction")
	}

	const query = `
        UPDATE transactions
        SET status = $1,
            failure_code = NULLIF($2, ''),
            reference_id = NULLIF($3, ''),
            updated_at = $4
        WHERE id = $5
    `

	result, err := tx.ExecContext(
		ctx,
		query,
		string(transaction.Status()),
		transaction.FailureCode(),
		transaction.ResolvedReferenceID(),
		transaction.UpdatedAt(),
		transaction.ID(),
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return fmt.Errorf("%w: %s", ErrTransactionConflict, pgErr.ConstraintName)
		}
		return fmt.Errorf("update transaction status: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check transaction status update: %w", err)
	}

	if rowsAffected == 0 {
		return ErrTransactionNotFound
	}

	return nil
}

// LockByID reads a transaction and locks its row for the caller's database
// transaction, serializing concurrent reversal attempts against the same
// referenced transaction.
func (r *TransactionRepository) LockByID(
	ctx context.Context,
	tx *sql.Tx,
	id string,
) (wager.Transaction, error) {
	var locked string
	err := tx.QueryRowContext(
		ctx,
		`SELECT id FROM transactions WHERE id = $1 FOR UPDATE`,
		id,
	).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return wager.Transaction{}, ErrTransactionNotFound
	}
	if err != nil {
		return wager.Transaction{}, fmt.Errorf("lock transaction: %w", err)
	}

	return r.GetByID(ctx, id)
}

// HasSuccessfulReversal reports whether the referenced transaction already
// has a PROCESSED REFUND or ROLLBACK against it. Call after LockByID so the
// check observes a consistent, serialized view.
func (r *TransactionRepository) HasSuccessfulReversal(
	ctx context.Context,
	tx *sql.Tx,
	providerID string,
	referencedInternalID string,
) (bool, error) {
	var exists bool
	err := tx.QueryRowContext(
		ctx,
		`SELECT EXISTS (
			SELECT 1 FROM transactions
			WHERE provider_id = $1
			  AND reference_id = $2
			  AND status = 'PROCESSED'
			  AND kind IN ('REFUND', 'ROLLBACK')
		)`,
		providerID,
		referencedInternalID,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check existing reversal: %w", err)
	}

	return exists, nil
}

// ScheduleReferenceRetry bumps the retry counter for a PENDING_REFERENCE
// transaction whose reference could still not be resolved.
func (r *TransactionRepository) ScheduleReferenceRetry(
	ctx context.Context,
	tx *sql.Tx,
	transactionID string,
	attempts int,
	nextAttemptAt time.Time,
) error {
	if tx == nil {
		return errors.New("schedule reference retry: nil database transaction")
	}

	const query = `
		UPDATE transactions
		SET reference_attempts = $1,
		    reference_next_attempt_at = $2,
		    updated_at = NOW()
		WHERE id = $3
	`

	_, err := tx.ExecContext(ctx, query, attempts, nextAttemptAt, transactionID)
	if err != nil {
		return fmt.Errorf("schedule reference retry: %w", err)
	}

	return nil
}

type DuePendingReference struct {
	Transaction wager.Transaction
	Attempts    int
}

// claimLease is how long ListDuePendingReferences reserves a row for the
// caller that claimed it. If that caller crashes before processing it (and
// so never calls ScheduleReferenceRetry or UpdateStatus again), the row
// becomes eligible again once the lease expires; nothing stays stuck.
const claimLease = 60 * time.Second

// ListDuePendingReferences atomically claims up to `limit` PENDING_REFERENCE
// transactions whose next retry attempt is due. The claim uses
// SELECT ... FOR UPDATE SKIP LOCKED inside the UPDATE, so two instances
// polling at the same time never claim the same row: this is required for
// correctness, since processing a row twice concurrently could race on its
// own status transition (one instance's success getting overwritten by the
// other's rejection) even though the financial movement itself stays safe
// behind the transactions table's unique indexes.
func (r *TransactionRepository) ListDuePendingReferences(
	ctx context.Context,
	limit int,
) ([]DuePendingReference, error) {
	const query = `
		UPDATE transactions
		SET reference_next_attempt_at = NOW() + ($2 * INTERVAL '1 second')
		WHERE id IN (
			SELECT id
			FROM transactions
			WHERE status = 'PENDING_REFERENCE'
			  AND (reference_next_attempt_at IS NULL OR reference_next_attempt_at <= NOW())
			ORDER BY reference_next_attempt_at NULLS FIRST
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, reference_attempts
	`

	rows, err := r.db.QueryContext(ctx, query, limit, claimLease.Seconds())
	if err != nil {
		return nil, fmt.Errorf("list due pending references: %w", err)
	}
	defer rows.Close()

	var due []DuePendingReference
	for rows.Next() {
		var id string
		var attempts int
		if err := rows.Scan(&id, &attempts); err != nil {
			return nil, fmt.Errorf("scan pending reference: %w", err)
		}

		transaction, err := r.GetByID(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("load pending reference transaction: %w", err)
		}

		due = append(due, DuePendingReference{Transaction: transaction, Attempts: attempts})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending references: %w", err)
	}

	return due, nil
}
