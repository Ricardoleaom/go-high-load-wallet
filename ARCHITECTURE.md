# Architecture

How the service works as of phase 2b (unreleased), and why. The reasoning behind the
larger choices lives in the [decision records](docs/adr/README.md); where
the service is heading lives in the [roadmap](ROADMAP.md).

## 1. Overview

One Go binary (`cmd/api`) runs the HTTP API and three background workers:
the SQS consumer, the outbox relay and the pending-reference retry worker.
Every instance runs all four; they coordinate only through PostgreSQL, so
any number of instances can run side by side.

```
providers ──HTTP──► httpapi ─┐                 ┌─► engine ──one call──► PostgreSQL
providers ──SQS───► sqsx ────┴─► wagering ─────┤                        (wallets, transactions,
                                               └─► queries ◄────────────  ledger, inbox, outbox)
                    relay (outbox ──► SQS events queue)
                    retry worker (PENDING_REFERENCE ──► engine)
Keycloak ──JWKS──► httpapi        admin port ──► Prometheus ─► Grafana ◄─ Jaeger (OTLP traces)
```

The code follows a hexagonal layout
([ADR 0013](docs/adr/0013-hexagonal-layout-and-engine-port.md)): the
domain holds the rules, the app layer the use cases and the ports they
need, adapters plug transports and storage into those ports, and only
`cmd/` composes them with Fx. `internal/archtest` checks the dependency
rule on every import.

| Package | Role |
| --- | --- |
| `internal/domain/{money,ids,wallet,wager,ledger}` | Pure domain types and rules; no I/O, no frameworks |
| `internal/app/engine` | The `Engine` port every money movement goes through, its reference model (`model`) and contract suite (`enginetest`) |
| `internal/app/wagering` | The use case HTTP and SQS share: validation, identity, payload hash, retries of pending reversals |
| `internal/app/queries`, `internal/app/relay`, `internal/app/events` | The read-side port, the outbox relay and its ports, the event envelope |
| `internal/adapters/postgres` | pgx pool, the engine (one function call per command), reads, outbox, inbox, reference queue |
| `internal/adapters/httpapi` | HTTP handlers, authentication with a verified-token cache, request metrics |
| `internal/adapters/sqsx` | SQS client, consumer and the relay's sink |
| `internal/platform/{config,observability,lifecycle,buildinfo}` | Configuration, metrics and tracing, start and stop of servers and workers, version |
| `internal/bench/{seed,verify}` | Benchmark seeding and invariant checks (`cmd/seed`, `cmd/verify`) |
| `internal/testsupport/*` | Containers and fakes for self-contained tests |
| `cmd/api` | Fx composition |

## 2. Money (`internal/domain/money`)

`Money` is immutable and stores an `int64` amount in minor units (cents)
plus an ISO 4217 currency code (three upper-case letters). It never goes
through `float32`/`float64`: parsing, arithmetic, serialization and
persistence (`BIGINT` columns) use only integers and strings.

`New(amount, currency)` accepts only digits, one dot and exactly two
decimal places (`"25.00"`). It rejects empty input, signs, scientific
notation, `NaN`, `Infinity`, any other scale and any other character, and
never rounds. Leading zeros are the only equivalent form accepted
(`"025.00"` equals `25.00`); before hashing for idempotency the amount is
re-formatted from its minor units, so both forms produce the same hash.

`Add`, `Subtract`, `Negate` and `Compare` require equal currencies
(`ErrCurrencyMismatch`) and check `int64` overflow explicitly. The limit
is `9223372036854775807` minor units. Negative values only arise inside
the service (a reconciliation difference, for example), never from input,
and a wallet balance cannot go negative (section 5). See
[ADR 0002](docs/adr/0002-money-as-int64-minor-units.md).

## 3. Database access: one call per command

pgx's own pool (`pgxpool`), hand-written SQL, no ORM; versioned
`golang-migrate` migrations in `migrations/` (each with `up` and `down`),
embedded in the `migrations` package so tests apply exactly the same
files. `postgres.Open` keeps 10 connections at most and 2 at least,
recycles each after 30 minutes (with 3 minutes of jitter), traces every
statement on request, and, when a request's context ends mid-statement,
asks PostgreSQL to cancel it rather than closing the connection
([ADR 0015](docs/adr/0015-pgxpool-and-server-side-cancellation.md)).

Every command the engine runs is **one call to a PL/pgSQL function**
(migration 13): `wager_submit`, `wager_retry_reference` or `wallet_open`.
One round trip, one statement, one commit; what a command writes commits
together or not at all
([ADR 0014](docs/adr/0014-one-statement-per-command.md)):

| Operation | Committed together |
| --- | --- |
| Opening with balance > 0 | wallet, `OPENING` as `PROCESSED`, ledger credit, `WagerTransactionProcessed` and `WalletBalanceChanged` |
| Opening with zero balance | the wallet only |
| `BET`/`WIN` processed | `PROCESSED` transaction, conditional wallet `UPDATE`, ledger entry, `WagerTransactionProcessed` and `WalletBalanceChanged` |
| `BET` without balance or with another currency | the `REJECTED` transaction and `WagerTransactionRejected` |
| `LOSS` | `PROCESSED` transaction and `WagerTransactionProcessed` (wallet and ledger untouched) |
| `REFUND`/`ROLLBACK` without its reference | `PENDING_REFERENCE` transaction, the retry schedule and `WagerTransactionPendingReference` |
| `REFUND`/`ROLLBACK` applied | lock on the original transaction, `PROCESSED` reversal, wallet, ledger entry and both events |

A traced `BET` shows one SQL statement under its HTTP span, where phase 2a
showed eleven (`TestEveryCommandIsOneStatement` counts them). Reads
(`queries.Reader`) are one statement each too: a transaction comes back
with the balance it left in the same query.

## 4. WagerTransaction and its state machine (`internal/domain/wager`)

Kinds: `OPENING`, `BET`, `WIN`, `LOSS`, `REFUND`, `ROLLBACK`
(`Kind.Known()` tells them apart from anything else a client sends).
Amount rules are validated in the constructor: `OPENING`, `BET` and `WIN`
need an amount above zero; `LOSS` exactly `0.00`; `REFUND` and `ROLLBACK`
an amount above zero and a `referenceExternalTransactionId`.

```
PENDING ──> PROCESSED
   │    ──> REJECTED
   │    ──> FAILED
   └──> PENDING_REFERENCE ──> PROCESSED | REJECTED
```

Only `PENDING` and `PENDING_REFERENCE` can transition; `PROCESSED`,
`REJECTED` and `FAILED` are terminal and any further transition returns
`ErrInvalidTransition`. `New()` always starts in `PENDING`; `Rehydrate()`
rebuilds what came from the database without re-applying movements,
transitions or events, so loading a transaction for a `GET` or a replay
has no side effect.

**`PENDING` is never stored.** Processing is synchronous: the engine
inserts a transaction directly in its final state (or in
`PENDING_REFERENCE`) in the same statement as the operation, so there is
no orphan `PENDING` for another instance to pick up. The state machine
above is the domain's, which the reference model runs in memory. The only
durable wait is `PENDING_REFERENCE`, which has its own worker (section 8).

**Transient versus permanent failures.** Infrastructure failures
(PostgreSQL or SQS down, timeouts, unexpected errors) leave no record: the
transaction rolls back, nothing commits, and the error reaches the caller.
HTTP clients get a 500 (or a 503 `TIMEOUT`) and can safely resend with
the same `Idempotency-Key`; SQS messages are not deleted and come back
(section 10). Business rejections are final and stored as `REJECTED` with a
`failureCode` (section 12). `FAILED` exists in the domain but the service
does not store it; messages that exhaust their SQS attempts end in the DLQ.

A transaction keeps its reference in two columns: `reference_external_id`
(the `referenceExternalTransactionId` as sent) and `reference_id` (the
internal ID of the original, filled once resolved).

`OPENING` is internal. The HTTP handler rejects `kind: "OPENING"` with 400
and `wagering.Validate` returns `ErrOpeningNotAllowed` as a second
barrier, which also covers SQS. `OPENING` carries no provider, external IDs, key, hash,
round or game; the `transactions_external_fields_required` constraint
(migration `000008`) requires them for every other kind, and the unique
index `idx_transactions_single_opening` prevents a second initial credit.

## 5. Wallet and concurrency (`internal/domain/wallet`)

The wallet is the aggregate root: identity, player, currency, balance,
version and timestamps. `(playerId, currency)` is unique
(`idx_wallets_player_currency_unique`); a second opening for the same pair
answers 409. `version` starts at 1 and only increments when the balance
changes, so `LOSS` leaves it alone. The schema enforces
`CHECK (balance_minor_units >= 0)`, `CHECK (version >= 1)` and the
currency format.

A movement happens inside the engine's function, which locks the wallet
row (`SELECT ... FOR NO KEY UPDATE`), decides on the locked values
(unknown wallet or another player's, currency, balance), inserts the
transaction and then updates the balance and version. The lock is held for
that one statement and released by its commit, so two concurrent bets
never decide on the same balance, and different wallets proceed in
parallel without a global or an application lock, with one instance or
many. `FOR NO KEY UPDATE` leaves the `KEY SHARE` locks of the foreign keys
to the wallet free.

**One lock order.** A pending reversal's own row, then the original a
reversal names, then the wallet. A fresh reversal only reads the wallet
before locking the original, so it never holds the wallet while waiting
for a row the retry worker holds; inverting that order produces
`40P01` deadlocks in `TestFreshRollbackRacesTheRetryWorkerWithoutDeadlock`.

Tests against a real PostgreSQL, with the race detector:

- the engine contract (`internal/app/engine/enginetest`, 37 cases) runs
  against PostgreSQL and the reference model, concurrency cases included:
  20 concurrent 30.00 bets on 100.00 process exactly three, 50 concurrent
  duplicates move money once, ten keys for one external ID apply once, ten
  refunds of one bet apply once;
- `TestConcurrentBetsAcrossThreeIndependentPools`,
  `TestDuplicatesAcrossThreeIndependentPoolsMoveMoneyOnce` and
  `TestDistinctWalletsProcessConcurrently` act as three instances;
- `TestEngineIsLinearizable` checks a history of 320 concurrent bets,
  wins, resends and reads on one wallet with porcupine.

## 6. Idempotency

An operation's identity is `(provider_id, idempotency_key)`, with a partial
unique index (`idx_transactions_idempotency_unique`, excluding `OPENING`).
`(provider_id, external_id)` has another, so the same financial operation
cannot be applied again under a different key. Both live in
`transactions`, so idempotency survives restarts of every process.

**Payload hash** (`ComputePayloadHash`,
`internal/app/wagering/payload_hash.go`): SHA-256, hex encoded, of
the JSON of a struct with fields in a fixed alphabetical order — `amount`,
`currency`, `externalId`, `gameId`, `kind`, `playerId`, `providerId`,
`referenceExternalTransactionId` (omitted when empty), `roundId`,
`walletId`. The idempotency key and any transport metadata (HTTP headers,
SQS envelope, `messageId`) stay out. HTTP and SQS build the same
`SubmitInput`, so the same operation hashes the same through either.

`wagering.Submit` validates the whole submission first (IDs, field
lengths, the amount rules of its kind), so a request that can never
succeed answers 400 before anything is looked up. The engine then, in one
call:

1. finds the transaction under `(providerId, idempotencyKey)` or, failing
   that, under `(providerId, externalTransactionId)`, in one statement so
   both see the same snapshot: the same key with the same hash replays the
   stored result with `idempotentReplay: true`; the same key with another
   hash is `IDEMPOTENCY_CONFLICT` (409); the external ID under another key
   is `IDEMPOTENCY_KEY_MISMATCH` (409);
2. otherwise processes it as a new operation.

A replay of a `PROCESSED` transaction returns the balance recorded in its
ledger entry, not the wallet's current balance. Two concurrent first
submissions of the same key can both pass the lookup; the transaction is
inserted with `ON CONFLICT DO NOTHING` before any money moves, so the
loser inserts nothing, finds the winner and answers with its result in the
same call (`wager_concurrency_conflicts_total` counts it).

The two unique indexes are led by their own column,
`(idempotency_key, provider_id)` and `(external_id, provider_id)`
(migration 14), so each lookup has exactly one index to use even when the
statistics do not know a provider's rows yet; led by `provider_id`, either
could serve the other's lookup and read every transaction of the provider.
The server never replaces the `Idempotency-Key` it receives; the header
is mandatory (400 without it). See
[ADR 0004](docs/adr/0004-idempotency-unique-index-and-payload-hash.md).

## 7. Ledger (`internal/domain/ledger`)

`ledger.Entry` is immutable and its constructor checks
`balanceAfter = balanceBefore ± amount` according to the direction
(`DEBIT` or `CREDIT`). In `ledger_entries`:

- `UNIQUE (transaction_id)`: at most one entry per transaction;
- `CHECK`s on a non-zero amount, non-negative before and after balances
  and a valid direction;
- the `ledger_entries_immutable` trigger refuses any `UPDATE` or `DELETE`:
  append-only enforced by the database, not by convention.

`amount_delta_minor_units` is signed (negative for debits), which makes
reconciliation a plain sum. `LOSS` and rejected transactions create no
entry; corrections are new entries (a `ROLLBACK`, for example).

`GET /wallets/{id}/ledger` orders by `(created_at, id)`, stable even with
equal timestamps, and pages with an opaque cursor (`base64url` of
`created_at|id`), `limit` 50 by default and 500 at most. The engine takes
an entry's `created_at` after it locks the wallet, so a wallet's entries
read in the order they were applied: each `balance_before` is the previous
`balance_after` (`TestLedgerPagesWithoutGapsOrRepeats`). Phase 4 keys the
ledger by `(wallet_id, version)` as well.

## 8. Pending references and reversals

`REFUND` and `ROLLBACK` resolve their reference by
`(providerId, referenceExternalTransactionId)`:

| Reference | Result |
| --- | --- |
| Does not exist yet | `PENDING_REFERENCE` (202) |
| Exists but is `PENDING_REFERENCE` | `PENDING_REFERENCE`, waits too |
| Ended `REJECTED` or `FAILED` | rejected now with `REFERENCE_NOT_PROCESSED` |
| Differs in provider, player, wallet, round, currency or amount | `REFERENCE_MISMATCH` |
| Kind cannot be reversed | `REFERENCE_KIND_NOT_REVERSIBLE` |
| Already has a successful reversal | `REFERENCE_ALREADY_REVERSED` |
| `PROCESSED` and compatible | the reversal is applied |

`REFUND` only reverses a `BET` (credits it back). `ROLLBACK` reverses a
`BET` (credit), a `WIN` or a `REFUND` (debit). There are no partial
reversals: the amount must equal the original's.

**Retry.** A pending transaction gets `reference_next_attempt_at` with
exponential backoff (5 s, 10 s, 20 s, 40 s… capped at 5 minutes; the rule
is `wager.ReferenceBackoff`). The `RetryWorker` runs every
`REFERENCE_RETRY_INTERVAL` (15 s), claims up to 20 due rows and hands each
to the engine as a `RetryReference`; after 8 attempts (about 15 minutes in
total) the engine rejects it with `REFERENCE_NOT_FOUND_TIMEOUT` and
publishes `WagerTransactionRejected`. Claims are atomic
(`ReferenceQueue.ClaimDue`: `UPDATE ... WHERE id IN (SELECT ... FOR UPDATE
SKIP LOCKED)` pushes the next attempt 60 s ahead in the same statement),
so two instances never take the same row, and a crashed instance's claim
expires on its own (`TestClaimDueClaimsEachRowOnce`). One reversal that
keeps failing does not hold back the others claimed with it.

**One successful reversal per original.** The partial unique index
`idx_transactions_single_successful_reversal` covers
`(provider_id, reference_id)` where `status = 'PROCESSED' AND kind IN
('REFUND', 'ROLLBACK')`. Before inserting, the engine's function locks the
original with `SELECT ... FOR UPDATE` and checks for an existing reversal;
the index is the database's guarantee if that check is ever bypassed. The
rules for which originals a reversal may undo live in the domain
(`wager.DecideReversal`, `wager.ReversalDirection`) and, for PostgreSQL,
in `wager_resolve_reference`; the contract suite holds both to the same
answers.
Because the index spans both kinds, a bet accepts a single successful
`REFUND` or `ROLLBACK`; undoing the `REFUND` itself is still possible with
a `ROLLBACK` that references the `REFUND`. A `ROLLBACK` that would debit
more than the balance is rejected with `ROLLBACK_INSUFFICIENT_BALANCE`,
distinct from a bet's `INSUFFICIENT_BALANCE`. See
[ADR 0006](docs/adr/0006-pending-reference-durable-retry.md).

## 9. Inbox and outbox

**Outbox** (`outbox_events`). Every event is written in the same SQL
transaction as the change that caused it (section 3): no commit, no event;
commit, event. Nothing is published before the commit.

The relay (`internal/app/relay`) runs every `OUTBOX_PUBLISH_INTERVAL`
(2 s) in every instance. Each round (`PublishOnce`) asks the store
(`postgres.OutboxStore`) to claim up to `OUTBOX_BATCH_SIZE` (20) due events
with `SELECT ... FOR UPDATE SKIP LOCKED` in one transaction, hands each to
the sink (`sqsx.Sink`, the events queue), marks `published_at` and
commits; concurrent relays skip each other's rows
(`TestConcurrentPublishersNeverSendAnEventTwice`). A failed send backs that
event off exponentially (base 2 s, cap 2 minutes) without aborting the
batch, and counts as failed. The row ID is the envelope's `eventId`, so a
resend keeps it. If a process dies after the business commit, the event is
published by the next round of any instance; if it dies between sending
and marking, the same event is sent again: delivery is at least once. As
shipped this publishes at most about 10 events per second per instance,
and a money movement produces two; the
[benchmarks](docs/benchmarks/README.md) measure the consequence, and
phase 3 replaces the relay. See
[ADR 0005](docs/adr/0005-transactional-outbox-with-polling.md).

**Inbox** (`inbox_messages`). Unique on `(consumer_name, message_id)`,
with the payload hash, reception and completion time. A redelivery with
the same `messageId` and different content is refused
(`ErrInboxPayloadMismatch`). The inbox write and the business operation
are separate statements (record, `Submit`, complete); the guarantee
against a double movement is the idempotency index (section 6), and a
crash between `Submit` and completion just replays on redelivery.

## 10. SQS consumer

Queues created by `deploy/localstack/init-queues.sh`:
`wager-transactions.fifo` with `wager-transactions-dlq.fifo`, and
`wager-events.fifo` with `wager-events-dlq.fifo`; FIFO,
`ContentBasedDeduplication=false`, `VisibilityTimeout=30`, redrive after
`maxReceiveCount=5`. Producers use the `walletId` as `MessageGroupId`
(ordered per wallet, parallel across wallets) and, preferably, the
idempotency key as `MessageDeduplicationId` (SQS deduplication only spans
5 minutes; the database is the guarantee).

Per message (long polling of 10 s, up to 10 per call), inside a
`sqs.process_message` span:

1. parse the envelope; its `messageId` is the durable identity, not SQS's
   own `MessageId` (a producer that resends gets a new one);
2. record it in the inbox with the payload hash; an already completed
   message is just deleted (`duplicate`);
3. call the same `wagering.Submit` as HTTP;
4. mark the inbox entry completed;
5. only then delete the message.

| Result | What happens | `outcome` |
| --- | --- | --- |
| `PROCESSED`, `REJECTED`, `PENDING_REFERENCE` or replay | deleted; business rejection is final | `processed` |
| Already completed in the inbox | deleted | `duplicate` |
| Malformed JSON, no `messageId`, invalid money, malformed IDs, fields over 255 characters, other validation errors | not deleted; after 5 receptions, DLQ | `invalid` |
| Unknown wallet, idempotency conflict, transient failure | not deleted; redelivered after 30 s, eventually DLQ | `error` |

On shutdown the consumer's context is cancelled: no new receive starts, a
message in flight has its statement cancelled, is not deleted and is
redelivered after the visibility timeout. `TestConsumerRecordsOutcomes`
and `TestRelaySendsDueEventsToTheQueue` run both workers against
LocalStack.

## 11. Outgoing events

Destination `wager-events.fifo`; `MessageGroupId` is the event's
`aggregateId`, `MessageDeduplicationId` its `eventId`. Envelope, version 1:

```json
{
  "eventId": "uuid",
  "eventType": "WalletBalanceChanged",
  "aggregateId": "uuid",
  "correlationId": "string",
  "causationId": "string (optional)",
  "occurredAt": "2026-10-15T12:00:00Z",
  "version": 1,
  "data": {}
}
```

| Event | When | `aggregateId` | `data` |
| --- | --- | --- | --- |
| `WagerTransactionProcessed` | operation done, including `LOSS` and `OPENING` | transaction ID | `transactionId`, `walletId`, `kind`, `money` |
| `WalletBalanceChanged` | the balance actually changed | wallet ID | `walletId`, `transactionId`, `direction`, `money`, `balanceBefore`, `balanceAfter`, `walletVersion` |
| `WagerTransactionRejected` | final rejection | transaction ID | `transactionId`, `failureCode` |
| `WagerTransactionPendingReference` | reversal waiting for its reference | transaction ID | `transactionId`, `referenceExternalTransactionId` |

Timestamps are UTC RFC 3339 with microseconds, amounts decimal strings,
and the stored payload is an immutable snapshot. The engine builds the
envelope in SQL (`wager_emit`); the contract suite decodes every event of
every outcome into the Go types of `internal/app/events` and compares
them field by field with the reference model's. `correlationId` is the `messageId` for
SQS operations and the transaction ID for HTTP ones. Consumers must
deduplicate by `eventId`.

## 12. HTTP contract

Errors always have the body `{"code": "CODE", "error": "message"}`.

| Endpoint | Who may call |
| --- | --- |
| `POST /wallets` | `internal` role |
| `GET /wallets/{id}` | `internal` role |
| `GET /wallets/{id}/ledger?cursor=...&limit=50` | `internal` role |
| `POST /wallets/{id}/reconciliation` | `internal` role |
| `POST /wagering/transactions` | a provider |
| `GET /wagering/transactions/{id}` | the provider that owns the transaction |
| `GET /providers/{providerId}/wagering/transactions/{externalId}` | that provider |
| `GET /health/live`, `GET /health/ready` | public |

`/metrics` and pprof are not on this port (section 17).

`POST /wagering/transactions`:

| Situation | HTTP | Body |
| --- | --- | --- |
| Processed (replays included) | 200 | `transactionId`, `status: PROCESSED`, `balance`, `idempotentReplay` |
| Waiting for its reference | 202 | `status: PENDING_REFERENCE` |
| Business rejection | 422 | `status: REJECTED`, `failureCode` |
| Invalid input | 400 | `INVALID_BODY`, `MISSING_IDEMPOTENCY_KEY`, `INVALID_MONEY`, `INVALID_ID`, `VALIDATION_ERROR`, `OPENING_NOT_ALLOWED` |
| Body over 64 KiB | 413 | `BODY_TOO_LARGE` |
| Missing, invalid or expired token | 401 | `UNAUTHENTICATED` |
| Token without permission, body provider differs from the token | 403 | `FORBIDDEN`, `PROVIDER_MISMATCH` |
| Wallet not found (or another player's) | 404 | `WALLET_NOT_FOUND` |
| Idempotency conflict | 409 | `IDEMPOTENCY_CONFLICT`, `IDEMPOTENCY_KEY_MISMATCH` |
| Out of time (request deadline, statement or lock timeout) | 503 | `TIMEOUT`, with `Retry-After` |
| Unexpected failure | 500 | `INTERNAL_ERROR` |

Wallet endpoints also use `INVALID_PLAYER_ID`, `INVALID_LIMIT`,
`INVALID_CURSOR` (400) and `WALLET_CONFLICT` (409), and answer 201 on
opening. Identifiers in paths, bodies and the ledger cursor must be
canonical UUIDs (`INVALID_ID` otherwise); `externalTransactionId`,
`referenceExternalTransactionId`, `roundId`, `gameId` and the
`Idempotency-Key` are limited to 255 characters (`VALIDATION_ERROR`).
A submission is validated completely before anything is looked up, so a
400 depends only on the request. 400, 409 and 413 store nothing and can
be corrected and resent. A 503
`TIMEOUT` means the request ran out of time: almost always its transaction
rolled back, but a deadline that expires while the commit is in flight
can leave the operation committed. Either way resending it with the same
`Idempotency-Key` is safe: it is applied once or replayed. A 500 means
the outcome is unknown; resending with the same key is safe too. A 422 is
final for that operation: the replay returns the same rejection. See
[ADR 0012](docs/adr/0012-time-budgets-and-input-limits.md).

| `failureCode` | Meaning |
| --- | --- |
| `INSUFFICIENT_BALANCE` | `BET` without balance |
| `ROLLBACK_INSUFFICIENT_BALANCE` | a `ROLLBACK` that would debit more than the balance |
| `CURRENCY_MISMATCH` | the operation's currency differs from the wallet's |
| `REFERENCE_NOT_PROCESSED` | the reference ended without success |
| `REFERENCE_MISMATCH` | the reference differs in provider, player, wallet, round, currency or amount |
| `REFERENCE_KIND_NOT_REVERSIBLE` | the reference's kind cannot be reversed by this operation |
| `REFERENCE_ALREADY_REVERSED` | the reference already has a successful reversal |
| `REFERENCE_NOT_FOUND_TIMEOUT` | the reference never arrived after 8 attempts |

## 13. Reconciliation

`POST /wallets/{id}/reconciliation` (internal role) rebuilds the balance
from the ledger, opening credit included, and compares it with the stored
balance. Stored balance, delta sum and entry count come from one SQL
statement (`queries.Reader.Reconciliation`), and PostgreSQL gives each statement
one snapshot, so both sides see the same committed data even while the
wallet moves. A divergence shows in the response (`consistent: false`), in
an ERROR log and in `wallet_reconciliation_mismatches_total`.
Reconciliation never writes. `cmd/verify` (section 19) checks the same
invariant for every wallet at once, plus seven others.

## 14. Authentication and authorization

The IdP is Keycloak. The realm `wagering` is imported at startup
(`deploy/keycloak/realm-export.json`) with `client_credentials` clients:
`provider-a` and `provider-b` (a hard-coded `provider_id` claim),
`internal-service` (the `internal` realm role), and two benchmark clients
with two-hour tokens, `bench-provider` and `bench-internal`.

`httpapi.Authenticator` validates JWTs against Keycloak's JWKS
(`MicahParks/keyfunc/v3` with `golang-jwt/jwt/v5`): signature, algorithm,
issuer (`JWT_ISSUER`), audience (`JWT_AUDIENCE`) and a mandatory expiry.
Only `RS256` is accepted, which blocks algorithm confusion (an HS256 token
signed with the public key as secret). Every realm client adds the
`wallet-api` audience, and Keycloak runs with a fixed hostname so a token
carries the same issuer whether it was requested from the host or from
inside the Compose network.

- **Startup.** The first JWKS fetch must succeed; the API retries for up
  to 2 minutes while Keycloak starts and fails the boot otherwise, instead
  of starting without keys.
- **Refresh.** The JWKS is re-read every `JWKS_REFRESH_INTERVAL` (1 hour)
  for as long as the app runs, so keys revoked at the IdP stop being
  trusted without a restart.
- **Unknown key.** A token with an unknown `kid` triggers an immediate
  re-read, which is what accepts a freshly rotated key; it happens at most
  once every 5 minutes and costs the request at most 2 seconds, even with
  the IdP slow or down.
- **Verified-token cache.** A verified token is kept (by its SHA-256)
  until the earlier of its expiry and one refresh interval, and served
  only while its signing key is still in the JWKS, so a revoked key stops
  being trusted at the next refresh as before. A cached request costs
  about 10 µs in the middleware instead of 78 µs; the cache holds at most
  10,000 entries and never a rejected token
  ([ADR 0017](docs/adr/0017-cache-verified-tokens.md)).

Authorization: wallet endpoints require the `internal` role; wagering
endpoints require a `provider_id` claim, and the authorized provider is
always the token's (a different `providerId` in the body is 403). Lookups
by ID or external ID only return the caller's transactions; another
provider's transaction answers 404 so its existence is not confirmed.
Rejected requests never reach the database.

On SQS, access is up to broker credentials and policies; the domain checks
still apply (currency, amounts, no `OPENING`, wallet ownership). See the
provider identity gap in section 22.

## 15. Composition, lifecycle and shutdown (`cmd/api`)

Fx modules: `config` (configuration and metrics), `persistence` (pool,
engine, reader, outbox store, inbox, reference queue, readiness),
`application` (the wagering service, the retry worker, the relay),
`messaging` (SQS client, consumer, sink, readiness) and `http`
(authenticator and handler). One `appInvokes` list, shared by `main` and
its composition test, registers everything with a lifecycle: tracing, the
database metrics, the public HTTP server, the admin server and the three
workers.

`config.Load` fails the boot on a missing or invalid variable. The pool is
opened and pinged before anything starts; the authenticator waits for the
JWKS (section 14). Servers and workers are `platform/lifecycle`
components, free of Fx: a server listens on start, so a taken port fails
the boot, and drains in-flight requests on stop; a worker gets a
cancellable context on start, and on stop the context is cancelled and the
stop waits for the worker within the shutdown deadline, failing if it does
not exit. `cmd/api` only hooks them into Fx. On `SIGTERM`, Fx stops them
in reverse order: the HTTP and admin servers stop accepting and drain,
the workers stop, traces are flushed and the pool is closed last.
`TestFxAppStartsAndStopsCleanly` builds the whole composition against
PostgreSQL and LocalStack containers and an in-process JWKS and checks
that start and stop complete with every worker exiting.

The dependency rule ([ADR 0013](docs/adr/0013-hexagonal-layout-and-engine-port.md)),
checked by `internal/archtest`: the domain imports only the domain and
`google/uuid`; the app layer imports no adapter, platform, driver,
transport or Fx; no adapter imports another; only `cmd/` imports Fx.

## 16. Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `DATABASE_URL` | required | PostgreSQL connection |
| `JWKS_URL` | required | Keycloak's public keys |
| `JWT_ISSUER`, `JWT_AUDIENCE` | required | tokens must come from this issuer and be meant for this audience |
| `SQS_TRANSACTIONS_QUEUE_URL`, `SQS_EVENTS_QUEUE_URL` | required | inbound and outbound queues |
| `SQS_ENDPOINT`, `AWS_REGION`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` | LocalStack defaults | SQS access |
| `HTTP_PORT` | `8080` | public API |
| `ADMIN_ADDR` | `127.0.0.1:6060` | metrics and pprof, loopback by default |
| `PPROF_ENABLED` | `false` | serve the Go profiler on the admin port |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `DB_MAX_OPEN_CONNS`, `DB_MIN_CONNS` | `10`, `2` | connection pool: most and fewest open connections |
| `DB_STATEMENT_TIMEOUT`, `DB_IDLE_IN_TRANSACTION_TIMEOUT`, `DB_LOCK_TIMEOUT` | `5s`, `10s`, `2s` | PostgreSQL session limits on every pooled connection |
| `REQUEST_TIMEOUT` | `5s` | deadline of every authenticated request; past it the API answers 503 `TIMEOUT` |
| `OUTBOX_PUBLISH_INTERVAL`, `OUTBOX_BATCH_SIZE` | `2s`, `20` | outbox relay |
| `REFERENCE_RETRY_INTERVAL` | `15s` | pending-reference worker |
| `JWKS_REFRESH_INTERVAL` | `1h` | JWKS re-read; also the longest a verified token stays cached |
| `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_SERVICE_NAME`, `OTEL_TRACES_SAMPLER[_ARG]` | tracing off | OpenTelemetry |

Durations accept whole seconds (`2`) or Go durations (`500ms`). An
invalid value fails the boot naming the variable.

## 17. Observability

**Metrics** (Prometheus, on `ADMIN_ADDR`/metrics, OpenMetrics format so
exemplars travel):

| Metric | Measures |
| --- | --- |
| `http_requests_total{path,method,status}` | requests; the method is normalized to stop label explosion |
| `http_request_duration_seconds{route,method,status_class}` | request latency, with trace exemplars |
| `wager_submit_duration_seconds{transport,kind,status}` | `Submit` latency by transport; unknown kinds become `OTHER` |
| `wager_transactions_total{status}` | outcomes by status |
| `wager_idempotent_replays_total`, `wager_concurrency_conflicts_total` | replays and idempotency races |
| `wager_reference_retries_total` | pending-reference retries |
| `sqs_message_processing_duration_seconds{outcome}` | SQS handling by outcome |
| `outbox_events_published_total`, `outbox_events_publish_failed_total` | publishing |
| `outbox_publish_batch_duration_seconds` | one publishing round |
| `outbox_pending_events`, `outbox_oldest_pending_age_seconds` | backlog, queried per scrape (omitted, never failing, if the database is slow) |
| `wallet_reconciliation_mismatches_total` | reconciliation divergences |
| `auth_token_cache_total{result}` | bearer tokens served from the verified-token cache (`hit`) or verified (`miss`) |
| `wallet_db_pool_*` | the pool: acquires, acquires that waited and their total wait, cancelled acquires, connections by state, maximum, connections opened |
| `go_*`, `process_*`, `wallet_build_info` | runtime, process, build version |

**Traces** (OpenTelemetry over OTLP): HTTP server spans, one span per SQL
statement under its request (one per command since phase 2b), and spans
for each SQS message, outbox round and retry round. Off unless `OTEL_EXPORTER_OTLP_ENDPOINT` is set.

**Logs** are JSON (`log/slog`) and carry `trace_id`/`span_id` when logged
inside a span. Wager operations log `transport`, `correlationId`,
`messageId` (SQS), `transactionId`, `walletId`, `providerId`, `status`,
`failureCode` and `idempotentReplay`; never tokens, secrets, amounts or
full payloads.

**Health**: `/health/live` answers when the process is up;
`/health/ready` checks PostgreSQL and SQS and answers 503 if either fails,
saying only `ok` or `unavailable` per dependency (the error goes to the
log).

**Stack**: `make up-obs` adds Prometheus, Grafana (dashboard "Wallet /
Overview", exemplars linking to traces), Jaeger and the PostgreSQL
exporter, and turns tracing on. See
[ADR 0010](docs/adr/0010-observability-stack.md).

## 18. Testing

`go test ./...` needs Docker and nothing else
([ADR 0011](docs/adr/0011-self-contained-integration-tests.md)):
`internal/testsupport/pgtest` provides a migrated PostgreSQL (container or
`DATABASE_URL`) and an isolated database per test (`FreshURL`); `sqstest`
runs LocalStack; `jwttest` is an in-process IdP with key rotation and
malicious tokens; `hostport` pins container host ports to ports free in
the test's namespace (Docker Desktop on WSL can hand out ports a local
process already holds). Integration tests run or fail; `-short` runs unit
tests only.

Four kinds of checks hold the engine to its rules
([ADR 0013](docs/adr/0013-hexagonal-layout-and-engine-port.md)): the
engine contract (`enginetest.Run`, 37 cases, against the reference model
and PostgreSQL), a property test (`TestEngineMatchesTheModel`, rapid)
that drives PostgreSQL and the model with the same random command
sequences, a linearizability check (`TestEngineIsLinearizable`,
porcupine) and fuzz targets for HTTP and SQS decoding. The app layer's
tests run against the model and need no Docker.

| Scenario | Test |
| --- | --- |
| Every command's outcome, events and ledger | `TestEngineHonoursTheEngineContract`, `TestModelHonoursTheEngineContract` |
| PostgreSQL answers like the model on random sequences | `TestEngineMatchesTheModel` |
| Concurrent histories on one wallet are linearizable | `TestEngineIsLinearizable` |
| Concurrency across three instances | `TestConcurrentBetsAcrossThreeIndependentPools`, `TestDuplicatesAcrossThreeIndependentPoolsMoveMoneyOnce`, `TestDistinctWalletsProcessConcurrently` |
| No deadlock between a fresh reversal and the retry worker | `TestFreshRollbackRacesTheRetryWorkerWithoutDeadlock` |
| One statement per command, no sequential scan, HOT updates | `TestEveryCommandIsOneStatement`, `TestSubmitNeverScansTransactions`, `TestBalanceUpdatesAreHOT` |
| Each lookup on its own index, helpers inlined | `TestLookupsUseTheirOwnIndexEvenWithStaleStatistics`, `TestEngineHelpersAreInlinedOrCached` |
| A request out of time cancels its statement and keeps the connection | `TestDeadlineCancelsTheStatementAndKeepsTheConnection`, `TestSubmitTimesOutWith503WhenTheWalletIsLocked` |
| Retry worker across instances | `TestClaimDueClaimsEachRowOnce` |
| Reconciliation | `TestReconciliationSnapshot` |
| The HTTP contract end to end | `TestE2E*` in `internal/adapters/httpapi` |
| JWKS revocation, startup, hung IdP, token cache | `TestAuthenticatorStopsTrustingRevokedKeys`, `TestNewAuthenticator*`, `TestUnknownKeyIsRejectedQuicklyWhileTheIdPHangs`, `TestCachedToken*` |
| SQS consumer outcomes and the relay | `TestConsumerRecordsOutcomes`, `TestRelaySendsDueEventsToTheQueue` |
| Decoding never panics and never lets an invalid command through | `FuzzDecodeSubmitRequest`, `FuzzDecodeMessage` |
| The dependency rule | `TestDependencyRule` |
| Invariant checks detect corruption | `TestVerifyDetectsEachViolation` |
| Fx start and stop | `TestFxAppStartsAndStopsCleanly` |

## 19. Benchmark harness

- `cmd/seed` (`make seed`) writes wallets, their `OPENING` transactions
  and ledger credits with `COPY` in one transaction, without outbox
  events, and the CSV the scenarios read.
- `cmd/verify` (`make verify`) checks eight invariants: non-negative
  balances, balance equals the ledger sum, one entry per processed
  movement, entries only for processed transactions, none for `LOSS`, one
  successful reversal per original, no persisted `PENDING`, an outbox
  event for every terminal transaction. Exit 0, 1 on violation, 2 if it
  cannot run.
- `bench/k6/` holds five open-model scenarios; `bench/run.sh` (`make
  bench`) runs one on a fresh, pinned, seeded stack and collects the
  summary, profiles, container stats, a Prometheus snapshot, PostgreSQL's
  table and index counters and log, and the verification, and exits 1
  when k6 or the verification failed. `bench/summarize.py` turns a run
  into the report tables, PostgreSQL CPU and commits per bet served
  and the wallet HOT ratio included. Method in
  [ADR 0009](docs/adr/0009-benchmark-methodology.md), results in
  [`docs/benchmarks`](docs/benchmarks/README.md).

## 20. `playerId` and `walletId` consistency

The engine checks that the wallet belongs to the given `playerId` before
any movement; otherwise a provider could move another
player's wallet by sending a `walletId` it learned. A mismatch answers
`WALLET_NOT_FOUND` (404) without telling "missing" from "someone else's".
There is no provider-player link (no `provider_players` table): the
challenge never defined that relationship.

## 21. Interpretations

- Synchronous processing: no operation commits an intermediate `PENDING`.
- Infrastructure failures are not stored as `FAILED`; they are transient
  and retried by the HTTP client or by SQS redelivery.
- Leading zeros in amounts are accepted and normalized before hashing; no
  other format variation is.
- `WIN` accepts `referenceExternalTransactionId` as information but does
  not resolve it; only `REFUND` and `ROLLBACK` must.
- One successful reversal per original, counting `REFUND` and `ROLLBACK`
  together.
- Ledger uniqueness by `transaction_id`, stricter than
  `(walletId, transactionId)`.
- Over HTTP, without an inbound correlation ID, the `correlationId` is the
  transaction ID.
- Access to someone else's resource answers 404 on lookups by ID.

## 22. Limitations and known gaps

- **No admission control.** A saturated service queues requests up to
  their deadline instead of rejecting the excess at once
  ([benchmark 004](docs/benchmarks/004-single-statement-hot-path.md));
  phase 2c adds it.
- **A hot wallet still serializes** on its row lock, now held for one
  statement instead of a multi-statement exchange: one wallet takes about
  450 to 470 bets a second (benchmark 004). Past that, latency grows to
  the request budget, and waits that outlast the lock timeout end as 503s.
  Phase 6's single writer per partition is the way past it.
- **An outbox that publishes about 10 events per second per instance**,
  one at a time, while a money movement produces two. Under load the
  backlog grows without bound; since migration 15 the relay keeps
  publishing at that rate however large the backlog, where it used to
  stall. Phase 3 replaces it.
- **PostgreSQL CPU per bet** is about halved by the one-statement
  path (1.5 to 2.1 ms against 3.9) but still above phase 2's target of
  1 ms, and the knee, 400 rps within a 50 ms p99, is short of phase 2's
  1,000. Phase 2c tunes PostgreSQL, the pool and the indexes benchmark
  004 reports unused.
- **Short stalls** of a few hundred milliseconds were seen in benchmarks
  002 to 004. Benchmark 004's PostgreSQL logs tie them to checkpoints,
  triggered by WAL volume from about 700 rps, but not a one-minute stall
  on a single wallet at 350 rps; phase 2c tunes the checkpoints and logs
  autovacuum to explain it.
- **Inbox and business operation in separate statements** (section 9).
  The financial guarantee rests on the idempotency index.
- **SQS retries on a fixed visibility timeout** and do not tell transient
  from permanent errors: both use the same 5 attempts before the DLQ.
- **No automated tests** for a consumer killed between commit and delete,
  or a full application restart. The mechanisms exist (delete after
  completion, all state in the database) and were checked by hand.
- **No DLQ counter** and **no attempt limit on the outbox**: an event that
  can never be published keeps retrying every 2 minutes.
- **Provider identity is not verified on SQS.** Over HTTP the JWT proves
  the provider; on the queue the `providerId` comes from the message body,
  so anyone who can publish to the shared queue can claim any provider.
  Per-provider queues or IAM policies would close it; LocalStack with
  shared test credentials cannot model it.
- **`/health/ready` is public** and pings PostgreSQL and SQS on every
  call; phase 5's probes split liveness, readiness and dependency checks.
- **One API instance in Compose.** Coordination between instances is
  shown by tests with independent pools (section 5) rather than by several
  containers; phase 5 runs several replicas on Kubernetes.
- **Index changes take write locks.** Migrations 14 and 15 build indexes
  without `CONCURRENTLY`; on a live database they would block writes while
  they run. Phase 5 brings expand-and-contract migrations.
