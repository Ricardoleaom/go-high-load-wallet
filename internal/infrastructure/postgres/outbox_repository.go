package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type OutboxEvent struct {
	ID            string
	AggregateID   string
	EventType     string
	Payload       []byte
	CorrelationID string
	CausationID   string
	OccurredAt    time.Time
}

type OutboxRepository struct {
	db *sql.DB
}

func NewOutboxRepository(db *sql.DB) *OutboxRepository {
	return &OutboxRepository{db: db}
}

// Append inserts an outbox event using the caller's database transaction, so
// it commits atomically with the domain change that caused it.
func (r *OutboxRepository) Append(
	ctx context.Context,
	tx *sql.Tx,
	event OutboxEvent,
) error {
	if tx == nil {
		return errors.New("append outbox event: nil database transaction")
	}

	const query = `
		INSERT INTO outbox_events (
			id, aggregate_id, event_type, payload, correlation_id, causation_id, occurred_at
		)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), $7)
	`

	_, err := tx.ExecContext(
		ctx,
		query,
		event.ID,
		event.AggregateID,
		event.EventType,
		event.Payload,
		event.CorrelationID,
		event.CausationID,
		event.OccurredAt,
	)
	if err != nil {
		return fmt.Errorf("append outbox event: %w", err)
	}

	return nil
}

// Backlog reports how many events wait for publication and how old the
// oldest of them is; both are zero when nothing is pending.
func (r *OutboxRepository) Backlog(ctx context.Context) (pending int64, oldest time.Duration, err error) {
	var oldestSeconds float64
	err = r.db.QueryRowContext(ctx, `
		SELECT count(*), COALESCE(EXTRACT(EPOCH FROM now() - min(occurred_at)), 0)::float8
		FROM outbox_events
		WHERE published_at IS NULL
	`).Scan(&pending, &oldestSeconds)
	if err != nil {
		return 0, 0, fmt.Errorf("measure outbox backlog: %w", err)
	}
	return pending, time.Duration(oldestSeconds * float64(time.Second)), nil
}

// PublishDue claims up to `limit` due events with SELECT ... FOR UPDATE SKIP
// LOCKED, so multiple publisher instances can run concurrently without
// double-publishing or blocking on each other. A crash mid-batch releases
// the lock when the connection drops, so an abandoned batch is picked up by
// the next publisher to poll.
func (r *OutboxRepository) PublishDue(
	ctx context.Context,
	limit int,
	backoff func(attempts int) time.Duration,
	publish func(context.Context, OutboxEvent) error,
) (published int, err error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin outbox publish: %w", err)
	}
	defer tx.Rollback()

	// The batch waits on the broker between statements, which PostgreSQL
	// sees as idle in transaction: lift the session's limit for this
	// transaction only, so a slow broker cannot end it halfway and undo the
	// bookkeeping of events already sent.
	if _, err := tx.ExecContext(ctx, `SET LOCAL idle_in_transaction_session_timeout = 0`); err != nil {
		return 0, fmt.Errorf("lift idle limit for outbox publish: %w", err)
	}

	const selectQuery = `
		SELECT id, aggregate_id, event_type, payload, correlation_id,
		       COALESCE(causation_id, ''), occurred_at, attempts
		FROM outbox_events
		WHERE published_at IS NULL AND next_attempt_at <= NOW()
		ORDER BY occurred_at
		LIMIT $1
		FOR UPDATE SKIP LOCKED
	`

	rows, err := tx.QueryContext(ctx, selectQuery, limit)
	if err != nil {
		return 0, fmt.Errorf("claim outbox events: %w", err)
	}

	type claimed struct {
		event    OutboxEvent
		attempts int
	}
	var batch []claimed

	for rows.Next() {
		var c claimed
		if err := rows.Scan(
			&c.event.ID,
			&c.event.AggregateID,
			&c.event.EventType,
			&c.event.Payload,
			&c.event.CorrelationID,
			&c.event.CausationID,
			&c.event.OccurredAt,
			&c.attempts,
		); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan outbox event: %w", err)
		}
		batch = append(batch, c)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("iterate outbox events: %w", err)
	}
	_ = rows.Close()

	for _, c := range batch {
		publishErr := publish(ctx, c.event)
		if publishErr != nil {
			attempts := c.attempts + 1
			_, err = tx.ExecContext(
				ctx,
				`UPDATE outbox_events
				 SET attempts = $1, next_attempt_at = $2
				 WHERE id = $3`,
				attempts,
				time.Now().UTC().Add(backoff(attempts)),
				c.event.ID,
			)
			if err != nil {
				return published, fmt.Errorf("record outbox publish failure: %w", err)
			}
			continue
		}

		_, err = tx.ExecContext(
			ctx,
			`UPDATE outbox_events SET published_at = NOW() WHERE id = $1`,
			c.event.ID,
		)
		if err != nil {
			return published, fmt.Errorf("mark outbox event published: %w", err)
		}
		published++
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit outbox publish: %w", err)
	}

	return published, nil
}
