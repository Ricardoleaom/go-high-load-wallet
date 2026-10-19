# 0015. Use pgx's own pool and cancel timed-out statements on the server

- Status: accepted
- Date: 2026-10-18

## Context

The service reached PostgreSQL through `database/sql` with pgx as its
driver. That pool knows nothing about PostgreSQL: its statistics are
generic. And when a request's context ended mid-statement, pgx's default
was to give up on the connection: it asked the server to cancel the
statement and then closed the connection, so the pool had to open a new
one, adding work at the moment the database had least room for it
(ADR 0012 bounds every request in time, so this happens on every timeout).

## Decision

- **pgxpool** (`internal/adapters/postgres.Open`) instead of `database/sql`:
  at most `DB_MAX_OPEN_CONNS` (10) and at least `DB_MIN_CONNS` (2, which
  replaces `DB_MAX_IDLE_CONNS`; pgxpool has no idle maximum) connections,
  each recycled after 30 minutes with 3 minutes of jitter and closed after
  5 idle minutes, health-checked every 30 seconds. Session limits
  (`statement_timeout`, `idle_in_transaction_session_timeout`,
  `lock_timeout`) stay connection parameters, as in ADR 0012.
- **Server-side cancellation that keeps the connection.** When a context
  ends mid-statement, pgx sends PostgreSQL a cancel request at once
  (`CancelRequestContextWatcherHandler`); the statement rolls back and the
  connection goes back to the pool. Only if PostgreSQL has not answered
  2 seconds later is the connection closed.
- **Timeouts cross the adapter boundary as `Timeout() bool`.** The adapter
  wraps a context deadline, a cancelled statement (57014) or a lock
  timeout (55P03) in an error that answers `Timeout()` with true, the
  convention `net.Error` follows; the HTTP adapter turns any such error
  into 503 `TIMEOUT` without importing the PostgreSQL adapter.
- **Pool metrics** `wallet_db_pool_*`: acquires, acquires that waited and
  their total wait, cancelled acquires, connections by state, the maximum
  and connections opened. They replace `go_sql_*` in the benchmark
  snapshot and the dashboard.

## Consequences

- A request that runs out of time stops its statement and frees its locks
  at once, and the pool keeps its warm connections.
  `TestTimedOutBetUnderTheWalletLockAppliesOnceWhenResent` pins the whole
  path: a bet waiting for a locked wallet times out, the server drops it,
  the bet resent with the same key once the wallet is free applies
  exactly once, and the pool neither lost a connection nor opened one; with
  pgx's default handler it ends with one connection fewer.
- A cancelled connection is held about 100 ms longer before reuse: pgx
  waits that long so a late cancel cannot hit the next query.
- Prepared statements are cached per connection (pgx's default). Behind
  PgBouncer in transaction mode that needs a decision; phase 2c makes it
  (ADR to come).
