# 002. Baseline: configuration only

- Date: 2026-10-17
- Commit: `545054a` (phase 1, same code as [001](001-baseline-as-delivered.md))
- Raw results: [`002-baseline-config-tuned/`](002-baseline-config-tuned/); the run
  tables come from `python3 bench/summarize.py docs/benchmarks/002-baseline-config-tuned/2026*`,
  the diagnosis samples and scripts are in
  [`diagnosis/`](002-baseline-config-tuned/diagnosis/)

## Question

[001](001-baseline-as-delivered.md) found two ceilings: about 82 bets per
second with the ten-connection pool full, and an outbox that publishes 10
events per second whatever the load. Can configuration alone move them?

The hypothesis was that a larger pool moves the knee and that a larger
outbox batch or a shorter interval raises the published rate, but only up
to what one publisher sending events one at a time can reach. A pool
change that leaves the knee where it is refutes the first half; an outbox
that keeps up at any setting refutes the second.

## Environment

The machine, pinning, images and seeded data of 001: an Intel Core
i5-10300H with 4 vCPUs under WSL2, PostgreSQL on CPUs 0-1, the API on CPU 2,
k6 on CPU 3, 100,000 wallets. Only the settings named in each run change.

## Method

[ADR 0009](../adr/0009-benchmark-methodology.md), one fresh stack per run.

1. Pool: `uniform-mix` knee from 20 to 160 rps in steps of 20, 60 s each,
   with `DB_MAX_OPEN_CONNS` = `DB_MAX_IDLE_CONNS` = 20 and 40 (10 is the
   001 knee); then 5 minutes steady at 60 rps for 10, 20 and 40. Steady
   runs use 60 rps rather than 001's 30, because 30 sits below every pool
   size's limit and would show no difference.
2. Outbox: `uniform-mix` steady at 30 rps for 5 minutes with
   `OUTBOX_BATCH_SIZE`/`OUTBOX_PUBLISH_INTERVAL` at 20/1s, 100/2s and
   100/1s (20/2s is 001).
3. Diagnosis, added when the first two steps refuted the plan's
   hypothesis that commits and `fsync` were the limit:
   - a 140 rps run with pool 40 sampling every PostgreSQL backend's wait
     event and CPU, switching `synchronous_commit` off halfway through;
   - the same load sampling the statement each active backend runs;
   - `pg_test_fsync` on a Docker volume of the same machine;
   - a `uniform-mix` knee from 50 to 450 rps with two indexes the
     lookups can use, created after seeding: the same code, one schema
     difference;
   - a 500 rps run with those indexes capturing PostgreSQL's backends and
     the API's goroutines while the service was stalled.

## Results

### Pool size

| target rps | pool 10: served | p99 ms | pool 20: served | p99 ms | pool 40: served | p99 ms |
| --- | --- | --- | --- | --- | --- | --- |
| 20 | 19.1 | 43.0 | 18.5 | 43.2 | 18.9 | 55.2 |
| 40 | 38.4 | 47.1 | 37.6 | 52.6 | 38.2 | 46.0 |
| 60 | 58.4 | 69.0 | 57.6 | 60.5 | 58.2 | 108.1 |
| 80 | 78.1 | 332.2 | 77.5 | 221.2 | 78.1 | 154.3 |
| 100 | **81.9** | 2,466 | **85.9** | 2,346 | **85.2** | 2,400 |
| 120 | 75.0 | 3,033 | 75.0 | 2,592 | 74.1 | 2,476 |
| 140 | 68.3 | 3,890 | 74.4 | 2,620 | 72.3 | 2,481 |
| 160 | 48.2 | 4,833 | 49.5 | 4,732 | 58.3 | 3,282 |

Server-side, per knee step after the ramp. From 100 rps PostgreSQL uses
185–191% of a core, both of its two, with every pool size; the API never
passes 17% of its own.

| steady 60 rps | p50 ms | p95 ms | p99 ms | pool in use (avg) | CPU % API/PostgreSQL | verify |
| --- | --- | --- | --- | --- | --- | --- |
| pool 10 | 22.3 | 38.4 | 65.4 | 1.8 | 13/112 | pass |
| pool 20 | 23.5 | 48.0 | 92.7 | 2.2 | 13/121 | pass |
| pool 40 | 22.6 | 43.1 | 78.2 | 2.9 | 13/115 | pass |

### Outbox

| batch / interval | published/s | backlog after 5 min | oldest pending | avg batch | per event | p99 ms | verify |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 20 / 2s (001 r2) | 10.0 | 13,687 | 241 s | 160 ms | 8.3 ms | 50.0 | pass |
| 20 / 1s | 20.0 | 10,914 | 191 s | 106 ms | 5.9 ms | 35.1 | pass |
| 100 / 2s | 49.9 | 2,104 | 37 s | 344 ms | 4.1 ms | 37.4 | pass |
| 100 / 1s | **56.9** | **50** | **1 s** | 195 ms | 4.2 ms | 43.0 | pass |

`uniform-mix` at 30 rps produces about 57 events a second. Batch and per
event times come from the API's `outbox_publish_batch_duration_seconds`
at the end of each run (`metrics-after.txt`).

### Where PostgreSQL's CPU goes

Both runs below offer 140 rps with pool 40, so PostgreSQL is saturated.
Their samples are in [`diagnosis/`](002-baseline-config-tuned/diagnosis/).

**It is CPU, not the disk.** Over two minutes PostgreSQL's backends used
1.92 of its 2 cores (from `/proc`). In 60 samples of `pg_stat_activity`
over the same two minutes, an average of 25.8 backends were running with
no wait event, 4.2 were idle in a transaction waiting for the API's next
statement, 0.6 were waiting for a WAL `fsync` and 0.3 for the WAL write
lock. The disk is not slow either: `pg_test_fsync` measures `fdatasync`
at 0.67 ms, about 1,480 flushes a second, while these runs never commit
more than about 460 times a second, and group commit lets one flush serve
several of those. Switching
`synchronous_commit` off halfway through changed nothing: the served rate
kept sliding from 87 to 77 per second, as it had from 109 since the start
of the run at a constant offered load.

**It is two statements.** Sampling the statement of every active backend
twice a second, 98–99% of the backends on CPU were running one of these,
both early and late in the run:

```sql
SELECT id FROM transactions WHERE provider_id = $1 AND idempotency_key = $2
SELECT id FROM transactions WHERE provider_id = $1 AND external_id = $2
```

In four minutes the `transactions` table was read sequentially 38,445
times, 4.2 billion rows in all, about 109,000 per scan. On a freshly seeded
database each lookup is a sequential scan that discards 100,000 rows and
takes 9–11 ms.

**Why.** The indexes that enforce uniqueness on those columns are partial:
migration `000008` added `WHERE kind != 'OPENING'` when wallet openings
became transactions without a provider. The lookups do not repeat that
condition, so PostgreSQL cannot prove the partial index covers them and
reads the table instead. Every new submission makes both lookups before
it starts its transaction, a replay at least the first, and a reversal
makes the second one again for its original. The seed's 100,000
openings, one per wallet, are rows every lookup reads, and every bet adds
one more, which is why the cost per bet grows during a run and
throughput falls past the peak.

**Same code, usable indexes.** To size the effect without changing code,
one more knee ran with two single-column indexes, on `idempotency_key`
and on `external_id`, created after seeding, so each lookup has exactly
one index it can use. The plans go from a sequential scan of 100,000 rows
in 9–11 ms to an index scan of 2 buffers in 0.1–0.3 ms
([`explain-before.txt`, `explain-after.txt`](002-baseline-config-tuned/20261017T045619Z-uniform-mix-knee-lookup-index-clean/)).

| target rps | served rps | p50 ms | p99 ms | pool waits/s | CPU % API/PostgreSQL |
| --- | --- | --- | --- | --- | --- |
| 50 | 45.9 | 4.9 | 22.3 | 0.0 | 12/15 |
| **100** | **94.1** | **4.7** | **26.9** | 0.3 | 22/31 |
| 150 | 144.1 | 4.6 | 57.7 | 5.9 | 30/51 |
| 200 | 194.0 | 4.9 | 86.6 | 18.3 | 41/68 |
| 250 | 244.0 | 7.2 | 196.3 | 144.6 | 51/100 |
| 300 | 294.0 | 8.8 | 187.3 | 249.6 | 59/143 |
| 350 | 340.3 | 40.9 | 733.1 | 851.9 | 63/165 |
| 400 | 392.7 | 38.1 | 390.8 | 1,083.7 | 65/178 |
| 450 | **443.0** | 31.8 | 576.4 | 1,226.4 | 66/186 |

Invariants passed after the run.

| | as delivered (001) | indexes only |
| --- | --- | --- |
| knee (p99 under 50 ms) | 40 rps | **100 rps** |
| p50 at the knee | 18.0 ms | **4.7 ms** |
| highest served rate | 82 rps, then falling | **443 rps** at the last step |
| PostgreSQL CPU per bet | about 18 ms | **about 4.2 ms** |

A first attempt with composite `(provider_id, …)` indexes is kept in
[`diagnosis/3-composite-lookup-indexes/`](002-baseline-config-tuned/diagnosis/3-composite-lookup-indexes/).
Right after seeding every `provider_id` in the table is null, the planner
could not tell the two indexes apart, and the idempotency lookup used the
external-id index with a filter until autovacuum analyzed the table three
minutes into the run. The table's statistics matter as much as its
indexes, and the phase 2 runs have to start from representative ones.

### A pool deadlock under overload

In the first indexed knee, the composite-index one that climbed to 600
rps, the service stopped completely at the 500 rps step for
about a minute: PostgreSQL committed nothing, its CPU and the API's fell
to almost zero and all ten pool connections stayed in use; then it
recovered, stopped again and recovered again. A 500 rps steady run
reproduced it and captured the state during the stall
([`diagnosis/4-pool-deadlock/`](002-baseline-config-tuned/diagnosis/4-pool-deadlock/)):

- All ten connections were `idle in transaction`, waiting for the API's
  next statement, each right after
  `SELECT id FROM transactions WHERE id = $1 FOR UPDATE`; no lock was
  waiting and no backend was blocked by another.
- The API's goroutine dump showed exactly ten goroutines in
  `TransactionRepository.GetByID` called from `LockByID`, from
  `applyReversal`, all waiting in `database/sql.(*DB).conn` for a pool
  connection; hundreds of other requests waited behind them.

`LockByID` takes the row lock through the reversal's database
transaction and then reads the row back through the pool, which needs a
second connection while the first is held. When as many reversals as
there are pool connections reach that point together, each holds one
connection and waits for another that will never be free. Nothing breaks
the cycle until the clients give up: k6's 60-second timeout cancels the
requests, the connections return and the service resumes. A client
without a timeout would leave it stuck. The as-delivered calibration
sweep in 001 stopped committing for about 30 seconds at its 500–600 rps
steps with the same signature; that is where its requests waited a
minute for a connection.

It takes a saturated pool, so it never appeared at or below the knee,
and every invariant still held: the stalled transactions roll back.

## Analysis

1. **Configuration does not move the ceiling.** With 10, 20 or 40
   connections the service peaks at 82, 86 and 85 rps with PostgreSQL's
   two cores saturated; at 60 rps the larger pools only add backends
   competing for the same cores (p99 65, 93 and 78 ms). The hypothesis that
   the pool moves the knee is refuted: the pool was full because the
   database was slow, not the other way round.
2. **The first ceiling was a defect, not the design.** Two lookups that
   cannot use their indexes cost about 14 of the 18 ms of database CPU per
   bet and grow with every row stored. With indexes they can use, the same
   code serves 5.4 times the peak (443 against 82 rps), its knee moves from
   40 to 100 rps and the median falls from 18 to under 5 ms.
3. **Behind it are the ceilings 001 predicted.** At about 4.2 ms of
   database CPU per bet, eleven statements and 4.3 commits saturate two
   cores near 450 rps, and the ten-connection pool starts to queue at
   150 rps, which is what pushes the p99 over 50 ms there: 5.9 waits a
   second at 150 rps, 1,226 at 450. The API reaches two thirds of its core
   at 443 rps and comes next.
4. **One sequential publisher cannot carry the events.** The publisher
   moves exactly `batch / interval` events a second, and with 100 every
   second it keeps up with 30 rps (backlog 50, oldest event 1 s). But each
   event costs 4–8 ms sent one by one, so one publisher tops out near 240
   events a second, about 125 bets a second, while 443 rps produce some
   840. Larger batches only spread the cost of the claim query; this half
   of the hypothesis holds.
5. **Overload is unsafe.** Without admission control a saturated service
   queues without bound, and once the pool is full, reversals can deadlock
   it until clients time out.
6. **The invariants held in every run**, including the stalls: every
   stalled transaction rolled back, and all eight checks passed after
   each of the thirteen runs in this report.

## Targets for phases 2 and 3

The project's goal is thousands of requests a second. On this machine,
with PostgreSQL on two cores, the targets below are the steps toward it;
each phase's report measures them with this harness against 001 and 002.

**Phase 2, PostgreSQL hot path (v0.3.0).** It starts by fixing the two
defects, each with a regression test that fails first: the lookups state
`kind <> 'OPENING'` so the unique indexes serve them, and `LockByID` reads
the row inside its own transaction. Report 003 measures those fixes alone
before the hot path is rewritten.

| `uniform-mix`, 100,000 wallets | 001 | 002, indexes only | phase 2 target |
| --- | --- | --- | --- |
| knee, p99 under 50 ms | 40 rps | 100 rps | **≥ 1,000 rps** |
| p50 at 80% of the knee | 20 ms | under 5 ms | ≤ 5 ms |
| PostgreSQL CPU per bet | about 18 ms | about 4.2 ms | ≤ 1 ms |
| statements and commits per bet | 11 and 4.3 | 11 and 4.3 | one round trip, one commit |
| offered load above the peak | throughput falls, waits of a minute, deadlock | not measured | served rate ≥ 90% of the peak; the excess rejected in under 10 ms with 429 or 503; no stall |
| `hot-wallet` knee | about 50 rps | not measured | ≥ 300 rps |
| invariants | pass | pass | pass, plus a linearizability check of concurrent histories |

At 450 rps k6 already uses 29% of its core, so above roughly 1,200 rps
the generator would saturate before the service. Phase 2 re-calibrates the
pinning or lightens the scenarios, and records it, before reporting
anything above that.

**Phase 3, Kafka (v0.4.0).**

| | today | phase 3 target |
| --- | --- | --- |
| events published per second | 10 as shipped; about 240 at most with one sequential publisher | ≥ the rate produced at the phase 2 knee, about 2,000 a second |
| age of the oldest unpublished event | 241 s at 30 rps | p99 under 1 s at the phase 2 knee |
| bets ingested through the broker | SQS, not measured | ≥ 1,000 a second with the same invariants |
| relay killed with `kill -9` | not tested | no event lost; duplicates only as at-least-once allows |

## Conclusion

Configuration alone cannot fix the as-delivered service: the pool size
changes nothing and the outbox settings only trade one fixed rate for
another. The measurements found what the diagnosis before them did not:
the first ceiling is two lookups reading the whole `transactions` table,
which an index-only change lifts 5.4 times, and under overload a
reversal path can deadlock the pool. Phase 2 opens by fixing both, then
rebuilds the hot path toward 1,000 bets a second within a 50 ms p99.
