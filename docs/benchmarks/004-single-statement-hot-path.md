# 004. The single-statement hot path

- Date: 2026-10-18
- Commit: `1870054` (the knees ran on `23b4817`, which differs from it only in a test and `.gitignore`) (phase 2b: every command one call to a PL/pgSQL function, behind the `Engine` port)
- Raw results: [`004-single-statement-hot-path/`](004-single-statement-hot-path/); every table below comes from
  `python3 bench/summarize.py docs/benchmarks/004-single-statement-hot-path/2026*`

## Question

[003](003-fixes-measured.md) traced the remaining ceiling to the hot path
itself: about eleven statements and four commits per bet, about 3.9 ms of
PostgreSQL CPU each, and a wallet locked for the whole exchange. Phase 2b
makes every command one call to a PL/pgSQL function
([ADR 0014](../adr/0014-one-statement-per-command.md)): one round trip,
one commit, the wallet locked for one statement.

Does that cut PostgreSQL's CPU and commits per bet, move the knee and the
hot-wallet ceiling, and keep every invariant? It is refuted if commits per
bet stay above one, if PostgreSQL's CPU per bet does not fall, if
the knee does not move past 003's 150 rps, or if any run breaks an
invariant.

## Environment

The machine, pinning, images and seeded data of 001 to 003: an Intel Core
i5-10300H with 4 vCPUs under WSL2, PostgreSQL on CPUs 0-1, the API on CPU 2,
k6 on CPU 3, 100,000 wallets, default settings (pool 10/2, outbox 20 every
2 s, tracing at 1%, `REQUEST_TIMEOUT` 5 s, `DB_LOCK_TIMEOUT` 2 s).

## Method

[ADR 0009](../adr/0009-benchmark-methodology.md), one fresh stack per run.

1. `uniform-mix` knee from 100 to 1,200 rps in steps of 100, and
   `hot-wallet` knee from 50 to 600 in steps of 50, 60 s each.
2. `uniform-mix` steady at 320 rps (80% of the knee), 5 minutes, three
   runs; `replay-storm` and `read-mix` at the same rate.
3. Overload: `uniform-mix` steady at 1,500 rps for 3 minutes, about 1.5
   times the highest rate served.
4. Contention: `hot-wallet` steady at 450 rps (1.5 times its knee) for
   2 minutes.

New in this report, from the runner: PostgreSQL CPU and commits per bet
served, the share of wallet updates that were HOT, and the
indexes a run never scanned, read from PostgreSQL's own counters.

## Results

### Where it breaks now (`uniform-mix` knee)

| target rps | server rps | p50 ms | p99 ms | 5xx/s | dropped/s | pool waits/s | API CPU cores | CPU % app/pg/k6 | PG CPU ms/bet | commits/bet |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 100 | 95.2 | 2.8 | 6.5 | 0.00 | 0.0 | 0.0 | 0.07 | 7/17/8 | 1.83 | 1.01 |
| 200 | 192.4 | 3.5 | 33.6 | 0.00 | 0.0 | 1.2 | 0.15 | 15/36/16 | 1.88 | 1.01 |
| 300 | 292.3 | 5.8 | 54.1 | 0.00 | 0.0 | 7.8 | 0.20 | 19/46/23 | 1.57 | 1.02 |
| 400 | 392.4 | 3.2 | 29.9 | 0.00 | 0.0 | 4.5 | 0.25 | 26/69/31 | 1.76 | 1.01 |
| 500 | 492.4 | 3.5 | 62.4 | 0.00 | 0.0 | 24.6 | 0.31 | 35/103/39 | 2.10 | 1.01 |
| 600 | 588.6 | 5.4 | 299.0 | 0.00 | 6.8 | 162.1 | 0.35 | 36/105/43 | 1.78 | 1.00 |
| 700 | 691.5 | 5.0 | 190.5 | 0.00 | 0.0 | 200.1 | 0.40 | 42/130/46 | 1.87 | 1.00 |
| 800 | 752.7 | 27.8 | 756.9 | 0.00 | 56.0 | 452.2 | 0.40 | 42/138/47 | 1.83 | 1.00 |
| 900 | 888.4 | 26.0 | 280.2 | 0.00 | 15.7 | 590.0 | 0.45 | 46/156/52 | 1.75 | 1.00 |
| 1000 | 908.6 | 175.9 | 651.7 | 0.00 | 103.9 | 843.1 | 0.41 | 43/154/45 | 1.69 | 1.01 |
| 1100 | 970.0 | 330.9 | 496.6 | 0.00 | 118.2 | 970.7 | 0.42 | 42/161/51 | 1.66 | 1.00 |
| 1200 | 834.1 | 378.5 | 1428.5 | 0.00 | 353.4 | 834.9 | 0.37 | 37/145/43 | 1.73 | 1.03 |

Server-side, per step after the ramp; CPU is a percentage of one core
(PostgreSQL has two). No step returned a 5xx.

- **Knee: 400 rps** by the 50 ms rule. The 300 rps step read 54 ms, in the
  window where a timed checkpoint started; the steps on either side stayed
  under 34 ms.
- **The service serves up to 970 rps** (at 1,100 offered), with PostgreSQL
  at 161% of a core. Past 800 rps the ten-connection pool queues: up to
  970 acquires a second wait for a connection.
- **One commit per bet** at every rate (1.00 to 1.03), and about **1.6
  to 2.1 ms of PostgreSQL CPU per bet**, against about 3.9 in 003.
- From about 700 rps the checkpoints are triggered by WAL volume
  (`checkpoint starting: wal`) every two to two and a half minutes: the
  default `max_wal_size` is too small for this write rate.

### A single wallet (`hot-wallet` knee)

| target rps | server rps | p50 ms | p99 ms | 5xx/s | dropped/s | pool waits/s | API CPU cores | CPU % app/pg/k6 | PG CPU ms/bet | commits/bet |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 50 | 47.1 | 2.9 | 6.7 | 0.00 | 0.0 | 0.0 | 0.04 | 4/9/4 | 1.94 | 1.16 |
| 100 | 95.4 | 2.1 | 5.0 | 0.00 | 0.0 | 0.0 | 0.07 | 7/15/7 | 1.62 | 1.07 |
| 150 | 145.4 | 2.1 | 6.4 | 0.00 | 0.0 | 0.1 | 0.10 | 10/23/10 | 1.56 | 1.04 |
| 200 | 195.4 | 2.1 | 7.9 | 0.00 | 0.0 | 0.1 | 0.12 | 12/31/14 | 1.61 | 1.06 |
| 250 | 245.3 | 2.3 | 59.0 | 0.00 | 0.0 | 2.9 | 0.16 | 14/37/15 | 1.50 | 1.03 |
| 300 | 295.4 | 2.0 | 44.0 | 0.00 | 0.0 | 3.7 | 0.18 | 17/42/18 | 1.43 | 1.03 |
| 350 | 281.3 | 418.7 | 1948.7 | 0.00 | 83.3 | 115.8 | 0.17 | 19/46/19 | 1.64 | 1.02 |
| 400 | 395.4 | 2.4 | 61.0 | 0.00 | 0.0 | 13.4 | 0.24 | 24/67/33 | 1.69 | 1.03 |
| 450 | 445.0 | 4.4 | 123.2 | 0.00 | 0.0 | 105.6 | 0.27 | 27/79/36 | 1.77 | 1.02 |
| 500 | 420.0 | 635.8 | 1679.3 | 0.00 | 78.7 | 409.2 | 0.25 | 24/95/31 | 2.27 | 1.02 |
| 550 | 471.3 | 750.0 | 995.0 | 0.00 | 73.9 | 472.1 | 0.27 | 27/100/37 | 2.11 | 1.01 |
| 600 | 473.6 | 750.0 | 995.0 | 0.00 | 121.9 | 474.4 | 0.27 | 27/98/38 | 2.06 | 1.02 |

- **Knee: 300 rps** (p99 44 ms). One wallet now takes about 450 bets a
  second before latency climbs (445 served at a 4.4 ms p50), and under
  overload it keeps serving about 470, where 003's fell from 240 to 128.
  The server-side quantiles of the last two steps are interpolated inside
  the histogram's 0.5 to 1 s bucket, hence the round 750 and 995 ms.
- The 350 rps step stalled for its whole minute (p50 419 ms) and the next
  step was back to a 2.4 ms p50. A timed checkpoint was writing through
  that window, as through the calm steps before it, and completed three
  seconds after it; PostgreSQL used less than half a core, and the
  runner's log does not record autovacuum yet. It is the kind of stall 002 and 003
  saw, longer this time, and left to phase 2c with autovacuum logging.
- Under overload the excess is not shed: latency grows to the request
  budget, and k6 drops what it cannot send. Admission control is 2c.

### What PostgreSQL counted

| run | wallet updates | HOT % | transactions seq scans | indexes never scanned |
| --- | --- | --- | --- | --- |
| 20261018T202125Z-uniform-mix-knee | 419627 | 100 | 26 | inbox_messages_pkey, inbox_messages_unique, ledger_entries_pkey, schema_migrations_pkey, idx_transactions_single_opening, idx_transactions_wallet_created, idx_wallets_player_currency_unique |
| 20261018T203757Z-hot-wallet-knee | 231134 | 99 | 26 | inbox_messages_pkey, inbox_messages_unique, ledger_entries_pkey, schema_migrations_pkey, idx_transactions_reference, idx_transactions_single_opening, idx_transactions_wallet_created, idx_wallets_player_currency_unique |
| 20261018T205827Z-uniform-mix-steady-r1 | 88170 | 100 | 26 | inbox_messages_pkey, inbox_messages_unique, ledger_entries_pkey, schema_migrations_pkey, idx_transactions_single_opening, idx_transactions_wallet_created, idx_wallets_player_currency_unique |
| 20261018T210644Z-uniform-mix-steady-r2 | 87972 | 100 | 26 | inbox_messages_pkey, inbox_messages_unique, ledger_entries_pkey, schema_migrations_pkey, idx_transactions_single_opening, idx_transactions_wallet_created, idx_wallets_player_currency_unique |
| 20261018T211451Z-uniform-mix-steady-r3 | 88272 | 100 | 26 | inbox_messages_pkey, inbox_messages_unique, ledger_entries_pkey, schema_migrations_pkey, idx_transactions_single_opening, idx_transactions_wallet_created, idx_wallets_player_currency_unique |
| 20261018T212224Z-replay-storm-steady | 79469 | 100 | 26 | inbox_messages_pkey, inbox_messages_unique, ledger_entries_pkey, schema_migrations_pkey, idx_transactions_single_opening, idx_transactions_wallet_created, idx_wallets_player_currency_unique |
| 20261018T212953Z-read-mix-steady | 17906 | 100 | 26 | inbox_messages_pkey, inbox_messages_unique, ledger_entries_pkey, schema_migrations_pkey, idx_transactions_single_opening, idx_transactions_wallet_created, idx_wallets_player_currency_unique |
| 20261018T213720Z-uniform-mix-steady-overload | 173697 | 100 | 26 | inbox_messages_pkey, inbox_messages_unique, ledger_entries_pkey, schema_migrations_pkey, idx_transactions_single_opening, idx_transactions_wallet_created, idx_wallets_player_currency_unique |
| 20261018T214250Z-hot-wallet-steady-contention450 | 56533 | 99 | 26 | inbox_messages_pkey, inbox_messages_unique, ledger_entries_pkey, schema_migrations_pkey, idx_transactions_reference, idx_transactions_single_opening, idx_transactions_wallet_created, idx_wallets_player_currency_unique |

Every wallet update in the `uniform-mix` knee was HOT, and 99% in the
`hot-wallet` one, where a single row is updated hundreds of times a
second. The 26 sequential scans of `transactions` are the same in every
run because none of them comes from the load: on a fresh database the
migrations make 14, the seed 8 and the verification before the load 4
(counted with `pg_stat_user_tables` after each step). `idx_transactions_wallet_created`
was never used by any run, and the `inbox_*` indexes only serve the SQS
path, which these scenarios do not use: phase 2c weighs dropping the
first.

### The outbox relay, before and after migration 15

The first knees of this report ran before migration 15
([`before-outbox-index/`](004-single-statement-hot-path/before-outbox-index/)).
Their PostgreSQL log showed the relay's claim cancelled by the 5 s
statement timeout on every round from the eighth minute on: claiming the
oldest due events sorted the whole pending backlog, which at about 1,500
new events a second had passed half a million. The relay ended that run
publishing **nothing**, with 808,631 events pending, and the sort cost
PostgreSQL CPU the bets needed (2 to 3.3 ms per bet against 1.6 to 2.1
after). Indexed by age, the claim stops at the batch: the relay kept its
10 events a second to the end of the final run, with no timeout. That is
still the relay's ceiling while a bet produces two events; phase 3
replaces it.

Each log also records eight or nine `lock timeout` cancellations, all
before the load starts: the relay's claim and the backlog gauge waiting
on the seed's `TRUNCATE`.

### Steady state at 320 rps

| run | p50 ms | p95 ms | p99 ms | failed | CPU % API/PostgreSQL | PG CPU ms/bet | commits/bet | verify |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `uniform-mix` r1 | 3.1 | 9.3 | 42.3 | 0% | 18/49 | 1.54 | 1.03 | pass |
| `uniform-mix` r2 | 3.2 | 15.1 | 42.0 | 0% | 18/47 | 1.48 | 1.03 | pass |
| `uniform-mix` r3 | 3.1 | 8.0 | 23.6 | 0% | 18/50 | 1.57 | 1.03 | pass |
| **median (range)** | **3.1 (3.1–3.2)** | **9.3 (8.0–15.1)** | **42.0 (23.6–42.3)** | 0% | | | | |
| `replay-storm` | 3.1 | 8.5 | 36.3 | 0% | 18/48 | 1.49 | 1.03 | pass |
| `read-mix` | 0.8 | 3.8 | 6.3 | 0% | 15/19 | 2.93 | 5.04 | pass |

Client-side, from k6, at 2.7 times 003's steady rate (120 rps). The
per-bet columns divide by bets served; `read-mix` sends one bet in five
requests, so its 5.04 commits and 2.93 ms per bet are 1.02 commits and
0.6 ms of PostgreSQL CPU per request. k6 dropped 33 to 84 of about 96,000
iterations per `uniform-mix` run.

### Overload and contention

| run | offered rps | served rps (lowest–highest 30-s rate) | p50 ms | p99 ms | 5xx | lowest commits/s | verify |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `uniform-mix` | 1,500 | 1,021 (835–1,092) | 274 | 543 | 0 | 848 | pass |
| `hot-wallet` | 450 | 448 (440–454) | 15.3 | 283 | 0 | 447 | pass |

The API's counters at the end of each run, warmup included: 193,089 and
56,533 responses `200`, nothing else
([`metrics-after.txt`](004-single-statement-hot-path/20261018T213720Z-uniform-mix-steady-overload/metrics-after.txt)).
Under overload k6 dropped 85,911 of the 270,000 iterations it was asked
for: what the service could not take waited in k6, not in the API.

### Compared with 001 to 003

| `uniform-mix` | 001, as delivered | 002, same code with two indexes | 003, fixed | 004, one statement |
| --- | --- | --- | --- | --- |
| knee (p99 under 50 ms) | 40 rps | 100 rps | 150 rps | **400 rps** |
| p50 at the knee | 18.0 ms | 4.7 ms | 4.5 ms | **3.2 ms** |
| highest served rate | 82 rps, then falling | 443 rps (last step) | 495 rps (last step); 568 at 750 offered | **970 rps (at 1,100 offered); 1,021 at 1,500 offered** |
| PostgreSQL CPU per bet | about 18 ms | about 4.2 ms | about 3.9 ms | **1.5 to 2.1 ms** |
| commits per bet | | | about 4 | **1.00 to 1.03** |
| under overload | throughput collapses, minute-long waits, deadlock | deadlock at 500 rps | no stall; served rate stays at the peak | **no stall, no 5xx; served rate stays at the peak** |
| `hot-wallet` knee | about 50 rps | not measured | 150 rps | **300 rps; 448 served at 450 offered** |

## Analysis

1. **The one statement did what it was for.** Every bet is one commit
   (the 1 to 3% above one are the outbox relay's own, about ten a second),
   PostgreSQL spends 1.5 to 2.1 ms of CPU on it against 3.9, and every
   wallet update is HOT. None of the four refutation conditions holds:
   commits per bet are one, CPU per bet fell by about half, the knee moved
   from 150 to 400 rps, and every run passed every invariant check.
2. **The service now serves about 1,000 bets a second**, twice 003's
   peak, with PostgreSQL at 160% of its two cores. At about 1.7 ms of CPU
   per bet those two cores cap it near 1,200, so the next 2x needs a
   cheaper bet, not only a better queue.
3. **The 50 ms rule breaks on the tail, not on throughput.** The median
   stays under 6 ms up to 700 rps; the p99 crosses 50 ms from 500 rps,
   where requests start waiting for one of the ten pool connections
   (25 waits a second at 500 rps, 162 at 600), and on the steps a
   checkpoint runs through. Phase 2's target, 1,000 rps within a 50 ms
   p99, is not met yet: 2c sizes the pool against PostgreSQL's cores,
   spreads checkpoints with a larger `max_wal_size`, and adds admission
   control so that excess load gets a fast 503 instead of a queue.
4. **A single wallet takes about 450 bets a second**, about twice 003's
   240, and its knee moved from 150 to 300 rps: the lock is held for one statement, so the 450 rps contention run
   served 448 at a 15 ms median with no 503, where 003 served 236 of 350
   at 662 ms. The one-minute stall at 350 rps in the knee did not recur in
   the two-minute run at 450 and stays open for 2c, with autovacuum
   logging.
5. **Overload degrades without breaking.** At 1,500 rps offered the
   service kept committing at least 848 times a second, served about
   1,000 bets a second to the end and answered nothing but `200`. Latency
   still grows to half a second and the excess waits in the client:
   nothing sheds it yet.
6. **Replays and reads are cheap.** `replay-storm`, whose repeated sends
   answer before any lock, reads like `uniform-mix` (p99 36 ms, 1.49 ms
   per bet); a read is one commit and about 0.6 ms of PostgreSQL CPU.
7. **The outbox is still 10 events a second**, so its backlog reached
   180,000 events in five minutes at 320 rps and 354,000 under overload.
   Migration 15 keeps that from stalling the relay; phase 3 replaces it.
8. **Every run passed all eight invariant checks**, overload and
   contention included.

## Conclusion and next steps

With every command one statement, the service reaches 400 rps within a
50 ms p99, serves about 1,000 bets a second at saturation and keeps doing
so under 1.5 times that, with no 5xx and every invariant intact; a single
wallet takes about 450 bets a second. Phase 2's target of 1,000 rps within
50 ms is not met: the tail breaks first, on pool waits and checkpoints,
and the throughput ceiling is PostgreSQL's CPU at about 1.7 ms per bet.
Phase 2c works on both:

- admission control: a bounded wait for a connection, then 503 with
  `Retry-After`;
- the pool sized against PostgreSQL's cores, and PgBouncer in transaction
  mode in front of it;
- PostgreSQL settings for this write rate (`max_wal_size`, checkpoint
  spreading, WAL compression), with checkpoint and autovacuum logging to
  explain the stall at 350 rps;
- where the 1.7 ms per bet goes, from `pg_stat_statements`, and whether
  `idx_transactions_wallet_created`, which no run used, can go.
