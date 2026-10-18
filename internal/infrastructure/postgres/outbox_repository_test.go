package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/testsupport/pgtest"
)

func TestBacklogCountsOnlyUnpublishedEvents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db, err := Open(ctx, pgtest.FreshURL(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	repository := NewOutboxRepository(db)

	empty, oldest, err := repository.Backlog(ctx)
	if err != nil || empty != 0 || oldest != 0 {
		t.Fatalf("empty outbox: pending=%d oldest=%v err=%v, want 0 0 nil", empty, oldest, err)
	}

	now := time.Now().UTC()
	for _, event := range []struct {
		age       time.Duration
		published bool
	}{{60 * time.Second, false}, {10 * time.Second, false}, {120 * time.Second, true}} {
		_, err := db.ExecContext(ctx, `
			INSERT INTO outbox_events (id, aggregate_id, event_type, payload, correlation_id, occurred_at, published_at)
			VALUES ($1, 'aggregate', 'TestEvent', '{}', 'correlation', $2, CASE WHEN $3 THEN NOW() END)
		`, uuid.NewString(), now.Add(-event.age), event.published)
		if err != nil {
			t.Fatalf("insert outbox event: %v", err)
		}
	}

	pending, oldest, err := repository.Backlog(ctx)
	if err != nil {
		t.Fatalf("Backlog: %v", err)
	}
	if pending != 2 {
		t.Fatalf("pending = %d, want 2 (the published event must not count)", pending)
	}
	if oldest < 59*time.Second || oldest > 70*time.Second {
		t.Fatalf("oldest = %v, want about 60s", oldest)
	}
}

func TestPublishDueBacksOffAFailedEvent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db, err := Open(ctx, pgtest.FreshURL(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	repository := NewOutboxRepository(db)

	eventID := uuid.NewString()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO outbox_events (id, aggregate_id, event_type, payload, correlation_id, occurred_at)
		VALUES ($1, 'aggregate', 'TestEvent', '{}', 'correlation', NOW())
	`, eventID); err != nil {
		t.Fatalf("insert outbox event: %v", err)
	}

	backoff := func(attempts int) time.Duration { return time.Duration(attempts) * time.Hour }
	failing := func(context.Context, OutboxEvent) error { return errors.New("broker unavailable") }
	published, err := repository.PublishDue(ctx, 10, backoff, failing)
	if err != nil || published != 0 {
		t.Fatalf("PublishDue with a failing send = %d, %v; want 0, nil", published, err)
	}

	var attempts int
	var nextAttempt time.Time
	var publishedAt *time.Time
	read := func() {
		t.Helper()
		if err := db.QueryRowContext(ctx,
			`SELECT attempts, next_attempt_at, published_at FROM outbox_events WHERE id = $1`, eventID,
		).Scan(&attempts, &nextAttempt, &publishedAt); err != nil {
			t.Fatalf("read event: %v", err)
		}
	}
	read()
	if attempts != 1 || publishedAt != nil || time.Until(nextAttempt) < 59*time.Minute {
		t.Fatalf("after a failed send: attempts=%d next in %v published=%v; want 1, about 1h, nil",
			attempts, time.Until(nextAttempt), publishedAt)
	}

	// Not due yet: nothing is claimed, even with a working send.
	sent := 0
	working := func(context.Context, OutboxEvent) error { sent++; return nil }
	if published, err := repository.PublishDue(ctx, 10, backoff, working); err != nil || published != 0 || sent != 0 {
		t.Fatalf("PublishDue before the backoff ends = %d, %v (sent %d); want nothing", published, err, sent)
	}

	if _, err := db.ExecContext(ctx, `UPDATE outbox_events SET next_attempt_at = NOW() WHERE id = $1`, eventID); err != nil {
		t.Fatalf("make event due: %v", err)
	}
	if published, err := repository.PublishDue(ctx, 10, backoff, working); err != nil || published != 1 {
		t.Fatalf("PublishDue once due = %d, %v; want 1, nil", published, err)
	}
	read()
	if publishedAt == nil {
		t.Fatal("event not marked published")
	}
}
