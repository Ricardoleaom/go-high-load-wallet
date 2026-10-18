package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/wagering"
)

// claimLease is how long a claimed reversal stays out of other workers'
// reach. A worker that dies holding a claim only delays that reversal by
// this much.
const claimLease = 60 * time.Second

// ReferenceQueue hands out due PENDING_REFERENCE reversals.
type ReferenceQueue struct {
	pool *pgxpool.Pool
}

func NewReferenceQueue(pool *pgxpool.Pool) *ReferenceQueue {
	return &ReferenceQueue{pool: pool}
}

// ClaimDue claims up to limit due reversals in one statement: the rows are
// picked with FOR UPDATE SKIP LOCKED and pushed a lease into the future, so
// workers polling together never claim the same reversal.
func (q *ReferenceQueue) ClaimDue(ctx context.Context, limit int) ([]wagering.ReferenceClaim, error) {
	rows, err := q.pool.Query(ctx, `
		UPDATE transactions
		SET reference_next_attempt_at = now() + $2::interval
		WHERE id IN (
			SELECT id
			FROM transactions
			WHERE status = 'PENDING_REFERENCE'
			  AND (reference_next_attempt_at IS NULL OR reference_next_attempt_at <= now())
			ORDER BY reference_next_attempt_at NULLS FIRST
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id::text, reference_attempts`, limit, claimLease)
	if err != nil {
		return nil, fmt.Errorf("claim pending references: %w", err)
	}
	claims, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (wagering.ReferenceClaim, error) {
		var c wagering.ReferenceClaim
		err := row.Scan(&c.TransactionID, &c.Attempts)
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("claim pending references: %w", err)
	}
	return claims, nil
}

var _ wagering.ReferenceQueue = (*ReferenceQueue)(nil)
