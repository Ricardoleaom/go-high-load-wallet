# 001. Baseline: the service as delivered

- Date: 2026-10-17
- Commit: `545054a` (phase 1; the hot path is the code tagged `v0.1.0-challenge`, plus instrumentation)
- Raw results: [`001-baseline-as-delivered/`](001-baseline-as-delivered/); every table
  below comes from `python3 bench/summarize.py docs/benchmarks/001-baseline-as-delivered/*`

## Question

How far does the architecture delivered for the challenge go, where does it
break, and do its financial guarantees hold when it breaks? The diagnosis
before measuring predicted two ceilings: about eleven SQL round trips per
bet on a pool of ten connections, and an outbox publisher capped near ten
events per second while every request that moves money produces two.

## Environment

| | |
| --- | --- |
| Machine | Intel Core i5-10300H (4 vCPUs visible), 7.9 GB RAM, WSL2 kernel 6.18, Docker 29.7.2 |
| Pinning | PostgreSQL on CPUs 0-1, API on CPU 2, k6 on CPU 3; everything else unpinned |
| Images | `postgres:17-alpine` (default settings), Keycloak 26.0, LocalStack 3.8, Prometheus v3.15.0, Jaeger 2.22.0, k6 2.3.0 |
| Data | 100,000 seeded wallets, 1,000,000.00 BRL each |
| Service settings | as shipped: pool 10 open / 5 idle, outbox batch 20 every 2 s, tracing sampled at 1% |

Absolute numbers belong to this laptop (a 4-vCPU WSL2 box writing to a
virtual disk); what carries over is the comparison between phases on the
same machine.

## Method

[ADR 0009](../adr/0009-benchmark-methodology.md): every run builds, seeds
and warms a fresh stack, then checks the invariants. Mix for wager
scenarios: BET 60%, WIN 25%, LOSS 10%, REFUND 5%, amounts 1.00–50.00.

1. Calibration: `uniform-mix` knee from 100 to 800 rps, 30 s steps.
2. `uniform-mix` knee from 20 to 160 rps in steps of 20, 60 s each. The
   knee is the last step with p99 under 50 ms and no 5xx.
3. `uniform-mix` steady at 30 rps (about 80% of the knee), 5 minutes,
   three independent runs.
4. `zipf-mix`, `replay-storm` (10% resends) and `read-mix` (80% reads) at
   30 rps for 5 minutes; `hot-wallet` knee from 10 to 80 rps.
5. Control: `uniform-mix` at 30 rps with tracing switched off entirely.

## Results

### Where it breaks (`uniform-mix` knee)

| target rps | served rps | p50 ms | p99 ms | dropped/s | pool waits/s | CPU % API/PostgreSQL/k6 |
| --- | --- | --- | --- | --- | --- | --- |
| 20 | 19.1 | 17.8 | 43.0 | 0.0 | 0.0 | 5/32/2 |
| **40** | **38.4** | **18.0** | **47.1** | 0.0 | 0.0 | 10/63/3 |
| 60 | 58.4 | 19.0 | 69.0 | 0.0 | 0.0 | 13/108/5 |
| 80 | 78.1 | 31.4 | 332.2 | 0.2 | 29.3 | 16/171/7 |
| 100 | 81.9 | 771.1 | 2,466 | 14.9 | 328.6 | 14/187/7 |
| 120 | 75.0 | 964.8 | 3,033 | 43.0 | 313.3 | 13/185/6 |
| 140 | 68.3 | 1,136 | 3,890 | 70.0 | 285.2 | 12/187/6 |
| 160 | 48.2 | 1,703 | 4,833 | 110.3 | 201.1 | 10/185/5 |

Server-side latency from the API's histogram, per step after the ramp. CPU
is a percentage of one core: PostgreSQL has two, so 200% is all of them.

- **Knee: 40 rps.** p99 crosses 50 ms at 60 rps.
- **Peak throughput: about 82 rps.** Past it, throughput *falls* while
  latency grows: at 160 rps the service completes 48 bets per second.
- In the coarse calibration sweep the client-side p99 reached 55 s and
  104,584 iterations were dropped, with requests waiting for a pool
  connection for up to a minute. At its 500–600 rps steps the service
  stopped committing for about 30 seconds: the signature of the pool
  deadlock that [002](002-baseline-config-tuned.md#a-pool-deadlock-under-overload)
  reproduces and explains.

### Steady state at 30 rps

| run | p50 ms | p95 ms | p99 ms | failed | outbox published/s | outbox backlog after 5 min | oldest pending | verify |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `uniform-mix` r1 | 19.5 | 28.4 | 37.6 | 0% | 10.0 | 13,889 | 244 s | pass |
| `uniform-mix` r2 | 20.2 | 31.5 | 50.0 | 0% | 10.0 | 13,687 | 241 s | pass |
| `uniform-mix` r3 | 20.9 | 33.1 | 52.3 | 0% | 10.0 | 13,712 | 241 s | pass |
| **median (range)** | **20.2 (19.5–20.9)** | **31.5 (28.4–33.1)** | **50.0 (37.6–52.3)** | 0% | 10.0 | 13,712 | 241 s | |
| `uniform-mix`, tracing off | 19.4 | 28.0 | 38.1 | 0% | 10.0 | 13,817 | 242 s | pass |
| `zipf-mix` | 20.2 | 29.9 | 40.7 | 0% | 10.0 | 13,855 | 243 s | pass |
| `replay-storm` | 19.9 | 29.3 | 38.4 | 0% | 10.0 | 12,104 | 236 s | pass |
| `read-mix` | 1.1 | 21.9 | 27.8 | 0% | 10.0 | 431 | 37 s | pass |

Client-side latency from k6. At 30 rps the API used about 8% of its core
and PostgreSQL about 55% of one, a quarter of its two.

### Hot wallet (every request on one wallet)

| target rps | served rps | p50 ms | p99 ms | pool waits/s |
| --- | --- | --- | --- | --- |
| 30 | 29.0 | 17.8 | 43.2 | 0.0 |
| 50 | 49.0 | 18.9 | 49.1 | 0.0 |
| 60 | 58.8 | 23.3 | 233.7 | 3.1 |
| 80 | 75.5 | 351.2 | 1,090 | 274.4 |

### Inside one bet

A traced `BET` is one HTTP span with **11 SQL spans** beneath it. The
database commits about **4.3 times per transaction** (every autocommit
lookup counts), and its commit rate plateaus near **350 per second** once
the pool is full.

PostgreSQL spends about **18 ms of CPU per bet**: 32% of a core at 19 rps,
108% at 58, 171% at 78. Eleven statements that each touch a handful of
rows through an index should cost a small fraction of that.

The API's CPU profile at 30 rps (r1, 30 s, top 15 in
[`pprof-top.txt`](001-baseline-as-delivered/20261017T014806Z-uniform-mix-steady-r1/pprof-top.txt))
sampled 1.9 s of CPU, 6% of one core. The process mostly talks to
PostgreSQL:

| function (cumulative) | share of API CPU |
| --- | --- |
| `wager.(*Service).Submit` | 56% |
| `database/sql.withLock` (statements on a pooled connection) | 50% |
| `syscall.Syscall6`, flat (socket reads and writes) | 42% |
| `pgconn.(*PgConn).ExecStatement` | 34% |
| `pgproto3.(*Frontend).Flush` (sending one statement) | 29% |
| `runtime.findRunnable` (scheduler looking for work) | 11% |

A third of the API's CPU goes to writing statements to the database
socket one at a time; token verification (RSA) takes 3%.

## Analysis

1. **PostgreSQL's CPU is the first ceiling.** From 80 rps PostgreSQL uses
   both its cores while the API never passes a fifth of its own; the
   ten-connection pool fills because each statement waits for a core, not
   the other way round. At about 18 ms of database CPU per bet, two cores
   cannot serve much more than 100 bets a second. That cost is far too
   high for what a bet does, and [002](002-baseline-config-tuned.md)
   traces it to two lookups that read the whole `transactions` table.
   The predicted round-trip ceiling is real but sits behind this one.
2. **Throughput falls past the peak** because the cost per bet grows as
   the run goes on: the scans read every transaction stored so far, and
   each knee step adds thousands.
3. **There is no backpressure.** `database/sql` queues requests for a
   connection without limit, so overload turns into multi-second latency
   for everyone and *lower* throughput, instead of fast rejections for a
   few. Nothing tells a provider to slow down.
4. **The outbox caps events at exactly 10 per second at every load.**
   A request that moves money produces two events (`WagerTransactionProcessed`
   and `WalletBalanceChanged`), a `LOSS` or a rejection one, about 1.9 per
   request in this mix. At 30 rps about 57 events a second are produced
   against 10 published, so the backlog grows by about 46 a second: after
   five minutes 13,700 events are waiting and the oldest is four minutes
   old. Consumers of the events fall arbitrarily behind; the published
   rate does not move with the load.
5. **Hot rows are not the problem yet.** One wallet saturates at the same
   rate as 100,000 wallets: the row lock is held for a fraction of the
   request, and the shared bottleneck comes first. Contention will show
   once the scans and the round trips are gone.
6. **Idempotent replays cost no more than new bets** (`replay-storm`
   matches `uniform-mix`), and reads by wallet answer in about a
   millisecond.
7. **Tracing at 1% costs nothing measurable**: the control run sits inside
   the spread of the three traced runs.
8. **The guarantees held everywhere.** Every run, including the overloaded
   sweeps, passed all eight invariant checks: no negative balance, ledger
   and balance in agreement, no duplicate movement, an event for every
   committed transaction.

## Conclusion and next steps

As delivered, the service is correct under overload but sustains about
**40 bets per second within a 50 ms p99** and peaks near **82** on this
machine, with PostgreSQL's two cores saturated, and it **publishes 10
events per second** whatever the load.
[002](002-baseline-config-tuned.md) tests how much of this configuration
alone can fix, finds the cause of the database cost and sets the targets
for phases 2 and 3.
