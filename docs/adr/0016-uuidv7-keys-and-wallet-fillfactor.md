# 0016. Key new rows with UUIDv7 and leave room for HOT wallet updates

- Status: accepted
- Date: 2026-10-18

## Context

Every bet inserts a transaction, a ledger entry and one or two outbox
events, and updates one wallet. Their primary keys were random (version 4)
UUIDs, so each insert landed on a random leaf of each primary key index:
many pages touched, many page splits, and an index whose hot part is all
of it. A wallet update writes a new row version; if it does not fit on
the same page, every index on `wallets` gets a new entry too.

## Decision

- **UUIDv7** for every new key the service creates: wallets,
  transactions, ledger entries, events and inbox records. They are
  generated in Go (`uuid.NewV7`), since PostgreSQL 17 has no `uuidv7()`,
  and passed to the engine's functions. Their leading 48 bits are the
  creation time in milliseconds, so new rows go to the right edge of each
  index. Identifiers from clients stay any valid UUID, and existing data
  keeps its keys.
- **`fillfactor = 80` on `wallets`** (migration 12): a fifth of every page
  stays free, so the new version written by a balance update fits on the
  same page. No indexed column changes in such an update (balance,
  version and `updated_at` are not indexed), so PostgreSQL then writes no
  index entry at all: a heap-only tuple (HOT) update. Pruning reclaims the
  dead versions on the page as it fills.
- `idx_wallets_player_id` goes: the unique `(player_id, currency)` index
  serves every lookup by player.

## Consequences

- `TestBalanceUpdatesAreHOT` checks that 100 balance updates over seeded
  wallets are all HOT; with `fillfactor` 100 only 76 are. Benchmark 004
  reports the ratio under load (`n_tup_hot_upd / n_tup_upd`).
- `wallets` takes about a quarter more space; at 100,000 wallets that is
  a few megabytes.
- A UUIDv7 reveals when a row was created. Wallet and transaction IDs are
  shown only to authenticated providers and the internal service, and
  their creation time is in the same responses and events anyway.
- The transactions table keeps all its indexes in 2b; benchmark 004 lists
  the ones a run never scanned, for phase 2c to decide on.
