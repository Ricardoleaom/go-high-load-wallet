# 0012. Bound every request in time and size, and answer the client's mistakes with 4xx

- Status: accepted
- Date: 2026-10-17

## Context

Benchmark [002](../benchmarks/002-baseline-config-tuned.md) and the audit
that followed found a service with no limits of its own:

- the HTTP server only bounded how long a client took to send headers;
  nothing bounded a request once it was in, so the pool deadlock 002
  reproduced lasted until k6 gave up after 60 seconds, and a client
  without a timeout would have waited forever;
- PostgreSQL sessions had no statement, lock or idle-in-transaction
  timeout;
- request bodies and text fields had no size limit (a 20 MB body was read
  in full), and an identifier that was not a UUID travelled to PostgreSQL
  and came back as a 500.

A 500 tells the client nothing it can act on and, at volume, buries real
failures in alerts the client caused.

## Decision

**Time.** Every authenticated request gets a deadline (`REQUEST_TIMEOUT`,
5 s) that every database call inherits. Each pooled connection carries
`statement_timeout` (5 s), `idle_in_transaction_session_timeout` (10 s)
and `lock_timeout` (2 s). A request that runs out of time, or whose
statement or lock times out, answers **503 `TIMEOUT`** with `Retry-After`:
the operation did not happen, its transaction rolled back, and resending
it with the same idempotency key is safe. The HTTP server bounds reads
(10 s), writes (the request deadline plus 5 s) and idle keep-alive
connections (120 s); the admin server allows 70 s for a 60-second
profile.

**Size.** Request bodies are capped at 64 KiB (**413 `BODY_TOO_LARGE`**).
Free-text fields of a submission and the `Idempotency-Key` are capped at
255 characters (**400 `VALIDATION_ERROR`**).

**Shape.** Identifiers are canonical UUIDs. A malformed one in a path, a
body or the ledger cursor answers **400** (`INVALID_ID`,
`INVALID_PLAYER_ID`, `INVALID_CURSOR`) before anything reaches the
database. Submissions are validated in the application layer, so SQS
messages get the same rules and are counted as `invalid`.

## Consequences

- No client input produces a 500, and no request outlives its budget: a
  stall like 002's turns into fast 503s instead of minute-long waits.
- The time budget is fixed per process. A per-route budget, and the
  propagation of a client's own deadline, come with admission control in
  phase 2c, which also decides how 503s are shed under overload.
- `lock_timeout` below the request deadline means a hot wallet's queue of
  waiters gives up after 2 s instead of piling up; phase 2b measures how
  often that happens on `hot-wallet`.
- The limits are settings, so a deployment with slower dependencies can
  raise them without a code change.
