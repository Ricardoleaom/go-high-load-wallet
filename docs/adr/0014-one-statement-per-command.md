# 0014. Run every command as one call to a PL/pgSQL function

- Status: accepted
- Date: 2026-10-18
- Supersedes: [0003](0003-atomic-conditional-wallet-update.md)

## Context

A bet cost about eleven round trips and four commits in phase 2a:
lookups, `BEGIN`, an insert, a conditional `UPDATE`, a ledger insert, a
status update, two outbox inserts and `COMMIT`
([benchmark 003](../benchmarks/003-fixes-measured.md)). PostgreSQL's CPU
was the ceiling at about 3.9 ms per bet, and each wallet stayed locked
for the whole exchange, so a single wallet serialized at about 240 bets a
second.

The roadmap called for one statement per command and left open whether
that statement is a chain of data-modifying CTEs or a PL/pgSQL function,
to be decided by measuring both.

## Decision

Each engine command is one call to a function of migration 13:
`wager_submit`, `wager_retry_reference` or `wallet_open`. One round trip,
one statement, one commit. Inside, in this order:

1. **Replay before any lock.** The transaction under the key, or under the
   external ID, is found in one statement (`wager_find`, so both lookups
   see one snapshot): the same key replays, the same external ID under
   another key is a key mismatch.
2. **One lock order everywhere:** a pending reversal's own row, then the
   original a reversal names, then the wallet, locked
   `FOR NO KEY UPDATE` (which does not block the `KEY SHARE` locks foreign
   keys take). A fresh reversal only reads the wallet before locking the
   original, so it cannot wait on the retry worker in a cycle.
3. **Decide on the locked row**, then insert the transaction directly in
   its final status with `ON CONFLICT DO NOTHING`. A submission that lost
   a race on the key has moved nothing and answers with the winner's
   result in the same call (no `EXCEPTION` block, whose subtransaction
   would cost every call).
4. **Then move:** the wallet update, the ledger entry and the events,
   built with `jsonb_build_object` in the same envelope as before.
   `clock_timestamp()` is taken after the wallet lock and used for the
   transaction, the entry and the events, so a wallet's ledger reads in
   the order its movements were applied.

The engine validates a command in Go before calling (ADR 0013); the SQL
trusts its input.

**Why a function and not a CTE.** A CTE cannot lock the wallet and then
decide on what it locked as a separate step, cannot answer a lost race in
the same statement (it would need a second round trip), and does not fit
reversals at all, which need the original locked before the wallet. So
the CTE was written for the bet alone, checked against the function
(`TestBetStatementVariantsAgree`), and measured inside PostgreSQL, one
call running N commands, which leaves out the network and the WAL flush
that cost both forms the same (`BenchmarkBetStatementInServer`, 10,000
seeded wallets, eight runs per form in alternating order, medians):

| | function | CTE |
| --- | --- | --- |
| fresh bet | 983 µs (857–1,108) | 960 µs (918–1,177) |
| replay of a bet | 195 µs (186–217) | 384 µs (355–422) |

The CTE saves 2% on a fresh bet, within the noise, and costs twice as
much on a replay. Measured end to end with durable commits, both forms
take 2 to 3 ms per serial bet on this machine, dominated by the WAL flush
and the round trip through Docker, with more spread between runs than
between the forms. The function stays.

## Consequences

- A bet holds its wallet's lock for one statement instead of a
  multi-statement exchange, and pays one commit.
- The business rules exist twice, in the domain (run by the reference
  model) and in SQL. The contract suite, the property test against the
  model and the linearizability check are what keep them equal; they
  caught two differences while this was built.
- Measuring inside PostgreSQL found two costs the end-to-end numbers hid,
  fixed before release: lookups planned on the wrong index when
  statistics lagged (migration 14 gives each lookup an index of its own),
  and helper functions planned again on every call (a `STRICT` SQL
  function is not inlined; a SQL function with an `INSERT` never is).
  Together they had made a bet cost 2.3 ms and a replay 1.1 ms inside the
  server.
- Behind PgBouncer in transaction mode a function call is one statement,
  so it pools well; prepared statements are phase 2c's decision.
- The functions are `SECURITY INVOKER`, so a caller can do through them
  only what it could do directly.
