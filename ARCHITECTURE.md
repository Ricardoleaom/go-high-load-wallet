# Architecture

How the service works as of v0.2.0, and why. The reasoning behind the
larger choices lives in the [decision records](docs/adr/README.md); where
the service is heading lives in the [roadmap](ROADMAP.md).

## 1. Overview

One Go binary (`cmd/api`) runs the HTTP API and three background workers:
the SQS consumer, the outbox publisher and the pending-reference retry
worker. Every instance runs all four; they coordinate only through
PostgreSQL, so any number of instances can run side by side.

```
providers ──HTTP──► API ─────────────┐
providers ──SQS───► consumer ────────┤──► PostgreSQL (wallets, transactions, ledger, inbox, outbox)
                    outbox publisher ◄┘──► SQS events queue
                    retry worker (PENDING_REFERENCE)
Keycloak ──JWKS──► API          admin port ──► Prometheus ─► Grafana ◄─ Jaeger (OTLP traces)
```

| Package | Role |
| --- | --- |
| `internal/domain/{money,wallet,wager,ledger}` | Pure domain types and rules; no I/O, no frameworks |
| `internal/application/{wallet,wager}` | Use cases; they decide where SQL transactions begin and end |
| `internal/infrastructure/postgres` | Repositories and the atomic wallet movements |
| `internal/infrastructure/sqsx` | SQS client, consumer and outbox publisher |
| `internal/httpapi` | HTTP handlers, authentication and request metrics |
| `internal/observability` | Metrics, tracing, log correlation and the admin handler |
| `internal/config` | Environment configuration, validated at boot |
| `internal/bench/{seed,verify}` | Benchmark seeding and invariant checks (`cmd/seed`, `cmd/verify`) |
| `internal/testsupport/*` | Containers and fakes for self-contained tests |
| `cmd/api` | Fx composition and lifecycle |

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

## 3. Database access and SQL transactions

`database/sql` over the `jackc/pgx/v5/stdlib` driver, hand-written SQL, no
ORM; versioned `golang-migrate` migrations in `migrations/` (each with
`up` and `down`), embedded in the `migrations` package so tests apply
exactly the same files. `postgres.Open` keeps the shipped pool (10 open,
5 idle) unless configured, and can trace every statement.

The use case (`internal/application/...`) decides where a transaction
begins and ends, not the repository: write methods take the caller's
`*sql.Tx` (and refuse `nil`), so one transaction spans wallet,
transaction, ledger and outbox.

| Operation | Committed together |
| --- | --- |
| Opening with balance > 0 | wallet, `OPENING` as `PROCESSED`, ledger credit, `WagerTransactionProcessed` and `WalletBalanceChanged` |
| Opening with zero balance | the wallet only |
| `BET`/`WIN` processed | `PROCESSED` transaction, conditional wallet `UPDATE`, ledger entry, `WagerTransactionProcessed` and `WalletBalanceChanged` |
| `BET` without balance or with another currency | the attempt rolls back; a new commit stores the `REJECTED` transaction and `WagerTransactionRejected` |
| `LOSS` | `PROCESSED` transaction and `WagerTransactionProcessed` (wallet and ledger untouched) |
| `REFUND`/`ROLLBACK` without its reference | `PENDING_REFERENCE` transaction, the retry schedule and `WagerTransactionPendingReference` |
| `REFUND`/`ROLLBACK` applied | lock on the original transaction, `PROCESSED` reversal, wallet, ledger entry and both events |

A traced `BET` shows 11 SQL statements under its HTTP span: the
idempotency and wallet lookups, `BEGIN`, the transaction insert, the
wallet update, the ledger insert, the status update, two outbox inserts and
`COMMIT`. Collapsing them is phase 2 of the [roadmap](ROADMAP.md).

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

**`PENDING` is never committed on its own.** Processing is synchronous:
the transaction is born `PENDING` in memory and stored already in its final
state (or in `PENDING_REFERENCE`) in the same commit as the operation, so
there is no orphan `PENDING` for another instance to pick up. The only
durable wait is `PENDING_REFERENCE`, which has its own worker (section 8).

**Transient versus permanent failures.** Infrastructure failures
(PostgreSQL or SQS down, timeouts, unexpected errors) leave no record: the
transaction rolls back, nothing commits, and the error reaches the caller.
HTTP clients get a 500 and can safely resend with the same
`Idempotency-Key`; SQS messages are not deleted and come back (section
10). Business rejections are final and stored as `REJECTED` with a
`failureCode` (section 12). `FAILED` exists in the domain but the service
does not store it; messages that exhaust their SQS attempts end in the DLQ.

A transaction keeps its reference in two columns: `reference_external_id`
(the `referenceExternalTransactionId` as sent) and `reference_id` (the
internal ID of the original, filled once resolved).

`OPENING` is internal. The HTTP handler rejects `kind: "OPENING"` with 400
and `Submit` returns `ErrOpeningNotAllowed` as a second barrier, which
also covers SQS. `OPENING` carries no provider, external IDs, key, hash,
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

Every movement is one conditional `UPDATE`
(`internal/infrastructure/postgres/wallet_writer.go`):

```sql
UPDATE wallets
SET balance_minor_units = balance_minor_units - $1, version = version + 1, ...
WHERE id = $2 AND currency = $3 AND balance_minor_units >= $1 AND version < ...
RETURNING balance_minor_units, version
```

PostgreSQL locks the row during the `UPDATE` and re-evaluates the `WHERE`
against the latest committed values, so two concurrent bets never read
the same old balance: there is no window between reading and writing,
hence no lost update. Each wallet is one row and each `UPDATE` locks only
its own, so different wallets proceed in parallel without a global lock or
an application lock, with one instance or many. When the `UPDATE` affects
no row, a diagnostic read tells an unknown wallet, a currency mismatch and
an insufficient balance apart to return the right `failureCode`. See
[ADR 0003](docs/adr/0003-atomic-conditional-wallet-update.md).

Tests against a real PostgreSQL, with the race detector, where each
`Service` has its own `*sql.DB` to act as an independent process
(`internal/application/wager/service_test.go`):

- `TestConcurrentBetsOnSameWallet`: 100.00 and two concurrent 80.00 bets
  from two instances: one `PROCESSED`, one `REJECTED` with
  `INSUFFICIENT_BALANCE`, final balance 20.00, a single ledger debit;
- `TestConcurrentBetsAcrossThreeIndependentInstances`: three 40.00 bets on
  100.00 across three instances; exactly two process;
- `TestFiftyConcurrentDuplicateSubmissions`: the same bet 50 times across
  three instances; one debit;
- `TestDistinctWalletsProcessConcurrently`: ten wallets moved at once by
  three instances, all processed.

## 6. Idempotency

An operation's identity is `(provider_id, idempotency_key)`, with a partial
unique index (`idx_transactions_idempotency_unique`, excluding `OPENING`).
`(provider_id, external_id)` has another, so the same financial operation
cannot be applied again under a different key. Both live in
`transactions`, so idempotency survives restarts of every process.

**Payload hash** (`ComputePayloadHash`,
`internal/application/wager/payload_hash.go`): SHA-256, hex encoded, of
the JSON of a struct with fields in a fixed alphabetical order — `amount`,
`currency`, `externalId`, `gameId`, `kind`, `playerId`, `providerId`,
`referenceExternalTransactionId` (omitted when empty), `roundId`,
`walletId`. The idempotency key and any transport metadata (HTTP headers,
SQS envelope, `messageId`) stay out. HTTP and SQS build the same
`SubmitInput`, so the same operation hashes the same through either.

`Service.Submit`:

1. look up `(providerId, idempotencyKey)`: found with the same hash →
   return the stored result with `idempotentReplay: true`; different hash
   → `IDEMPOTENCY_CONFLICT` (409);
2. look up `(providerId, externalTransactionId)`: found under another key
   → `IDEMPOTENCY_KEY_MISMATCH` (409);
3. otherwise process it as a new operation.

A replay of a `PROCESSED` transaction returns the balance recorded in its
ledger entry, not the wallet's current balance. Two concurrent first
submissions of the same key can both pass the lookups; the unique index
lets one insert win, the loser gets the unique violation (translated to
`errIdempotencyRace`) and replays the winner. The server never replaces
the `Idempotency-Key` it receives; the header is mandatory (400 without
it). See [ADR 0004](docs/adr/0004-idempotency-unique-index-and-payload-hash.md).

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
`created_at|id`), `limit` 50 by default and 500 at most. Because
`created_at` does not follow lock order, the ledger cannot be checked as a
chain of `balance_before → balance_after` yet; phase 4 keys it by
`(wallet_id, version)`.

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
exponential backoff (5 s, 10 s, 20 s, 40 s… capped at 5 minutes). The
`RetryWorker` runs every `REFERENCE_RETRY_INTERVAL` (15 s), takes up to 20
due rows and tries again; after 8 attempts (about 15 minutes in total) it
rejects with `REFERENCE_NOT_FOUND_TIMEOUT` and publishes
`WagerTransactionRejected`. Rows are claimed atomically
(`ListDuePendingReferences`: `UPDATE ... WHERE id IN (SELECT ... FOR
UPDATE SKIP LOCKED)` pushes the next attempt 60 s ahead in the same
statement), so two instances never take the same row, and a crashed
instance's claim expires on its own. Covered by
`TestListDuePendingReferencesClaimsEachRowOnce`.

**One successful reversal per original.** The partial unique index
`idx_transactions_single_successful_reversal` covers
`(provider_id, reference_id)` where `status = 'PROCESSED' AND kind IN
('REFUND', 'ROLLBACK')`. Before inserting, the application locks the
original with `SELECT ... FOR UPDATE` and checks for an existing reversal;
the index is the database's guarantee if that check is ever bypassed.
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

`OutboxPublisher` runs every `OUTBOX_PUBLISH_INTERVAL` (2 s) in every
instance. Each round (`PublishOnce`) opens a transaction, claims up to
`OUTBOX_BATCH_SIZE` (20) due events with `SELECT ... FOR UPDATE SKIP
LOCKED`, sends each to SQS, marks `published_at` and commits; concurrent
publishers skip each other's rows. A failed send backs that event off
exponentially (base 2 s, cap 2 minutes) without aborting the batch. The
row ID is the envelope's `eventId`, so a resend keeps it. If a process
dies after the business commit, the event is published by the next round
of any instance; if it dies between sending and marking, the same event is
sent again: delivery is at least once. As shipped this publishes at most
about 10 events per second per instance, and a bet produces two; the
[baselines](docs/benchmarks/README.md) measure the consequence. See
[ADR 0005](docs/adr/0005-transactional-outbox-with-polling.md).

**Inbox** (`inbox_messages`). Unique on `(consumer_name, message_id)`,
with the payload hash, reception and completion time. A redelivery with
the same `messageId` and different content is refused
(`ErrInboxPayloadMismatch`). The inbox write and the business operation
are separate commits (record, `Submit`, complete); the guarantee against a
double movement is the idempotency index (section 6), and a crash between
`Submit` and completion just replays on redelivery.

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
3. call the same `Service.Submit` as HTTP;
4. mark the inbox entry completed;
5. only then delete the message.

| Result | What happens | `outcome` |
| --- | --- | --- |
| `PROCESSED`, `REJECTED`, `PENDING_REFERENCE` or replay | deleted; business rejection is final | `processed` |
| Already completed in the inbox | deleted | `duplicate` |
| Malformed JSON, no `messageId`, invalid money | not deleted; after 5 receptions, DLQ | `invalid` |
| Validation error, unknown wallet, idempotency conflict, transient failure | not deleted; redelivered after 30 s, eventually DLQ | `error` |

On shutdown the consumer's context is cancelled: no new receive starts, a
message in flight has its SQL transaction aborted, is not deleted and is
redelivered after the visibility timeout. `TestConsumerRecordsOutcomes`
and `TestPublishOnceSendsDueEventsAndRecordsTheBatch` run both workers
against LocalStack.

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

Timestamps are UTC RFC 3339, amounts decimal strings, and the stored
payload is an immutable snapshot. `correlationId` is the `messageId` for
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
| Invalid input | 400 | `INVALID_BODY`, `MISSING_IDEMPOTENCY_KEY`, `INVALID_MONEY`, `VALIDATION_ERROR`, `OPENING_NOT_ALLOWED` |
| Missing, invalid or expired token | 401 | `UNAUTHENTICATED` |
| Token without permission, body provider differs from the token | 403 | `FORBIDDEN`, `PROVIDER_MISMATCH` |
| Wallet not found (or another player's) | 404 | `WALLET_NOT_FOUND` |
| Idempotency conflict | 409 | `IDEMPOTENCY_CONFLICT`, `IDEMPOTENCY_KEY_MISMATCH` |
| Transient unavailability | 500 | `INTERNAL_ERROR` |

Wallet endpoints also use `INVALID_PLAYER_ID`, `INVALID_LIMIT`,
`INVALID_CURSOR` (400) and `WALLET_CONFLICT` (409), and answer 201 on
opening. 400 and 409 store nothing and can be corrected and resent. A 500
means the operation did not commit or its outcome is unknown; resending
with the same `Idempotency-Key` is safe. A 422 is final for that
operation: the replay returns the same rejection.

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
statement (`ReconciliationSnapshot`), and PostgreSQL gives each statement
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
(`MicahParks/keyfunc/v3` with `golang-jwt/jwt/v5`): signature, expiry and
algorithm. Only `RS256` is accepted, which blocks algorithm confusion
(an HS256 token signed with the public key as secret).

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

Authorization: wallet endpoints require the `internal` role; wagering
endpoints require a `provider_id` claim, and the authorized provider is
always the token's (a different `providerId` in the body is 403). Lookups
by ID or external ID only return the caller's transactions; another
provider's transaction answers 404 so its existence is not confirmed.
Rejected requests never reach the database.

On SQS, access is up to broker credentials and policies; the domain checks
still apply (currency, amounts, no `OPENING`, wallet ownership). See the
provider identity gap in section 22.

## 15. Fx composition, lifecycle and shutdown (`cmd/api`)

Fx modules: `config` (configuration and metrics), `persistence` (pool and
repositories), `application` (use cases and the retry worker),
`messaging` (SQS client, consumer, publisher, readiness) and `http`
(authenticator and handler). One `appInvokes` list, shared by `main` and
its composition test, registers everything with a lifecycle: tracing, the
database metrics, the public HTTP server, the admin server and the three
workers.

`config.Load` fails the boot on a missing or invalid variable. The
database is opened and pinged before anything starts; the authenticator
waits for the JWKS (section 14). Workers use `runBackground`
(`cmd/api/lifecycle.go`): on `OnStart` they get a cancellable context and
a goroutine; on `OnStop` the context is cancelled and the hook waits for
the goroutine within the shutdown deadline, returning an error if it does
not exit. On `SIGTERM`, Fx runs `OnStop` in reverse order: the HTTP and
admin servers stop accepting and drain in-flight requests, the workers
stop, traces are flushed and the database is closed last.
`TestFxAppStartsAndStopsCleanly` builds the whole composition against
PostgreSQL and LocalStack containers and an in-process JWKS and checks
that start and stop complete with every worker exiting.

The domain does not import Fx, HTTP, SQS or the database library; only
`cmd/api` depends on Fx.

## 16. Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `DATABASE_URL` | required | PostgreSQL connection |
| `JWKS_URL` | required | Keycloak's public keys |
| `SQS_TRANSACTIONS_QUEUE_URL`, `SQS_EVENTS_QUEUE_URL` | required | inbound and outbound queues |
| `SQS_ENDPOINT`, `AWS_REGION`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` | LocalStack defaults | SQS access |
| `HTTP_PORT` | `8080` | public API |
| `ADMIN_ADDR` | `127.0.0.1:6060` | metrics and pprof, loopback by default |
| `PPROF_ENABLED` | `false` | serve the Go profiler on the admin port |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `DB_MAX_OPEN_CONNS`, `DB_MAX_IDLE_CONNS` | `10`, `5` | connection pool |
| `OUTBOX_PUBLISH_INTERVAL`, `OUTBOX_BATCH_SIZE` | `2s`, `20` | outbox publisher |
| `REFERENCE_RETRY_INTERVAL` | `15s` | pending-reference worker |
| `JWKS_REFRESH_INTERVAL` | `1h` | JWKS re-read |
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
| `go_sql_*{db_name="wallet"}` | connection pool |
| `go_*`, `process_*`, `wallet_build_info` | runtime, process, build version |

**Traces** (OpenTelemetry over OTLP): HTTP server spans, one span per SQL
statement under its request, and spans for each SQS message, outbox round
and retry round. Off unless `OTEL_EXPORTER_OTLP_ENDPOINT` is set.

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

| Scenario | Test |
| --- | --- |
| 100.00 and two concurrent 80.00 bets | `TestConcurrentBetsOnSameWallet` |
| The same bet 50 times in parallel | `TestFiftyConcurrentDuplicateSubmissions` |
| Distinct wallets at once, three instances | `TestDistinctWalletsProcessConcurrently`, `TestConcurrentBetsAcrossThreeIndependentInstances` |
| Reversal before its reference | `TestSubmitRefundBeforeReferenceArrivesGoesPendingThenResolves` |
| Retry worker across instances | `TestListDuePendingReferencesClaimsEachRowOnce` |
| Same key, different payload | `TestSubmitRejectsSameKeyDifferentPayload` |
| Reconciliation | `TestReconciliationSnapshot` |
| The HTTP contract end to end | `TestE2E*` in `internal/httpapi` |
| JWKS revocation, startup and hung IdP | `TestAuthenticatorStopsTrustingRevokedKeys`, `TestNewAuthenticator*`, `TestUnknownKeyIsRejectedQuicklyWhileTheIdPHangs` |
| SQS consumer outcomes and outbox publishing | `TestConsumerRecordsOutcomes`, `TestPublishOnceSendsDueEventsAndRecordsTheBatch` |
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
  summary, profiles, container stats, a Prometheus snapshot and the
  verification. Method in
  [ADR 0009](docs/adr/0009-benchmark-methodology.md), results in
  [`docs/benchmarks`](docs/benchmarks/README.md).

## 20. `playerId` and `walletId` consistency

`Service.Submit` loads the wallet and checks it belongs to the given
`playerId` before any movement; otherwise a provider could move another
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

- **About eleven SQL round trips per bet** and **an outbox that publishes
  about 10 events per second per instance** while a bet produces two. Both
  are measured in the [baselines](docs/benchmarks/README.md) and are the
  targets of phases 2 and 3.
- **Inbox and business operation in separate commits** (section 9). The
  financial guarantee rests on the idempotency index.
- **SQS retries on a fixed visibility timeout** and do not tell transient
  from permanent errors: both use the same 5 attempts before the DLQ.
- **No automated tests** for two publishers racing on the same events, a
  consumer killed between commit and delete, or a full application
  restart. The mechanisms exist (`SKIP LOCKED`, delete after completion,
  all state in the database) and were checked by hand.
- **No DLQ counter** and **no attempt limit on the outbox**: an event that
  can never be published keeps retrying every 2 minutes.
- **Provider identity is not verified on SQS.** Over HTTP the JWT proves
  the provider; on the queue the `providerId` comes from the message body,
  so anyone who can publish to the shared queue can claim any provider.
  Per-provider queues or IAM policies would close it; LocalStack with
  shared test credentials cannot model it.
- **One API instance in Compose.** Coordination between instances is
  shown by tests with independent `Service`s (section 5) rather than by
  several containers; phase 5 runs several replicas on Kubernetes.
- **The ledger cannot be checked as a chain** until it is keyed by
  `(wallet_id, version)` (phase 4).
