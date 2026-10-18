# 0007. Grow a PostgreSQL-centric engine first, a single-writer engine later

- Status: accepted
- Date: 2026-10-16

## Context

The project's goal is high volume without giving up the invariants of a
wallet: no negative balance, every movement exactly once, an append-only
ledger that always explains the balance, an event for every change. Three
ways to scale were considered.

- **A. PostgreSQL at the center.** Stateless API instances, the database
  as the single source of truth and coordination point, made fast by
  using it better: one statement per operation, a native pgx pool, HOT
  updates, partitioning, batched publishing and consuming. Ceiling: the
  write throughput of one primary; past it, shard by wallet.
- **B. Single writer per partition (LMAX style).** Commands partitioned
  by wallet in Kafka, each partition owned by one instance that keeps
  balances in memory and commits in micro-batches. Much higher
  throughput, paid for with partition fencing, cache rebuilds, partition
  skew and an extra broker hop on the synchronous HTTP path.
- **C. A dedicated ledger engine (TigerBeetle) with PostgreSQL for
  metadata.** Enormous throughput and native two-phase transfers, but the
  core problem moves into a third-party system and a hard dual-write
  appears between the two stores.

## Decision

Build A first, measured against the as-delivered baseline. Add B in phase
6 behind the same application port (`Engine`), benchmarked head to head
with A in the same harness. Do not adopt C.

## Consequences

Every phase has a before and after in numbers. The `Engine` port arrives
in phase 2, when the first alternative implementation needs it, not
before. B's fencing design is deferred to its own record.
