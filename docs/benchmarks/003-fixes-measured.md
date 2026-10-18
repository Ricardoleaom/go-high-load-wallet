# 003. The fixes measured

- Date: 2026-10-18
- Commit: `3247253` (phase 2a: the defects 002 found fixed, hot path otherwise unchanged)
- Raw results: [`003-fixes-measured/`](003-fixes-measured/); every table below comes from
  `python3 bench/summarize.py docs/benchmarks/003-fixes-measured/2026*`

## Question

[002](002-baseline-config-tuned.md) traced the as-delivered ceiling to two
defects: idempotency lookups that read the whole `transactions` table, and
a pool deadlock in reversals under overload. Phase 2a fixed both, bounded
every request and database session in time, and validated input. Does the
fixed code reach what 002's index-only experiment predicted (about 443
rps), and does overload still stall it?

The fixes are refuted if the knee or the peak stay near 001's, if any run
stalls (an interval with no commits) or if a 5xx other than a deliberate
503 appears.

## Environment

The machine, pinning, images and seeded data of 001 and 002: an Intel Core
i5-10300H with 4 vCPUs under WSL2, PostgreSQL on CPUs 0-1, the API on CPU 2,
k6 on CPU 3, 100,000 wallets, default settings (pool 10/5, outbox 20 every
2 s, tracing at 1%, `REQUEST_TIMEOUT` 5 s, `DB_LOCK_TIMEOUT` 2 s).

## Method

[ADR 0009](../adr/0009-benchmark-methodology.md), one fresh stack per run.

1. `uniform-mix` and `hot-wallet` knees from 50 to 500 rps in steps of 50,
   60 s each.
2. `uniform-mix` steady at 120 rps (80% of the knee), 5 minutes, three
   runs; `replay-storm` and `read-mix` at the same rate.
3. Overload: `uniform-mix` steady at 750 rps for 3 minutes, about 1.5
   times the highest knee step.
4. Contention: `hot-wallet` steady at 350 and 500 rps for 2 minutes, the
   second with the API's own response counters captured at the end.

## Results

### Where it breaks now (`uniform-mix` knee)

| target rps | served rps | p50 ms | p99 ms | pool waits/s | CPU % API/PostgreSQL |
| --- | --- | --- | --- | --- | --- |
| 50 | 46.4 | 7.3 | 23.7 | 0.0 | 13/22 |
| 100 | 94.7 | 6.3 | 211.6 | 4.9 | 22/29 |
| **150** | **144.7** | **4.5** | **42.4** | 3.2 | 32/45 |
| 200 | 194.7 | 4.8 | 85.2 | 21.2 | 41/66 |
| 250 | 244.8 | 6.3 | 123.9 | 104.4 | 53/102 |
| 300 | 294.8 | 8.1 | 190.6 | 236.8 | 56/124 |
| 350 | 344.5 | 13.9 | 232.2 | 522.9 | 63/164 |
| 400 | 388.7 | 27.7 | 586.4 | 907.4 | 63/165 |
| 450 | 444.9 | 23.4 | 235.6 | 1,035.1 | 65/185 |
| 500 | **494.9** | 35.4 | 316.6 | 1,516.5 | 75/192 |

Server-side, per step after the ramp; CPU is a percentage of one core
(PostgreSQL has two). No step returned a 5xx.

- **Knee: 150 rps** by the 50 ms rule. The 100 rps step reads 211 ms
  because of one stall of about 400 ms, 82 s into the run, that held the
  p99 of a single 30-second window; the median never moved. A smaller one
  shows at 180 s. Their timing suggests a checkpoint or autovacuum; the
  runner does not keep PostgreSQL's logs yet, so this is left to the tuning
  work of phase 2c.
- **The service serves every step up to 500 rps**, where PostgreSQL uses
  192% of a core.

### Steady state at 120 rps

| run | p50 ms | p95 ms | p99 ms | failed | CPU % API/PostgreSQL | verify |
| --- | --- | --- | --- | --- | --- | --- |
| `uniform-mix` r1 | 5.3 | 15.2 | 62.4 | 0% | 23/49 | pass |
| `uniform-mix` r2 | 5.3 | 13.4 | 28.1 | 0% | 23/48 | pass |
| `uniform-mix` r3 | 5.3 | 13.5 | 28.2 | 0% | 23/48 | pass |
| **median (range)** | **5.3** | **13.5 (13.4–15.2)** | **28.2 (28.1–62.4)** | 0% | | |
| `replay-storm` | 5.1 | 11.8 | 20.1 | 0% | 22/41 | pass |
| `read-mix` | 1.0 | 7.8 | 12.6 | 0% | 11/15 | pass |

Client-side, from k6. r1's p99 caught one of the short stalls above.

### Overload and contention

| run | offered rps | served rps | p50 ms | p99 ms | 5xx | lowest commits/s | verify |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `uniform-mix` | 750 | 568 (619 at the lowest 5-s point) | 425 | 2,057 | 0 | 2,331 | pass |
| `hot-wallet` | 350 | 236 | 662 | 1,908 | 0 | 947 | pass |
| `hot-wallet` | 500 | 226 | 1,005 | 2,725 | 1 (503) | 937 | pass |

The `hot-wallet` knee:

| target rps | served rps | p50 ms | p99 ms | 5xx/s |
| --- | --- | --- | --- | --- |
| 50 | 45.7 | 6.7 | 17.1 | 0.00 |
| 100 | 93.7 | 6.4 | 23.0 | 0.00 |
| **150** | **143.7** | **4.0** | **32.3** | 0.00 |
| 200 | 193.7 | 4.2 | 136.0 | 0.00 |
| 250 | 242.2 | 75.3 | 693.5 | 0.00 |
| 300 | 208.7 | 1,017 | 4,053 | 0.09 |
| 350 | 195.8 | 1,311 | 4,454 | 0.02 |
| 400 | 163.0 | 1,556 | 4,790 | 0.17 |
| 450 | 141.4 | 1,747 | 4,903 | 0.47 |
| 500 | 128.2 | 1,873 | 4,984 | 1.09 |

The API's counters at the end of the 500 rps contention run: 30,300
responses `200` and one `503`, no `500`
([`http-status-at-end.txt`](003-fixes-measured/20261018T031456Z-hot-wallet-steady-contention500/http-status-at-end.txt)).

### Compared with 001 and 002

| `uniform-mix` | 001, as delivered | 002, same code with two indexes | 003, fixed |
| --- | --- | --- | --- |
| knee (p99 under 50 ms) | 40 rps | 100 rps | **150 rps** |
| p50 at the knee | 18.0 ms | 4.7 ms | **4.5 ms** |
| highest served rate | 82 rps, then falling | 443 rps (last step) | **495 rps (last step); 568 at 750 offered** |
| PostgreSQL CPU per bet | about 18 ms | about 4.2 ms | about 3.9 ms |
| under overload | throughput collapses, minute-long waits, deadlock | deadlock at 500 rps | **no stall; served rate stays at the peak** |
| `hot-wallet` knee | about 50 rps | not measured | **150 rps** |

## Analysis

1. **The two defects were the ceiling.** The fixed code serves six times
   001's peak (495 against 82 rps), slightly above 002's index-only
   experiment, and its knee moved from 40 to 150 rps. The median is under
   5 ms wherever the pool is not queueing.
2. **Overload no longer stalls the service.** At 750 rps offered it kept
   committing at least 2,331 times a second and served about 570–620 bets
   a second, where 001 collapsed to 48 and the composite-index run of 002
   deadlocked. Latency still climbs to seconds and k6 drops what it cannot
   send: nothing sheds the excess yet. Admission control in phase 2c turns
   that queue into fast 503s.
3. **The next ceilings are the ones 001 predicted.** PostgreSQL is
   saturated near 500 rps at about 3.9 ms of CPU per bet, the cost of
   eleven statements and four commits; the ten-connection pool queues from
   200 rps and drives the p99 above 50 ms there. Phase 2b's single
   statement per movement is aimed at both.
4. **A single wallet now serializes at about 240 bets a second.** Each bet
   holds the wallet's row lock for its whole multi-statement transaction,
   and waiters hold pool connections while they wait. Past 250 rps the
   served rate falls, and the longest waits end as 503s from the lock and
   request timeouts. That is the intended answer: 503, not a 500 or a
   hang. Shortening the lock to one statement (phase 2b) is what moves it.
5. **The time limits work as designed.** The only 5xx in any run was a
   503, under deliberate single-row contention.
6. **The outbox is unchanged**: 10 events a second at every load, so the
   backlog reaches 64,000 events in five minutes at 120 rps. Phase 3
   replaces it.
7. **Every run passed all eight invariant checks**, overload and
   contention included.

## Conclusion and next steps

With the defects fixed and every request bounded, the service as designed
reaches 150 rps within a 50 ms p99, serves about 500 rps at saturation and
keeps serving under 1.5 times that, with no stall and no 500. The
remaining distance to phase 2's target (1,000 rps within 50 ms) is the hot
path itself: eleven round trips and four commits per bet, a row lock held
for all of them, and a pool that queues without limit. Phase 2b rewrites
that path; phase 2c adds admission control, tunes PostgreSQL and looks into
the short stalls with its logs.
