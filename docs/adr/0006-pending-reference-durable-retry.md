# 0006. Hold early reversals as PENDING_REFERENCE with a durable retry

- Status: accepted
- Date: 2026-10-16

## Context

A `REFUND` or `ROLLBACK` can arrive before the bet it reverses, because
providers send over different channels and queues do not order across
groups. Rejecting it loses money; applying it blindly is wrong.

## Decision

A reversal whose reference does not exist yet is stored as
`PENDING_REFERENCE` with a next attempt time. A worker claims due rows
atomically (`UPDATE ... WHERE id IN (SELECT ... FOR UPDATE SKIP LOCKED)`)
and retries with exponential backoff from 5 s, capped at 5 minutes; after
8 attempts it rejects with `REFERENCE_NOT_FOUND_TIMEOUT`. A partial unique
index allows only one successful reversal per original transaction.

## Consequences

Out-of-order arrival is handled without losing operations, across
restarts and instances. It also means consumers can tolerate reordering,
which lets the phase 3 Kafka consumer use non-blocking retries.
