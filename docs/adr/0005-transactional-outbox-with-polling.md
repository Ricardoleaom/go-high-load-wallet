# 0005. Publish events through a transactional outbox, polled

- Status: accepted (throughput limits addressed in phase 3, see 0008)
- Date: 2026-10-16

## Context

Every committed change must produce its event, and no event may exist for
a change that rolled back. Publishing to the broker inside the database
transaction cannot give that guarantee.

## Decision

Events are inserted into `outbox_events` in the same SQL transaction as
the change. A publisher in every instance claims due rows with `SELECT ...
FOR UPDATE SKIP LOCKED`, sends them, and marks `published_at`; failed
sends back off exponentially. The row ID is the event ID and the
broker's deduplication ID, so a crash between sending and marking resends
the same event: delivery is at least once, deduplicated by `eventId`.

## Consequences

The outbox is correct and simple, and several publishers never claim the
same row. As shipped it publishes at most 20 events every 2 seconds per
instance, sequentially, inside an open transaction: about 10 events per
second, while a single bet produces two. The phase 1 baseline measures
that ceiling; phase 3 replaces the publisher.
