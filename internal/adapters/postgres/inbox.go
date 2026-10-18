package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrInboxPayloadMismatch = errors.New("inbox message redelivered with a different payload")

// Inbox records which broker messages a consumer has handled, so a
// redelivery of a completed message is acknowledged without being applied
// again.
type Inbox struct {
	pool *pgxpool.Pool
}

func NewInbox(pool *pgxpool.Pool) *Inbox {
	return &Inbox{pool: pool}
}

// Record registers a message's arrival and reports whether an earlier
// delivery already completed it. A message redelivered with another
// payload is ErrInboxPayloadMismatch.
func (i *Inbox) Record(ctx context.Context, consumer, messageID, payloadHash string) (alreadyCompleted bool, err error) {
	tag, err := i.pool.Exec(ctx, `
		INSERT INTO inbox_messages (id, consumer_name, message_id, payload_hash)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (consumer_name, message_id) DO NOTHING`,
		newID(), consumer, messageID, payloadHash)
	if err != nil {
		return false, fmt.Errorf("record inbox message: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return false, nil
	}

	var existingHash string
	var completedAt *time.Time
	if err := i.pool.QueryRow(ctx, `
		SELECT payload_hash, completed_at FROM inbox_messages
		WHERE consumer_name = $1 AND message_id = $2`, consumer, messageID,
	).Scan(&existingHash, &completedAt); err != nil {
		return false, fmt.Errorf("inspect existing inbox message: %w", err)
	}
	if existingHash != payloadHash {
		return false, ErrInboxPayloadMismatch
	}
	return completedAt != nil, nil
}

// Complete marks a message as durably handled.
func (i *Inbox) Complete(ctx context.Context, consumer, messageID string) error {
	if _, err := i.pool.Exec(ctx, `
		UPDATE inbox_messages SET completed_at = now()
		WHERE consumer_name = $1 AND message_id = $2`, consumer, messageID); err != nil {
		return fmt.Errorf("complete inbox message: %w", err)
	}
	return nil
}
