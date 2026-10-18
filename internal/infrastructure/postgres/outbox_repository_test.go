package postgres

import (
	"context"
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
