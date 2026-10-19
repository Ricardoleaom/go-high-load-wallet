package relay_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/relay"
)

// memoryStore hands its events to publish once and keeps the ones that
// failed.
type memoryStore struct {
	events []relay.Event
	failed []relay.Event
}

func (s *memoryStore) PublishDue(ctx context.Context, limit int, _ func(int) time.Duration,
	publish func(context.Context, relay.Event) error) (int, int, error) {
	published, failed := 0, 0
	for _, e := range s.events[:min(limit, len(s.events))] {
		if err := publish(ctx, e); err != nil {
			s.failed = append(s.failed, e)
			failed++
			continue
		}
		published++
	}
	s.events = nil
	return published, failed, nil
}

func (s *memoryStore) Backlog(context.Context) (int64, time.Duration, error) {
	return int64(len(s.events)), 0, nil
}

// sink fails the events whose ID is in failing.
type sink struct {
	failing map[string]bool
	sent    []string
}

func (s *sink) Send(_ context.Context, e relay.Event) error {
	if s.failing[e.ID] {
		return errors.New("broker unavailable")
	}
	s.sent = append(s.sent, e.ID)
	return nil
}

type batch struct{ published, failed int }

type observer struct{ batches chan batch }

func (o observer) Batch(_ context.Context, published, failed int, took time.Duration) {
	if took < 0 {
		panic("negative duration")
	}
	o.batches <- batch{published, failed}
}

func TestRelayPublishesAndReportsTheBatch(t *testing.T) {
	store := &memoryStore{events: []relay.Event{{ID: "a"}, {ID: "b"}, {ID: "c"}}}
	broker := &sink{failing: map[string]bool{"b": true}}
	obs := observer{batches: make(chan batch, 1)}

	published, err := relay.New(store, broker, 10, obs).PublishOnce(context.Background())
	if err != nil || published != 2 {
		t.Fatalf("PublishOnce = %d, %v; want 2, nil", published, err)
	}
	if got := <-obs.batches; got != (batch{2, 1}) {
		t.Errorf("observed %+v, want 2 published and 1 failed", got)
	}
	if len(broker.sent) != 2 || len(store.failed) != 1 || store.failed[0].ID != "b" {
		t.Errorf("sent %v, failed %v", broker.sent, store.failed)
	}
}

func TestRelayRunStopsWithItsContext(t *testing.T) {
	store := &memoryStore{events: []relay.Event{{ID: "a"}}}
	obs := observer{batches: make(chan batch, 100)}
	r := relay.New(store, &sink{}, 10, obs)

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error)
	go func() { stopped <- r.Run(ctx, 5*time.Millisecond) }()
	select {
	case got := <-obs.batches:
		if got.published != 1 {
			t.Errorf("first round published %d, want 1", got.published)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the relay never ran")
	}
	cancel()
	select {
	case err := <-stopped:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}
