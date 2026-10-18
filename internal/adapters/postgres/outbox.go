package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/relay"
)

// ClaimDueEventsQuery claims the oldest due events, skipping those another
// relay holds. Exported for the plan test.
const ClaimDueEventsQuery = `
	SELECT id::text, aggregate_id, event_type, payload::text, correlation_id,
	       COALESCE(causation_id, ''), occurred_at, attempts
	FROM outbox_events
	WHERE published_at IS NULL AND next_attempt_at <= now()
	ORDER BY occurred_at
	LIMIT $1
	FOR UPDATE SKIP LOCKED`

// OutboxStore is the outbox table as a relay.Store.
type OutboxStore struct {
	pool *pgxpool.Pool
}

func NewOutboxStore(pool *pgxpool.Pool) *OutboxStore {
	return &OutboxStore{pool: pool}
}

func (s *OutboxStore) Backlog(ctx context.Context) (int64, time.Duration, error) {
	var pending int64
	var oldestSeconds float64
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(EXTRACT(EPOCH FROM now() - min(occurred_at)), 0)::float8
		FROM outbox_events
		WHERE published_at IS NULL`,
	).Scan(&pending, &oldestSeconds); err != nil {
		return 0, 0, fmt.Errorf("measure outbox backlog: %w", err)
	}
	return pending, time.Duration(oldestSeconds * float64(time.Second)), nil
}

// PublishDue claims due events with FOR UPDATE SKIP LOCKED, so several
// relays run side by side without sending an event twice or waiting on
// each other. A relay that dies mid-batch releases its claim with its
// connection, and the next poll picks the batch up.
func (s *OutboxStore) PublishDue(
	ctx context.Context,
	limit int,
	backoff func(attempts int) time.Duration,
	publish func(context.Context, relay.Event) error,
) (published, failed int, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("begin outbox publish: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The batch waits on the broker between statements, which PostgreSQL
	// sees as idle in transaction: lift the session's limit for this
	// transaction only, so a slow broker cannot end it halfway and undo the
	// bookkeeping of events already sent.
	if _, err := tx.Exec(ctx, `SET LOCAL idle_in_transaction_session_timeout = 0`); err != nil {
		return 0, 0, fmt.Errorf("lift idle limit for outbox publish: %w", err)
	}

	type claimed struct {
		event    relay.Event
		attempts int
	}
	rows, err := tx.Query(ctx, ClaimDueEventsQuery, limit)
	if err != nil {
		return 0, 0, fmt.Errorf("claim outbox events: %w", err)
	}
	batch, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (claimed, error) {
		var c claimed
		var payload string
		err := row.Scan(&c.event.ID, &c.event.AggregateID, &c.event.EventType, &payload,
			&c.event.CorrelationID, &c.event.CausationID, &c.event.OccurredAt, &c.attempts)
		c.event.Payload = []byte(payload)
		return c, err
	})
	if err != nil {
		return 0, 0, fmt.Errorf("claim outbox events: %w", err)
	}

	for _, c := range batch {
		if sendErr := publish(ctx, c.event); sendErr != nil {
			attempts := c.attempts + 1
			if _, err := tx.Exec(ctx, `UPDATE outbox_events SET attempts = $1, next_attempt_at = $2 WHERE id = $3`,
				attempts, time.Now().Add(backoff(attempts)), c.event.ID); err != nil {
				return published, failed, fmt.Errorf("record outbox publish failure: %w", err)
			}
			failed++
			continue
		}
		if _, err := tx.Exec(ctx, `UPDATE outbox_events SET published_at = now() WHERE id = $1`, c.event.ID); err != nil {
			return published, failed, fmt.Errorf("mark outbox event published: %w", err)
		}
		published++
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, 0, fmt.Errorf("commit outbox publish: %w", err)
	}
	return published, failed, nil
}

var _ relay.Store = (*OutboxStore)(nil)
