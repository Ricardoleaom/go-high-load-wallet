CREATE INDEX idx_outbox_events_pending_by_attempt
    ON outbox_events (next_attempt_at)
    WHERE published_at IS NULL;

DROP INDEX idx_outbox_events_pending;

ALTER INDEX idx_outbox_events_pending_by_attempt RENAME TO idx_outbox_events_pending;
