# 0003. Move balances with one conditional UPDATE

- Status: accepted
- Date: 2026-10-16

## Context

Two concurrent bets on the same wallet must never both read the old
balance. The usual options are `SELECT ... FOR UPDATE` followed by an
`UPDATE` (two round trips holding a lock in between), optimistic
versioning with retries (wasted work under contention) or an in-process
lock (wrong as soon as there are two instances).

## Decision

Every movement is a single statement:

```sql
UPDATE wallets
SET balance_minor_units = balance_minor_units - $1, version = version + 1, updated_at = NOW()
WHERE id = $2 AND currency = $3 AND balance_minor_units >= $1
RETURNING balance_minor_units, version
```

PostgreSQL locks the row and re-evaluates the `WHERE` against the latest
committed version, so there is no window between reading and writing.
Zero affected rows triggers a diagnostic read to tell an unknown wallet, a
currency mismatch and an insufficient balance apart. The schema also
enforces `CHECK (balance_minor_units >= 0)`.

## Consequences

No lost updates with one or many instances, no application locks, and
different wallets proceed in parallel. A single hot wallet is serialized
by its row lock, which is the contention the `hot-wallet` benchmark
measures. The surrounding flow still costs about eleven round trips per
bet (see the phase 1 baselines); reducing those is phase 2.
