# 0004. Enforce idempotency with a unique index and a canonical payload hash

- Status: accepted
- Date: 2026-10-16

## Context

Providers retry. The same operation arrives twice over HTTP, over SQS, or
once over each, sometimes concurrently and sometimes after a restart. It
must move money at most once, and a retry that changed the payload must be
refused rather than silently accepted.

## Decision

An operation's identity is `(provider_id, idempotency_key)`, backed by a
partial unique index on `transactions`; `(provider_id, external_id)` has
another, so the same financial operation cannot be replayed under a new
key. A SHA-256 of a canonical JSON of the business fields (fixed key
order, amounts normalized) is stored with the transaction. A repeated key
with the same hash returns the persisted result with `idempotentReplay:
true`; a different hash is `IDEMPOTENCY_CONFLICT`. When two first
submissions race, the unique index lets exactly one insert win and the
loser replays the winner's result.

## Consequences

The guarantee survives restarts and holds across instances and transports
because it lives in the database. Replays read the balance recorded in the
ledger at the time, not the current one. The idempotency lookups are part
of the round trips phase 2 removes from the common path.

Since wallet openings became transactions without a provider, the unique
indexes are partial (`WHERE kind != 'OPENING'`). A lookup that does not
state that condition cannot use them and reads the whole table, which
[benchmark 002](../benchmarks/002-baseline-config-tuned.md) found to be
the first throughput ceiling; queries against these indexes must repeat
their predicate.
