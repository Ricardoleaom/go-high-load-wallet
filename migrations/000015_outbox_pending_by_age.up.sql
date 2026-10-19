-- The relay claims the oldest due events. Indexed by next_attempt_at, the
-- pending events had to be sorted by age on every round, all of them: under
-- load the backlog grew until the claim ran past the statement timeout and
-- the relay published nothing. Indexed by age, the claim reads pending
-- events in order and stops at the batch.
CREATE INDEX idx_outbox_events_pending_by_age
    ON outbox_events (occurred_at)
    WHERE published_at IS NULL;

DROP INDEX idx_outbox_events_pending;

ALTER INDEX idx_outbox_events_pending_by_age RENAME TO idx_outbox_events_pending;
