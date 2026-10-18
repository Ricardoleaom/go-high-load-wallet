package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/adapters/postgres"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/relay"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/testsupport/pgtest"
)

// insertEvent writes an outbox row that is age old, published or not.
func insertEvent(t *testing.T, pool *pgxpool.Pool, age time.Duration, published bool) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := pool.Exec(testContext(t), `
		INSERT INTO outbox_events (id, aggregate_id, event_type, payload, correlation_id, causation_id, occurred_at, published_at)
		VALUES ($1, 'aggregate', 'TestEvent', '{"k": 1}', 'correlation', 'cause', $2, CASE WHEN $3 THEN now() END)`,
		id, time.Now().Add(-age), published); err != nil {
		t.Fatalf("insert outbox event: %v", err)
	}
	return id
}

func TestBacklogCountsOnlyUnpublishedEvents(t *testing.T) {
	pool := openPool(t, pgtest.FreshURL(t))
	store := postgres.NewOutboxStore(pool)

	pending, oldest, err := store.Backlog(testContext(t))
	if err != nil || pending != 0 || oldest != 0 {
		t.Fatalf("empty outbox: pending=%d oldest=%v err=%v, want 0 0 nil", pending, oldest, err)
	}

	insertEvent(t, pool, 60*time.Second, false)
	insertEvent(t, pool, 10*time.Second, false)
	insertEvent(t, pool, 120*time.Second, true)

	pending, oldest, err = store.Backlog(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if pending != 2 {
		t.Errorf("pending = %d, want 2 (the published event must not count)", pending)
	}
	if oldest < 59*time.Second || oldest > 70*time.Second {
		t.Errorf("oldest = %v, want about 60s", oldest)
	}
}

func TestPublishDueSendsEveryFieldAndMarksThePublished(t *testing.T) {
	pool := openPool(t, pgtest.FreshURL(t))
	store := postgres.NewOutboxStore(pool)
	id := insertEvent(t, pool, time.Second, false)

	var sent []relay.Event
	published, failed, err := store.PublishDue(testContext(t), 10, relay.Backoff,
		func(_ context.Context, e relay.Event) error { sent = append(sent, e); return nil })
	if err != nil || published != 1 || failed != 0 {
		t.Fatalf("PublishDue = %d published, %d failed, %v", published, failed, err)
	}
	e := sent[0]
	if e.ID != id || e.AggregateID != "aggregate" || e.EventType != "TestEvent" || string(e.Payload) != `{"k": 1}` ||
		e.CorrelationID != "correlation" || e.CausationID != "cause" || time.Since(e.OccurredAt) > time.Minute {
		t.Errorf("sent %+v", e)
	}
	if pending, _, _ := store.Backlog(testContext(t)); pending != 0 {
		t.Errorf("%d events still pending", pending)
	}
}

func TestPublishDueBacksOffAFailedEvent(t *testing.T) {
	pool := openPool(t, pgtest.FreshURL(t))
	store := postgres.NewOutboxStore(pool)
	eventID := insertEvent(t, pool, 0, false)

	backoff := func(attempts int) time.Duration { return time.Duration(attempts) * time.Hour }
	failing := func(context.Context, relay.Event) error { return errors.New("broker unavailable") }
	published, failed, err := store.PublishDue(testContext(t), 10, backoff, failing)
	if err != nil || published != 0 || failed != 1 {
		t.Fatalf("PublishDue with a failing send = %d, %d, %v; want 0, 1, nil", published, failed, err)
	}

	var attempts int
	var nextAttempt time.Time
	var publishedAt *time.Time
	read := func() {
		t.Helper()
		if err := pool.QueryRow(testContext(t),
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
	working := func(context.Context, relay.Event) error { sent++; return nil }
	if published, _, err := store.PublishDue(testContext(t), 10, backoff, working); err != nil || published != 0 || sent != 0 {
		t.Fatalf("PublishDue before the backoff ends = %d, %v (sent %d); want nothing", published, err, sent)
	}

	if _, err := pool.Exec(testContext(t), `UPDATE outbox_events SET next_attempt_at = now() WHERE id = $1`, eventID); err != nil {
		t.Fatal(err)
	}
	if published, _, err := store.PublishDue(testContext(t), 10, backoff, working); err != nil || published != 1 {
		t.Fatalf("PublishDue once due = %d, %v; want 1, nil", published, err)
	}
	read()
	if publishedAt == nil {
		t.Fatal("event not marked published")
	}
}

// A batch is published inside one transaction that waits on the broker
// between statements; the session's idle-in-transaction limit must not end
// it halfway and undo the bookkeeping of events already sent.
func TestPublishDueSurvivesSendsSlowerThanTheIdleInTransactionLimit(t *testing.T) {
	pool := openPool(t, pgtest.FreshURL(t), postgres.WithSessionTimeouts(0, 200*time.Millisecond, 0))
	store := postgres.NewOutboxStore(pool)
	insertEvent(t, pool, time.Second, false)
	insertEvent(t, pool, time.Second, false)

	slow := func(context.Context, relay.Event) error { time.Sleep(300 * time.Millisecond); return nil }
	published, _, err := store.PublishDue(testContext(t), 10, func(int) time.Duration { return time.Minute }, slow)
	if err != nil || published != 2 {
		t.Fatalf("PublishDue with sends slower than the idle limit = %d, %v; want 2, nil", published, err)
	}
	if pending, _, _ := store.Backlog(testContext(t)); pending != 0 {
		t.Fatalf("pending after the batch = %d, want 0", pending)
	}
}

// Two relays polling together split the due events between them.
func TestConcurrentPublishersNeverSendAnEventTwice(t *testing.T) {
	url := pgtest.FreshURL(t)
	pool := openPool(t, url)
	for range 40 {
		insertEvent(t, pool, time.Second, false)
	}
	stores := []*postgres.OutboxStore{postgres.NewOutboxStore(pool), postgres.NewOutboxStore(openPool(t, url))}

	seen := make(chan string, 100)
	slow := func(_ context.Context, e relay.Event) error {
		seen <- e.ID
		time.Sleep(5 * time.Millisecond)
		return nil
	}
	done := make(chan error, 2)
	for _, store := range stores {
		go func() {
			_, _, err := store.PublishDue(testContext(t), 40, relay.Backoff, slow)
			done <- err
		}()
	}
	for range stores {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	close(seen)

	counts := map[string]int{}
	for id := range seen {
		counts[id]++
	}
	for id, n := range counts {
		if n != 1 {
			t.Errorf("event %s sent %d times", id, n)
		}
	}
	if len(counts) != 40 {
		t.Errorf("%d distinct events sent, want 40", len(counts))
	}
}
